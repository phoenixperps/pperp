package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/phoenixperps/pperp/internal/ops"
)

// The MCP server against a fake API (docs/api/design/15-cli-mcp.md "Verification"): who is taken where, the tools each
// caller sees, an idempotency key checked before any call, a trade previewed then placed only with its confirmation,
// and the API's answers shaped (429, 202, no answer in time).

type seen struct {
	method, path, ip, key, auth, host, channel string
	body                                       map[string]any
}

type fakeAPI struct {
	mu   sync.Mutex
	seen []seen
}

func (f *fakeAPI) requests(path string) []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []seen
	for _, s := range f.seen {
		if path == "" || s.path == path {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.seen = append(f.seen, seen{r.Method, r.URL.Path, r.Header.Get("X-Forwarded-For"), r.Header.Get("Idempotency-Key"), token, r.Host, r.Header.Get("X-PP-Channel"), body})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req_fake")
	switch {
	case r.URL.Path == "/v1/me":
		access, confirm := "trade", false
		switch token {
		case "ppo_live_read":
			access = "read"
		case "ppo_live_trade":
			confirm = true
		case "ppo_live_gone":
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"code":"revoked_token","message":"This sign-in was ended."}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"credential": map[string]any{"kind": "app", "id": "g1", "environment": "live", "access": access},
			"assistant":  map[string]any{"confirm_trades": confirm, "confirm_seconds": 120},
		}})
	case strings.HasSuffix(r.URL.Path, "/orders/test") && body["size"] == "0.0015":
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_size_step","message":"The size is not a whole number of steps.","details":{"size_step":"0.001"}}}`)
	case strings.HasSuffix(r.URL.Path, "/orders/test") && body["size"] == "7":
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"code":"state_unavailable","message":"Try again."}}`)
	case strings.HasSuffix(r.URL.Path, "/close"):
		w.Header().Set("Warning", `299 - "size above the position: the whole position was closed"`)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"data":{"id":"ord_2","status":"filled"}}`)
	case strings.HasSuffix(r.URL.Path, "/orders/test") && body["size"] == "999":
		_, _ = io.WriteString(w, `{"data":{"result":"would_reject","reject":{"code":"insufficient_margin","message":"Not enough margin.","details":{"required_usd":"9","available_usd":"1"}}}}`)
	case strings.HasSuffix(r.URL.Path, "/orders/test"):
		_, _ = io.WriteString(w, `{"data":{"result":"would_rest","margin":"124"}}`)
	case strings.HasSuffix(r.URL.Path, "/orders") && r.Method == "POST":
		switch r.Header.Get("Idempotency-Key") {
		case "slow":
			time.Sleep(300 * time.Millisecond)
		case "replay":
			w.Header().Set("Idempotent-Replayed", "true")
		case "busy":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limited","message":"slow down"}}`)
			return
		}
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"data":{"id":"ord_1","status":"working"}}`)
	case strings.HasSuffix(r.URL.Path, "/flatten"):
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(202)
		_, _ = io.WriteString(w, `{"data":{"status":"running"}}`)
	default:
		_, _ = io.WriteString(w, `{"data":[]}`)
	}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	r.Header.Set("X-Real-IP", "203.0.113.9")
	// as nginx sends it: the public host name, to a server listening on loopback
	r.Host = "mcp.test"
	return b.base.RoundTrip(r)
}

func setup(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	api := &fakeAPI{}
	upstream := httptest.NewServer(api)
	t.Cleanup(upstream.Close)
	host := strings.TrimPrefix(upstream.URL, "http://")
	gw := NewGateway(upstream.URL, map[string]string{"live": host, "sandbox": host}, "pperp-mcpd/test")
	gw.Timeout = 150 * time.Millisecond
	h := New(Config{PublicURL: "https://mcp.test", AuthServer: "https://site.test", DocsURL: "https://docs.test/mcp", Version: "test"}, gw, NewConfirmations([]byte(strings.Repeat("k", 32)), nil))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return api, srv
}

func connect(t *testing.T, url, token string) *sdk.ClientSession {
	t.Helper()
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}}}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func names(t *testing.T, s *sdk.ClientSession) map[string]*sdk.Tool {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*sdk.Tool{}
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

func result(t *testing.T, r *sdk.CallToolResult) Result {
	t.Helper()
	var out Result
	b, _ := json.Marshal(r.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func call(t *testing.T, s *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, Result) {
	t.Helper()
	r, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return r, result(t, r)
}

func TestWhoIsTakenWhere(t *testing.T) {
	api, srv := setup(t)
	for _, c := range []struct {
		path, auth, want string
	}{
		{"/", "", "missing_token"},
		{"/", "Bearer ppk_live_somekey", "invalid_token"},
		{"/", "Bearer ppo_test_session", "invalid_token"},
		{"/sandbox", "Bearer ppk_live_somekey", "invalid_token"},
		{"/", "Bearer ppo_live_gone", "invalid_token"},
	} {
		req, _ := http.NewRequest("POST", srv.URL+c.path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		res.Body.Close()
		if res.StatusCode != 401 || e.Error.Code != c.want {
			t.Fatalf("%s %q: %d %s, want 401 %s", c.path, c.auth, res.StatusCode, e.Error.Code, c.want)
		}
		if wa := res.Header.Get("WWW-Authenticate"); !strings.Contains(wa, `resource_metadata="https://mcp.test/.well-known/oauth-protected-resource`) {
			t.Fatalf("WWW-Authenticate: %q", wa)
		}
	}
	if n := len(api.requests("/v1/me")); n != 1 {
		t.Fatalf("only the well-shaped token reaches the API: %d calls", n)
	}
	res, _ := http.Get(srv.URL + "/.well-known/oauth-protected-resource/sandbox")
	var md map[string]any
	_ = json.NewDecoder(res.Body).Decode(&md)
	res.Body.Close()
	if md["resource"] != "https://mcp.test/sandbox" || md["authorization_servers"].([]any)[0] != "https://site.test" {
		t.Fatalf("metadata: %v", md)
	}
}

func TestToolsByAccessAndEnvironment(t *testing.T) {
	_, srv := setup(t)
	read := names(t, connect(t, srv.URL+"/", "ppo_live_read"))
	if _, ok := read["list-orders"]; !ok {
		t.Fatal("a read grant reads")
	}
	if _, ok := read["place-order"]; ok {
		t.Fatal("a read grant never sees a trading tool")
	}
	if !read["list-orders"].Annotations.ReadOnlyHint {
		t.Fatal("reads are read-only")
	}
	live := names(t, connect(t, srv.URL+"/", "ppo_live_trade"))
	for _, n := range []string{"flatten", "cancel-all-orders", "list-trading-view-hooks", "list-webhook-endpoints", "create-sandbox-account", "get-spec"} {
		if _, ok := live[n]; ok {
			t.Fatalf("live: %s left out", n)
		}
	}
	// the spec's annotations (Q111): an order with its idempotency key is additive and may be sent again; what ends
	// something is destructive; a preview changes nothing
	po := live["place-order"]
	if po == nil || po.Annotations.DestructiveHint == nil || *po.Annotations.DestructiveHint || !po.Annotations.IdempotentHint {
		t.Fatalf("place-order: %+v", po.Annotations)
	}
	if cp := live["close-position"]; cp == nil || cp.Annotations.DestructiveHint == nil || !*cp.Annotations.DestructiveHint {
		t.Fatalf("close-position: %+v", cp)
	}
	if to := live["test-order"]; to == nil || !to.Annotations.ReadOnlyHint {
		t.Fatalf("test-order: %+v", to)
	}
	schema, _ := json.Marshal(po.InputSchema)
	for _, f := range []string{`"idempotency_key"`, `"confirmation"`, `"account_id"`, `"limit_price"`} {
		if !strings.Contains(string(schema), f) {
			t.Fatalf("place-order arguments: %s missing", f)
		}
	}
	sandbox := names(t, connect(t, srv.URL+"/sandbox", "ppk_test_key"))
	if _, ok := sandbox["flatten"]; !ok {
		t.Fatal("the sandbox keeps flatten")
	}
	if _, ok := sandbox["create-sandbox-account"]; ok {
		t.Fatal("never: opening sandbox accounts")
	}
	if strings.Contains(func() string { b, _ := json.Marshal(sandbox["place-order"].InputSchema); return string(b) }(), `"confirmation"`) {
		t.Fatal("a key that never asked for previews has no confirmation argument")
	}
}

func TestPreviewThenConfirm(t *testing.T) {
	api, srv := setup(t)
	s := connect(t, srv.URL+"/", "ppo_live_trade")
	order := map[string]any{"account_id": "PPERPS-00042-001", "market": "BTC-USD", "side": "buy", "type": "limit", "size": "0.01", "limit_price": "62000"}
	r, out := call(t, s, "place-order", order)
	if out.Code != "idempotency_key_required" || !r.IsError || len(api.requests("/v1/accounts/PPERPS-00042-001/orders")) != 0 {
		t.Fatalf("no key, nothing sent: %+v", out)
	}
	order["idempotency_key"] = "k-1"
	r, out = call(t, s, "place-order", order)
	if out.Status != 428 || out.Code != "confirmation_required" || out.Confirmation == "" || r.IsError {
		t.Fatalf("a preview first: %+v", out)
	}
	if !strings.Contains(out.Summary, "BUY 0.01 BTC-USD limit at 62000 on PPERPS-00042-001") {
		t.Fatalf("summary: %q", out.Summary)
	}
	if p, _ := out.Preview.(map[string]any); p["result"] != "would_rest" {
		t.Fatalf("the what-if: %+v", out.Preview)
	}
	if len(api.requests("/v1/accounts/PPERPS-00042-001/orders")) != 0 {
		t.Fatal("a preview sends no order")
	}
	changed := map[string]any{}
	for k, v := range order {
		changed[k] = v
	}
	changed["size"] = "1"
	changed["confirmation"] = out.Confirmation
	_, again := call(t, s, "place-order", changed)
	if again.Status != 428 || again.NextAction == "" {
		t.Fatalf("other arguments: a new preview: %+v", again)
	}
	order["confirmation"] = out.Confirmation
	_, placed := call(t, s, "place-order", order)
	if placed.Status != 201 || placed.RequestID != "req_fake" || placed.IdempotencyKey != "k-1" {
		t.Fatalf("confirmed: placed: %+v", placed)
	}
	sent := api.requests("/v1/accounts/PPERPS-00042-001/orders")
	if len(sent) != 1 || sent[0].key != "k-1" || sent[0].ip != "203.0.113.9" || sent[0].auth != "ppo_live_trade" {
		t.Fatalf("sent: %+v", sent)
	}
	if _, ok := sent[0].body["confirmation"]; ok {
		t.Fatal("the confirmation is never sent on")
	}
	if _, ok := sent[0].body["account_id"]; ok {
		t.Fatal("a path argument is never sent in the body")
	}
}

func TestAnswersShaped(t *testing.T) {
	_, srv := setup(t)
	s := connect(t, srv.URL+"/sandbox", "ppk_test_key")
	base := map[string]any{"account_id": "SANDBOX-00042-001", "market": "BTC-USD", "side": "buy", "type": "market", "size": "0.01"}
	with := func(k string) map[string]any {
		m := map[string]any{"idempotency_key": k}
		for a, v := range base {
			m[a] = v
		}
		return m
	}
	r, busy := call(t, s, "place-order", with("busy"))
	if busy.Status != 429 || busy.RetryAfterSeconds == nil || *busy.RetryAfterSeconds != 7 || !r.IsError {
		t.Fatalf("429: %+v", busy)
	}
	r, slow := call(t, s, "place-order", with("slow"))
	if slow.Status != 0 || slow.Code != "outcome_unknown" || slow.IdempotencyKey != "slow" || !r.IsError {
		t.Fatalf("no answer in time: %+v", slow)
	}
	_, running := call(t, s, "flatten", map[string]any{"account_id": "SANDBOX-00042-001", "idempotency_key": "f-1"})
	if running.Status != 202 || running.NextAction == "" {
		t.Fatalf("202: %+v", running)
	}
}

// pperp mcp: the same tools over the public API for one credential, read only unless trading is allowed.
func TestLocal(t *testing.T) {
	api := &fakeAPI{}
	upstream := httptest.NewServer(api)
	defer upstream.Close()
	gw := NewRemote(map[string]string{"sandbox": upstream.URL}, http.DefaultClient, "pperp/test")
	ctx := context.Background()
	for _, allow := range []bool{false, true} {
		srv, err := Local(ctx, gw, "sandbox", "ppk_test_local", "test", allow, false)
		if err != nil {
			t.Fatal(err)
		}
		st, ct := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		s, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		tools := names(t, s)
		if _, ok := tools["place-order"]; ok != allow || tools["list-positions"] == nil {
			t.Fatalf("allow %v: place-order there %v", allow, ok)
		}
		if allow {
			_, r := call(t, s, "place-order", map[string]any{"account_id": "SANDBOX-00042-001", "market": "BTC-USD", "side": "buy", "type": "market", "size": "0.01", "idempotency_key": "local-1"})
			if r.Status != 201 {
				t.Fatalf("place-order: %+v", r)
			}
		}
		_ = s.Close()
	}
	orders := api.requests("/v1/accounts/SANDBOX-00042-001/orders")
	if len(orders) != 1 || orders[0].auth != "ppk_test_local" || orders[0].key != "local-1" || orders[0].ip != "" {
		t.Fatalf("orders: %+v", orders)
	}
}

// The arguments a tool takes, checked by name and type before anything is sent (Q18, Q45); an order the API would refuse
// is said at once, never previewed for agreement (Q110); a replay says so (Q113); an assistant says it is one (Q44).
func TestArgumentsAndAnswers(t *testing.T) {
	api, srv := setup(t)
	s := connect(t, srv.URL+"/sandbox", "ppk_test_key")
	order := map[string]any{"account_id": "SANDBOX-00042-001", "market": "BTC-USD", "side": "buy", "type": "market", "size": "0.01", "idempotency_key": "a-1"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{k: v}
		for a, x := range order {
			if a != k {
				m[a] = x
			}
		}
		return m
	}
	for name, args := range map[string]map[string]any{
		"unknown": with("trigger_market", "ETH-USD"),
		"type":    with("size", true),
		"path":    func() map[string]any { m := with("size", "0.01"); delete(m, "account_id"); return m }(),
	} {
		r, out := call(t, s, "place-order", args)
		if out.Code != "invalid_arguments" || !r.IsError {
			t.Fatalf("%s: %+v", name, out)
		}
	}
	if n := len(api.requests("/v1/accounts/SANDBOX-00042-001/orders")); n != 0 {
		t.Fatalf("nothing sent: %d", n)
	}
	_, replayed := call(t, s, "place-order", with("idempotency_key", "replay"))
	if replayed.Status != 201 || !replayed.IdempotentReplayed {
		t.Fatalf("replayed: %+v", replayed)
	}
	if got := api.requests("/v1/accounts/SANDBOX-00042-001/orders"); len(got) != 1 || got[0].channel != "mcp" {
		t.Fatalf("an assistant says it is one: %+v", got)
	}
	live := connect(t, srv.URL+"/", "ppo_live_trade")
	big := map[string]any{"account_id": "PPERPS-00042-001", "market": "BTC-USD", "side": "buy", "type": "limit", "size": "999", "limit_price": "62000", "idempotency_key": "big-1"}
	r, refused := call(t, live, "place-order", big)
	if refused.Status != 422 || refused.Code != "insufficient_margin" || refused.Confirmation != "" || !r.IsError {
		t.Fatalf("would_reject: the refusal, never a preview: %+v", refused)
	}
	// R7 Q110: an order test refused (a size off its step) is that refusal, never a preview to agree to; one the API
	// could not make (503) is a preview that says so
	off := map[string]any{"account_id": "PPERPS-00042-001", "market": "BTC-USD", "side": "buy", "type": "limit", "size": "0.0015", "limit_price": "62000", "idempotency_key": "off-1"}
	if _, o := call(t, live, "place-order", off); o.Status != 400 || o.Code != "invalid_size_step" || o.Confirmation != "" {
		t.Fatalf("a refused order test is the refusal: %+v", o)
	}
	busy := map[string]any{"account_id": "PPERPS-00042-001", "market": "BTC-USD", "side": "buy", "type": "limit", "size": "7", "limit_price": "62000", "idempotency_key": "busy-1"}
	if _, o := call(t, live, "place-order", busy); o.Status != 428 || o.Confirmation == "" || !strings.Contains(o.NextAction, "No preview could be made") {
		t.Fatalf("a preview the API could not make says so: %+v", o)
	}
	// the API's Warning reaches the assistant (an oversize close said)
	if _, o := call(t, s, "close-position", map[string]any{"account_id": "SANDBOX-00042-001", "market": "BTC-USD", "size": "9", "idempotency_key": "c-1"}); !strings.Contains(o.Warning, "whole position") {
		t.Fatalf("the Warning: %+v", o)
	}
}

// R7: a tool that takes no idempotency_key never promises one in its description.
func TestToolsPromiseNoKeyTheyDoNotTake(t *testing.T) {
	for _, name := range []string{"modify-order", "set-exits"} {
		o, ok := ops.ByName(name)
		if !ok {
			t.Fatalf("no %s", name)
		}
		d := tool(o, false).Description
		if strings.Contains(d, "Idempotency-Key") || !strings.Contains(d, "takes no idempotency_key") {
			t.Fatalf("%s: %s", name, d)
		}
	}
	if o, _ := ops.ByName("place-order"); strings.Contains(tool(o, false).Description, "takes no idempotency_key") {
		t.Fatal("place-order takes one")
	}
}
