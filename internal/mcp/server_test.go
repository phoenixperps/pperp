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
)

// The MCP server against a fake API (docs/api/design/15-cli-mcp.md "Verification"): who is taken where, the tools each
// caller sees, an idempotency key checked before any call, a trade previewed then placed only with its confirmation,
// and the API's answers shaped (429, 202, no answer in time).

type seen struct {
	method, path, ip, key, auth, host string
	body                              map[string]any
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
	f.seen = append(f.seen, seen{r.Method, r.URL.Path, r.Header.Get("X-Forwarded-For"), r.Header.Get("Idempotency-Key"), token, r.Host, body})
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
	case strings.HasSuffix(r.URL.Path, "/orders/test"):
		_, _ = io.WriteString(w, `{"data":{"result":"would_rest","margin":"124"}}`)
	case strings.HasSuffix(r.URL.Path, "/orders") && r.Method == "POST":
		switch r.Header.Get("Idempotency-Key") {
		case "slow":
			time.Sleep(300 * time.Millisecond)
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
	po := live["place-order"]
	if po == nil || po.Annotations.DestructiveHint == nil || !*po.Annotations.DestructiveHint || po.Annotations.IdempotentHint {
		t.Fatalf("place-order: %+v", po)
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
