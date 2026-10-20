package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/phoenixperps/pperp/internal/release"
)

// pperp against a fake API and a fake sign-in server (design 15 "Verification"): the exact requests commands make,
// the answers printed in each format, exit codes, the questions asked before trades, and signing in.

var testKey = "ppk_test_" + strings.Repeat("A", 50) // the shape of a sandbox key (the SDKs' fixture), no real one

type seen struct {
	method, path, auth, key string
	query                   url.Values
	body                    map[string]any
}

type fakeAPI struct {
	mu     sync.Mutex
	reqs   []seen
	routes map[string]http.HandlerFunc // "GET /v1/me"
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&body)
	f.mu.Lock()
	f.reqs = append(f.reqs, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.URL.Query(), body})
	h := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req_test")
	if h == nil {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"code":"route_not_found","message":"No such route."}}`)
		return
	}
	h(w, r)
}

func (f *fakeAPI) calls(method, path string) []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []seen
	for _, s := range f.reqs {
		if s.method == method && s.path == path {
			out = append(out, s)
		}
	}
	return out
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const meBody = `{"data":{"credential":{"kind":"key","id":"k1","name":"Bot","environment":"sandbox","access":"trade","expires_at":null,"created_at":1791460800000},"accounts":["SANDBOX-00042-001"],"account_scope":"all","controls":{"markets":[],"reduce_only":false,"max_order_usd":null,"max_position_usd":null,"daily_loss_usd":null},"budgets":{"read_per_min":600,"trade_per_min":120},"assistant":null}}`

func newAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{routes: map[string]http.HandlerFunc{
		"GET /v1/me":       reply(200, meBody),
		"GET /v1/accounts": reply(200, `{"data":[{"id":"SANDBOX-00042-001","status":"active","stage":"sandbox","balance":"100000.00"}],"has_more":false,"next_cursor":null}`),
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

type harness struct {
	t       *testing.T
	env     map[string]string
	stdin   string
	tty     bool
	browser func(string) error
}

func newHarness(t *testing.T, api *httptest.Server) *harness {
	keyring.MockInit()
	return &harness{t: t, env: map[string]string{"PPERP_CONFIG_DIR": t.TempDir(), "PHOENIXPERPS_BASE_URL": api.URL}}
}

func (h *harness) signIn(profile, token string) {
	b, _ := json.Marshal(Credential{Token: token, Kind: "key"})
	if err := keyring.Set(keychainService, "profile:"+profile, string(b)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) config(text string) {
	if err := os.WriteFile(filepath.Join(h.env["PPERP_CONFIG_DIR"], "config.toml"), []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run(args ...string) (string, string, int) {
	var out, errb bytes.Buffer
	a := &App{Version: "test", In: strings.NewReader(h.stdin), Out: &out, Err: &errb, Getenv: func(k string) string { return h.env[k] },
		OutTTY: h.tty, InTTY: h.tty, Browser: h.browser}
	code := a.Run(context.Background(), args)
	return out.String(), errb.String(), code
}

func TestPKCEVector(t *testing.T) { // RFC 7636 appendix B
	if got := challengeOf("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal(got)
	}
	v, c := pkce()
	if len(v) != 43 || c != challengeOf(v) {
		t.Fatal(v, c)
	}
}

func TestGeneratedCommandRequest(t *testing.T) {
	api, srv := newAPI(t)
	api.routes["POST /v1/accounts/SANDBOX-00042-001/orders"] = reply(201, `{"data":{"id":"ord_1","status":"working","size":"0.10000"}}`)
	h := newHarness(t, srv)
	h.signIn("sandbox", testKey)
	out, errOut, code := h.run("place-order", "--market", "BTC-USD", "--side", "buy", "--type", "limit", "--size", "0.10000", "--limit-price", "62000",
		"--expires-at", "1791460800000", "--reduce-only", "--data", `{"client_order_id":"from-data","side":"sell"}`, "--set", "exits.take_profit.0.price=64000",
		"--set", "exits.stop_loss.limit_offset_ticks=5", "-o", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	reqs := api.calls("POST", "/v1/accounts/SANDBOX-00042-001/orders")
	if len(reqs) != 1 {
		t.Fatalf("%d orders sent", len(reqs))
	}
	r := reqs[0]
	got, _ := json.Marshal(r.body)
	want := `{"client_order_id":"from-data","exits":{"stop_loss":{"limit_offset_ticks":5},"take_profit":[{"price":"64000"}]},"expires_at":1791460800000,"limit_price":"62000","market":"BTC-USD","reduce_only":true,"side":"buy","size":"0.10000","type":"limit"}`
	if string(got) != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
	if r.auth != "Bearer "+testKey || len(r.key) != 36 {
		t.Fatalf("auth %q, Idempotency-Key %q", r.auth, r.key)
	}
	if !strings.Contains(out, `"size": "0.10000"`) {
		t.Fatalf("decimals not kept: %s", out)
	}
	// the same key again when asked for
	_, _, code = h.run("place-order", "--market", "BTC-USD", "--side", "buy", "--type", "market", "--size", "1", "--idempotency-key", "retry-1")
	if code != 0 || api.calls("POST", "/v1/accounts/SANDBOX-00042-001/orders")[1].key != "retry-1" {
		t.Fatal("--idempotency-key not sent")
	}
}

func TestOutputs(t *testing.T) {
	api, srv := newAPI(t)
	api.routes["GET /v1/accounts/SANDBOX-00042-001/positions"] = reply(200, `{"data":[
		{"market":"BTC-USD","side":"long","size":"0.500","entry_price":"62000.5","mark_price":"63010.0","unrealized_pnl":"504.75","leverage":10,"margin_mode":"isolated","liquidation":{"price":"56420.5","distance_pct":"10.4","account_wide":false},"protected":true},
		{"market":"ETH-USD","side":"short","size":"2.00","entry_price":"3500","mark_price":null,"unrealized_pnl":"-12.50","leverage":5,"margin_mode":"cross","liquidation":null,"protected":false}]}`)
	h := newHarness(t, srv)
	h.signIn("sandbox", testKey)
	table, _, _ := h.run("positions", "-o", "table")
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "MARKET") || !strings.Contains(lines[1], "56420.5") || !strings.Contains(lines[2], "-12.50") || !strings.Contains(lines[2], " - ") {
		t.Fatalf("table:\n%s", table)
	}
	csv, _, _ := h.run("positions", "-o", "csv")
	if !strings.HasPrefix(csv, "market,side,size,entry_price,mark_price,unrealized_pnl,leverage,margin_mode,liquidation.price,protected\nBTC-USD,long,0.500,62000.5,63010.0,504.75,10,isolated,56420.5,true\nETH-USD,short,2.00,3500,,-12.50,5,cross,,false\n") {
		t.Fatalf("csv:\n%s", csv)
	}
	jsonl, _, _ := h.run("positions", "-o", "jsonl")
	if n := strings.Count(jsonl, "\n"); n != 2 || !strings.HasPrefix(jsonl, `{"market":"BTC-USD","side":"long","size":"0.500"`) {
		t.Fatalf("jsonl:\n%s", jsonl)
	}
	jq, _, _ := h.run("positions", "--jq", ".[] | select(.side == \"short\") | .unrealized_pnl")
	if jq != "-12.50\n" {
		t.Fatalf("jq: %q", jq)
	}
	if q := api.calls("GET", "/v1/accounts/SANDBOX-00042-001/positions"); len(q) != 4 || q[0].query.Encode() != "" {
		t.Fatalf("positions calls: %+v", q)
	}
}

func TestExitCodes(t *testing.T) {
	api, srv := newAPI(t)
	h := newHarness(t, srv)
	h.signIn("sandbox", testKey)
	for status, want := range map[int]int{401: ExitAuth, 403: ExitAuth, 404: ExitNotFound, 409: ExitConflict, 422: ExitRule, 400: ExitUsage, 500: ExitServer} {
		api.routes["DELETE /v1/accounts/SANDBOX-00042-001/orders/ord_9"] = reply(status, `{"error":{"code":"x_code","message":"Said no."}}`)
		_, errOut, code := h.run("cancel-order", "ord_9")
		if code != want || !strings.Contains(errOut, "Said no. (x_code, HTTP") {
			t.Fatalf("%d: exit %d, %s", status, code, errOut)
		}
	}
	api.routes["GET /v1/limits"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limited","message":"Slow down."}}`)
	}
	if _, errOut, code := h.run("get-limits"); code != ExitRate || !strings.Contains(errOut, "try again in 2m0s") {
		t.Fatalf("429: %d %s", code, errOut)
	}
	if _, _, code := h.run("get-order"); code != ExitUsage {
		t.Fatalf("missing argument: %d", code)
	}
	if _, _, code := h.run("nope"); code != ExitUsage {
		t.Fatalf("unknown command: %d", code)
	}
	h.env["PHOENIXPERPS_BASE_URL"] = "https://api.example.com"
	if _, errOut, code := h.run("accounts"); code != ExitUsage || !strings.Contains(errOut, "phoenixperpsfunding.com") {
		t.Fatalf("foreign host: %d %s", code, errOut)
	}
	h.env["PHOENIXPERPS_BASE_URL"] = "http://127.0.0.1:1"
	if _, _, code := h.run("cancel-order", "ord_9"); code != ExitNetwork {
		t.Fatalf("no network: %d", code)
	}
	delete(h.env, "PHOENIXPERPS_BASE_URL")
	h.env["PHOENIXPERPS_API_KEY"] = "ppk_live_" + strings.Repeat("A", 50)
	if _, errOut, code := h.run("--sandbox", "accounts"); code != ExitAuth || !strings.Contains(errOut, "wrong_environment") {
		t.Fatalf("live key on the sandbox: %d %s", code, errOut)
	}
}

func TestTradesAsk(t *testing.T) {
	api, srv := newAPI(t)
	api.routes["POST /v1/accounts/SANDBOX-00042-001/orders/test"] = reply(200, `{"data":{"result":"would_fill","execution":{"expected_price":"63010","worst_price":"63640"},"size":"0.01","notional":"630.10","fee":"0.32","margin":{"required":"63.01","available_after":"99936.99"}}}`)
	api.routes["POST /v1/accounts/SANDBOX-00042-001/orders"] = reply(201, `{"data":{"id":"ord_2","status":"filled"}}`)
	h := newHarness(t, srv)
	h.signIn("sandbox", testKey)

	// sandbox: no question, no preview
	if _, errOut, code := h.run("buy", "BTC-USD", "0.01"); code != 0 || len(api.calls("POST", "/v1/accounts/SANDBOX-00042-001/orders/test")) != 0 {
		t.Fatalf("sandbox buy: %d %s", code, errOut)
	}
	// confirm_trades on: no one to ask means --yes, and nothing is sent without it
	h.config("[profiles.sandbox]\nenv = \"sandbox\"\nconfirm_trades = true\n")
	if _, errOut, code := h.run("sell", "BTC-USD", "$630"); code != ExitUsage || !strings.Contains(errOut, "--yes") || len(api.calls("POST", "/v1/accounts/SANDBOX-00042-001/orders")) != 1 {
		t.Fatalf("unasked sell: %d %s", code, errOut)
	}
	// on a terminal: the preview, then the question
	h.tty, h.stdin = true, "n\n"
	if _, errOut, code := h.run("sell", "BTC-USD", "$630"); code != ExitFailed || !strings.Contains(errOut, "price 63010") || !strings.Contains(errOut, "margin 63.01") {
		t.Fatalf("declined: %d %s", code, errOut)
	}
	h.stdin = "y\n"
	if _, errOut, code := h.run("sell", "BTC-USD", "$630", "--tp", "2%", "--sl", "61000", "--trail", "1%"); code != 0 {
		t.Fatalf("accepted: %d %s", code, errOut)
	}
	placed := api.calls("POST", "/v1/accounts/SANDBOX-00042-001/orders")
	got, _ := json.Marshal(placed[len(placed)-1].body)
	if want := `{"exits":{"stop_loss":{"price":"61000","trail":{"percent":"1"}},"take_profit":[{"percent":"2"}]},"market":"BTC-USD","notional_usd":"630","side":"sell","type":"market"}`; string(got) != want || len(placed) != 2 {
		t.Fatalf("placed %d, last %s", len(placed), got)
	}
	// --dry-run: the what-if only
	h.tty = false
	if out, _, code := h.run("buy", "BTC-USD", "0.01", "--limit", "62000", "--dry-run", "-o", "json"); code != 0 || !strings.Contains(out, "would_fill") || len(api.calls("POST", "/v1/accounts/SANDBOX-00042-001/orders")) != 2 {
		t.Fatalf("dry run: %d %s", code, out)
	}
}

func TestCloseAndFlatten(t *testing.T) {
	api, srv := newAPI(t)
	api.routes["GET /v1/accounts/SANDBOX-00042-001/positions/BTC-USD"] = reply(200, `{"data":{"market":"BTC-USD","side":"long","size":"0.37"}}`)
	api.routes["GET /v1/markets/BTC-USD"] = reply(200, `{"data":{"market":"BTC-USD","size_step":"0.01"}}`)
	api.routes["POST /v1/accounts/SANDBOX-00042-001/positions/BTC-USD/close"] = reply(201, `{"data":{"id":"ord_3","status":"filled"}}`)
	var n atomic.Int32
	api.routes["POST /v1/accounts/SANDBOX-00042-001/flatten"] = func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(202)
			_, _ = io.WriteString(w, `{"data":{"account_id":"SANDBOX-00042-001","orders_canceled":2,"closes":[]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"account_id":"SANDBOX-00042-001","orders_canceled":2,"closes":[{"market":"BTC-USD"}]}}`)
	}
	h := newHarness(t, srv)
	h.signIn("sandbox", testKey)
	if _, errOut, code := h.run("close", "btc-usd", "--percent", "50"); code != 0 || !strings.Contains(errOut, "0.18") {
		t.Fatalf("close: %d %s", code, errOut)
	}
	if c := api.calls("POST", "/v1/accounts/SANDBOX-00042-001/positions/BTC-USD/close"); c[0].body["size"] != "0.18" || len(c[0].key) != 36 {
		t.Fatalf("close body %+v", c[0])
	}
	if _, _, code := h.run("flatten"); code != ExitUsage || n.Load() != 0 {
		t.Fatal("flatten without --yes")
	}
	out, errOut, code := h.run("flatten", "--yes", "-o", "json")
	calls := api.calls("POST", "/v1/accounts/SANDBOX-00042-001/flatten")
	if code != 0 || len(calls) != 3 || calls[0].key != calls[2].key || !strings.Contains(out, `"orders_canceled": 2`) || !strings.Contains(errOut, "flattening") {
		t.Fatalf("flatten: %d, %d calls, %s %s", code, len(calls), out, errOut)
	}
}

func TestAllPages(t *testing.T) {
	api, srv := newAPI(t)
	api.routes["GET /v1/accounts/SANDBOX-00042-001/fills"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_, _ = io.WriteString(w, `{"data":[{"id":"f1"}],"has_more":true,"next_cursor":"c2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"f2"}],"has_more":false,"next_cursor":null}`)
	}
	h := newHarness(t, srv)
	h.signIn("sandbox", testKey)
	out, errOut, _ := h.run("list-fills", "--jq", "map(.id) | join(\",\")")
	if out != "f1\n" || !strings.Contains(errOut, "--cursor c2") {
		t.Fatalf("one page: %q %q", out, errOut)
	}
	if out, _, _ := h.run("list-fills", "--all-pages", "--jq", "map(.id) | join(\",\")"); out != "f1,f2\n" {
		t.Fatalf("all pages: %q", out)
	}
}

func TestLogin(t *testing.T) {
	api, srv := newAPI(t)
	var mu sync.Mutex
	var challenge string
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/oauth/token":
			mu.Lock()
			want := challenge
			mu.Unlock()
			if r.PostForm.Get("client_id") != "pperp-cli" || challengeOf(r.PostForm.Get("code_verifier")) != want || r.PostForm.Get("code") != "code-1" {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"code_verifier does not match."}`)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"ppo_test_signedin","token_type":"Bearer","expires_in":7776000,"scope":"read trade"}`)
		case "/oauth/revoke":
			w.WriteHeader(200)
		}
	}))
	defer auth.Close()
	h := newHarness(t, srv)
	h.env["PPERP_AUTH_URL"] = auth.URL
	iss := auth.URL
	h.browser = func(u string) error { // the browser: approves, and comes back to the loopback address
		q, _ := url.Parse(u)
		v := q.Query()
		mu.Lock()
		challenge = v.Get("code_challenge")
		mu.Unlock()
		if v.Get("client_id") != "pperp-cli" || v.Get("code_challenge_method") != "S256" || v.Get("resource") != srv.URL || v.Get("scope") != "read trade" {
			t.Errorf("authorize: %s", u)
		}
		go func() {
			back := v.Get("redirect_uri") + "?" + url.Values{"code": {"code-1"}, "state": {v.Get("state")}, "iss": {iss}}.Encode()
			if res, err := http.Get(back); err == nil {
				res.Body.Close()
			}
		}()
		return nil
	}
	_, errOut, code := h.run("login", "--sandbox")
	if code != 0 || !strings.Contains(errOut, "Signed in on profile sandbox") {
		t.Fatalf("login: %d %s", code, errOut)
	}
	s, _ := keyring.Get(keychainService, "profile:sandbox")
	if !strings.Contains(s, "ppo_test_signedin") || api.calls("GET", "/v1/me")[0].auth != "Bearer ppo_test_signedin" {
		t.Fatalf("kept %q", s)
	}
	cfg, _ := os.ReadFile(filepath.Join(h.env["PPERP_CONFIG_DIR"], "config.toml"))
	if !strings.Contains(string(cfg), `profile = "sandbox"`) || strings.Contains(string(cfg), "ppo_") {
		t.Fatalf("config:\n%s", cfg)
	}
	// an answer from another issuer is refused, and nothing is kept
	_, _, _ = h.run("logout", "--sandbox")
	iss = "https://elsewhere.example"
	if _, errOut, code := h.run("login", "--sandbox"); code != ExitAuth || !strings.Contains(errOut, "another issuer") {
		t.Fatalf("mix-up: %d %s", code, errOut)
	}
	if _, err := keyring.Get(keychainService, "profile:sandbox"); err != keyring.ErrNotFound {
		t.Fatal("kept after a refused sign-in")
	}
	// a live key is never kept
	h.stdin = "ppk_live_x\n"
	if _, errOut, code := h.run("login", "--profile", "live", "--api-key-stdin"); code != ExitUsage || !strings.Contains(errOut, "live keys") {
		t.Fatalf("live key: %d %s", code, errOut)
	}
}

func TestAPIPassthrough(t *testing.T) {
	api, srv := newAPI(t)
	api.routes["GET /v1/limits"] = reply(200, `{"data":{"read_per_min":600}}`)
	h := newHarness(t, srv)
	h.signIn("sandbox", testKey)
	out, _, code := h.run("api", "GET", "/v1/limits?x=1", "-f", "y=2", "-o", "json")
	if code != 0 || !strings.Contains(out, `"read_per_min": 600`) || api.calls("GET", "/v1/limits")[0].query.Encode() != "x=1&y=2" {
		t.Fatalf("api: %d %s", code, out)
	}
	if _, _, code := h.run("api", "GET", "https://elsewhere.example/v1/limits"); code != ExitUsage {
		t.Fatal("another host taken")
	}
}

func TestUpgradeCheck(t *testing.T) {
	k, err := release.NewSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	latest := []byte(`{"version":"1.3.0","published_at":"2026-10-11T00:00:00Z","min_supported":"1.0.0","revoked":["1.2.5"],"notes":""}`)
	files := map[string][]byte{"latest.json": latest, "latest.json.minisig": []byte(k.Sign(latest, "file:latest.json version:1.3.0"))}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer site.Close()
	keyring.MockInit()
	run := func(version string, args ...string) (string, string, int) {
		var out, errb bytes.Buffer
		env := map[string]string{"PPERP_CONFIG_DIR": t.TempDir(), "PPERP_RELEASES_URL": site.URL}
		a := &App{Version: version, In: strings.NewReader(""), Out: &out, Err: &errb, Getenv: func(k string) string { return env[k] }, releaseKeys: []release.PublicKey{k.Public()}}
		code := a.Run(context.Background(), args)
		return out.String(), errb.String(), code
	}
	if out, _, code := run("1.2.0", "upgrade", "--check"); code != 0 || !strings.Contains(out, "pperp 1.3.0 is out (you have 1.2.0)") {
		t.Fatalf("check: %d %q", code, out)
	}
	if _, errOut, code := run("1.3.0", "upgrade"); code != 0 || !strings.Contains(errOut, "is the latest") {
		t.Fatalf("latest: %d %q", code, errOut)
	}
	if _, errOut, code := run("1.3.0", "upgrade", "--version", "1.2.5"); code != ExitFailed || !strings.Contains(errOut, "withdrawn") {
		t.Fatalf("withdrawn: %d %q", code, errOut)
	}
	other, _ := release.NewSecretKey()
	files["latest.json.minisig"] = []byte(other.Sign(latest, "file:latest.json version:1.3.0"))
	if _, errOut, code := run("1.2.0", "upgrade", "--check"); code != ExitNetwork || !strings.Contains(errOut, "does not know") {
		t.Fatalf("another key: %d %q", code, errOut)
	}
}
