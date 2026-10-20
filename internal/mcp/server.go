// Package mcp is pperp-mcpd, the hosted MCP server (docs/api/design/15-cli-mcp.md section 5, as reconciled in
// reviews/integration.md #14-#17): Streamable HTTP, stateless, JSON answers, at POST / (live) and POST /sandbox.
//
// It is a client of the API only, through the gateway's local socket: every call carries the caller's own token, so
// the key's or the grant's limits, the API's budgets and its AI budget all apply. On live only an assistant's OAuth
// token (ppo_live_, asked for this server) is taken: a live key never sits in an assistant's configuration; /sandbox
// also takes a sandbox key. The tools are the API's operations (internal/ops), filtered by the caller's access and the
// environment; a trade on a grant that asked for previews is previewed first (confirm.go).
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config is the server's settings (env MCP_*).
type Config struct {
	PublicURL  string // https://mcp.phoenixperpsfunding.com
	AuthServer string // https://phoenixperpsfunding.com (the OAuth authorization server)
	DocsURL    string // where a browser is sent
	Version    string
}

// Handler serves the MCP endpoints.
type Handler struct {
	cfg     Config
	gateway *Gateway
	confirm *Confirmations
	mu      sync.Mutex
	cache   map[string]cached // sha256(token) -> /v1/me, 30 s
	now     func() time.Time
}

type cached struct {
	me  *Me
	at  time.Time
	err *authError
}

type authError struct {
	status int
	code   string
	text   string
}

// New is the handler.
func New(cfg Config, gw *Gateway, confirm *Confirmations) *Handler {
	return &Handler{cfg: cfg, gateway: gw, confirm: confirm, cache: map[string]cached{}, now: time.Now}
}

func (h *Handler) metadataURL(env string) string {
	if env == "sandbox" {
		return h.cfg.PublicURL + "/.well-known/oauth-protected-resource/sandbox"
	}
	return h.cfg.PublicURL + "/.well-known/oauth-protected-resource"
}

func (h *Handler) resource(env string) string {
	if env == "sandbox" {
		return h.cfg.PublicURL + "/sandbox"
	}
	return h.cfg.PublicURL
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// unauthorized answers 401 with where to sign in (RFC 9728): an MCP client starts OAuth from it.
func (h *Handler) unauthorized(w http.ResponseWriter, env, code, text string) {
	v := `Bearer resource_metadata="` + h.metadataURL(env) + `", scope="read"`
	if code == "invalid_token" {
		v = `Bearer error="invalid_token", resource_metadata="` + h.metadataURL(env) + `"`
	}
	w.Header().Set("WWW-Authenticate", v)
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": code, "message": text}})
}

// clientIP is the caller's address as nginx saw it (the server listens on loopback behind it).
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		parts := strings.Split(v, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	return ""
}

// me is the caller's credential as the API sees it, kept 30 s.
func (h *Handler) me(ctx context.Context, env, token, ip string) (*Me, *authError) {
	sum := sha256.Sum256([]byte(env + "|" + token))
	k := hex.EncodeToString(sum[:])
	h.mu.Lock()
	c, ok := h.cache[k]
	h.mu.Unlock()
	if ok && h.now().Sub(c.at) < 30*time.Second {
		return c.me, c.err
	}
	a, err := h.gateway.Do(ctx, env, "GET", h.gateway.origin(env)+"/v1/me", token, ip, "", nil)
	if err != nil || a.TimedOut {
		return nil, &authError{status: http.StatusBadGateway, code: "unreachable", text: "The API could not be reached. Try again."}
	}
	var res cached
	res.at = h.now()
	if a.Status == http.StatusOK {
		var w struct {
			Data Me `json:"data"`
		}
		if json.Unmarshal(a.Body, &w) != nil {
			return nil, &authError{status: http.StatusBadGateway, code: "unreachable", text: "The API's answer could not be read."}
		}
		res.me = &w.Data
	} else {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(a.Body, &e)
		status := a.Status
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			res.err = &authError{status: status, code: "invalid_token", text: e.Error.Message}
		} else {
			return nil, &authError{status: status, code: e.Error.Code, text: e.Error.Message}
		}
	}
	h.mu.Lock()
	if len(h.cache) > 10000 { // a bound, not a policy: forget everything rather than grow
		h.cache = map[string]cached{}
	}
	h.cache[k] = res
	h.mu.Unlock()
	return res.me, res.err
}

// accepted tells whether a token may be used at this endpoint (by its prefix, before any lookup).
func accepted(env, token string) bool {
	if env == "live" {
		return strings.HasPrefix(token, "ppo_live_")
	}
	return strings.HasPrefix(token, "ppo_test_") || strings.HasPrefix(token, "ppk_test_")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/healthz":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": h.cfg.Version})
		return
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/sandbox":
		env := "live"
		if strings.HasSuffix(r.URL.Path, "/sandbox") {
			env = "sandbox"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"resource": h.resource(env), "authorization_servers": []string{h.cfg.AuthServer},
			"scopes_supported": []string{"read", "trade"}, "bearer_methods_supported": []string{"header"},
			"resource_name": "Phoenix Perpetuals", "resource_documentation": h.cfg.DocsURL,
		})
		return
	case "/", "/sandbox":
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "route_not_found", "message": "MCP is at / (live) and /sandbox."}})
		return
	}
	env := "live"
	if r.URL.Path == "/sandbox" {
		env = "sandbox"
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "authorization, content-type, mcp-protocol-version, mcp-session-id")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, h.cfg.DocsURL, http.StatusFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"code": "method_not_allowed", "message": "MCP takes POST."}})
		return
	}
	auth := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if !strings.HasPrefix(auth, "Bearer ") || token == "" {
		h.unauthorized(w, env, "missing_token", "Sign in: this server takes an OAuth token (your MCP client starts the sign-in from this answer).")
		return
	}
	if !accepted(env, token) {
		if env == "live" {
			h.unauthorized(w, env, "invalid_token", "On live, connect through sign-in (OAuth): an API key is never put in an assistant's configuration. For tests, /sandbox takes a sandbox key.")
		} else {
			h.unauthorized(w, env, "invalid_token", "The sandbox takes a sandbox sign-in or a sandbox key (ppk_test_).")
		}
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	ip := clientIP(r)
	me, aerr := h.me(r.Context(), env, token, ip)
	if aerr != nil {
		if aerr.code == "invalid_token" {
			h.unauthorized(w, env, "invalid_token", firstOf(aerr.text, "This sign-in has ended: sign in again."))
			return
		}
		writeJSON(w, aerr.status, map[string]any{"error": map[string]string{"code": aerr.code, "message": aerr.text}})
		return
	}
	s := &session{env: env, token: token, ip: ip, me: me}
	srv := sdk.NewServer(&sdk.Implementation{Name: "phoenix-perpetuals", Title: "Phoenix Perpetuals", Version: h.cfg.Version}, &sdk.ServerOptions{Instructions: Instructions})
	h.tools(srv, s)
	sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true}).ServeHTTP(w, r)
}
