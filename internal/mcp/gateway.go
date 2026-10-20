package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/phoenixperps/pperp/internal/ops"
)

// Gateway is the API as the MCP server reaches it: the gateway's local socket (only this role may open it), the
// environment told by the host name, the caller's own token and address on every request.
type Gateway struct {
	Hosts   map[string]string // live, sandbox: the REST host names
	origins map[string]string // pperp mcp: the public API's origins, reached over TLS instead of the socket
	client  *http.Client
	Timeout time.Duration
	Agent   string
}

// NewRemote reaches the public API (pperp mcp, on the user's computer): origins by environment.
func NewRemote(origins map[string]string, client *http.Client, agent string) *Gateway {
	return &Gateway{origins: origins, client: client, Timeout: 15 * time.Second, Agent: agent}
}

func (g *Gateway) origin(env string) string {
	if o := g.origins[env]; o != "" {
		return o
	}
	return "http://" + g.Hosts[env]
}

// NewGateway reaches the gateway at a local socket path ("" or an http:// URL in tests: plain TCP).
func NewGateway(socket string, hosts map[string]string, agent string) *Gateway {
	tr := &http.Transport{MaxIdleConns: 64, IdleConnTimeout: 90 * time.Second}
	if socket != "" && !strings.HasPrefix(socket, "http") {
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
	}
	return &Gateway{Hosts: hosts, client: &http.Client{Transport: tr}, Timeout: 15 * time.Second, Agent: agent}
}

// Answer is what the API answered.
type Answer struct {
	Status     int
	Header     http.Header
	Body       []byte
	TimedOut   bool
	RetryAfter *int
}

// Call sends one operation with its arguments split back into path, query and body.
func (g *Gateway) Call(ctx context.Context, env, token, clientIP string, op ops.Op, args map[string]any) (*Answer, error) {
	path := op.Path
	for _, p := range op.PathParams {
		v, _ := args[p].(string)
		if v == "" {
			return nil, errors.New(p + " is required")
		}
		path = strings.Replace(path, "{"+p+"}", url.PathEscape(v), 1)
	}
	q := url.Values{}
	for _, name := range op.Query {
		if v, ok := args[name]; ok && v != nil {
			q.Set(name, scalar(v))
		}
	}
	var body io.Reader
	if op.Body {
		b := map[string]any{}
		for _, f := range op.BodyFields {
			if v, ok := args[f]; ok {
				b[f] = v
			}
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	u := g.origin(env) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	key, _ := args["idempotency_key"].(string)
	return g.Do(ctx, env, op.HTTP, u, token, clientIP, key, body)
}

// Do sends one request to the gateway.
func (g *Gateway) Do(ctx context.Context, env, method, u, token, clientIP, idempotencyKey string, body io.Reader) (*Answer, error) {
	actx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Host = g.Hosts[env]
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", g.Agent)
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	res, err := g.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return &Answer{TimedOut: true}, nil
		}
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return &Answer{TimedOut: true}, nil
		}
		return nil, err
	}
	a := &Answer{Status: res.StatusCode, Header: res.Header, Body: b}
	if s := res.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			a.RetryAfter = &n
		}
	}
	return a, nil
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(x)
		return strings.Trim(string(b), `"`)
	}
}
