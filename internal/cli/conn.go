package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	pp "github.com/phoenixperps/sdk/go"

	"github.com/phoenixperps/pperp/internal/output"
)

// conn is the API as this run reaches it: the profile, its credential and the SDK client.
type conn struct {
	name    string
	p       *Profile
	env     string
	cred    *Credential
	fromEnv bool // PHOENIXPERPS_API_KEY
	c       *pp.Client
}

func loginHint(name string) string {
	switch name {
	case "live":
		return "pperp login"
	case "sandbox":
		return "pperp login --sandbox"
	}
	return "pperp login --profile " + name
}

// connect opens the API for this run's profile (PHOENIXPERPS_API_KEY first, else the profile's credential).
func (a *App) connect() (*conn, error) {
	name := a.profileName()
	p := a.profile(name)
	if p == nil {
		return nil, usage("there is no profile %q: pperp profiles lists them, %s makes it", name, loginHint(name))
	}
	k := &conn{name: name, p: p, env: p.Env}
	if key := a.Getenv("PHOENIXPERPS_API_KEY"); key != "" {
		k.fromEnv, k.cred = true, &Credential{Token: key, Kind: "key"}
	} else {
		cred, err := a.loadCredential(name, p)
		if err != nil {
			return nil, err
		}
		if cred == nil {
			return nil, &exitError{ExitAuth, fmt.Sprintf("Not signed in on profile %s: %s", name, loginHint(name))}
		}
		if cred.ExpiresAt > 0 && time.Now().UnixMilli() >= cred.ExpiresAt {
			return nil, &exitError{ExitAuth, fmt.Sprintf("The sign-in of profile %s has ended: %s", name, loginHint(name))}
		}
		k.cred = cred
	}
	opts := []pp.ClientOption{pp.WithAPIKey(k.cred.Token), pp.WithAppInfo("pperp/" + a.Version), pp.WithTimeout(a.flags.timeout), pp.WithHTTPClient(a.httpClient())}
	// a key from the environment is of its own environment, unless a profile was asked for
	if !k.fromEnv || a.flags.profile != "" || a.flags.sandbox {
		opts = append(opts, pp.WithEnvironment(p.Env))
	}
	base := firstOf(a.Getenv("PHOENIXPERPS_BASE_URL"), p.BaseURL)
	if base != "" {
		if err := checkBaseURL(base); err != nil {
			return nil, err
		}
		base = strings.TrimRight(base, "/")
		account, market := wsOrigin(base)+"/v1/stream", ""
		if loopback(base) { // a test API on this computer serves both streams
			market = wsOrigin(base) + "/v1/market-data"
		}
		opts = append(opts, pp.WithBaseURL(base), pp.WithStreamURLs(account, market))
	}
	c, err := pp.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	k.c, k.env = c, c.Environment()
	if k.env == "sandbox" && a.OutTTY {
		a.note("[sandbox]")
	}
	return k, nil
}

func wsOrigin(base string) string {
	if strings.HasPrefix(base, "http://") {
		return "ws://" + strings.TrimPrefix(base, "http://")
	}
	return "wss://" + strings.TrimPrefix(base, "https://")
}

func loopback(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}

func (a *App) httpClient() *http.Client {
	h := a.HTTP
	if h == nil {
		h = &http.Client{}
	}
	if !a.flags.verbose {
		return h
	}
	base := h.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	v := *h
	v.Transport = verbose{base: base, w: a.Err}
	return &v
}

// verbose prints each request and answer line, the credential cut to its prefix and last 4 characters.
type verbose struct {
	base http.RoundTripper
	w    io.Writer
}

func (v verbose) RoundTrip(r *http.Request) (*http.Response, error) {
	fmt.Fprintf(v.w, "> %s %s\n", r.Method, r.URL.Redacted())
	if t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); t != "" {
		fmt.Fprintf(v.w, "> Authorization: Bearer %s\n", maskToken(t))
	}
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		fmt.Fprintf(v.w, "> Idempotency-Key: %s\n", k)
	}
	start := time.Now()
	res, err := v.base.RoundTrip(r)
	if err != nil {
		fmt.Fprintf(v.w, "< %v\n", err)
		return nil, err
	}
	fmt.Fprintf(v.w, "< %s in %s %s\n", res.Status, time.Since(start).Round(time.Millisecond), res.Header.Get("X-Request-Id"))
	return res, nil
}

// maskToken is a credential as it may be shown: its prefix and its last 4 characters.
func maskToken(t string) string {
	cut := 0
	if i := strings.LastIndex(t, "_"); i >= 0 && i < len(t)-1 {
		cut = i + 1
	}
	if len(t)-cut <= 4 {
		return t[:cut] + "****"
	}
	return t[:cut] + "****" + t[len(t)-4:]
}

// call sends one request and answers its body decoded (nil for an empty one).
func (a *App) call(ctx context.Context, k *conn, method, path string, query url.Values, body any, idempotencyKey string) (*pp.RawResponse, any, error) {
	var raw pp.RawResponse
	opts := []pp.CallOption{pp.WithRawResponse(&raw)}
	if idempotencyKey != "" {
		opts = append(opts, pp.WithIdempotencyKey(idempotencyKey))
	}
	res, err := k.c.Do(ctx, method, path, query, body, opts...)
	if a.flags.include && raw.StatusCode != 0 {
		a.headers(&raw)
	}
	if err != nil {
		return &raw, nil, err
	}
	if len(bytes.TrimSpace(res.Body)) == 0 {
		return res, nil, nil
	}
	v, err := output.Decode(res.Body)
	if err != nil {
		return res, nil, fmt.Errorf("the answer could not be read: %w", err)
	}
	return res, v, nil
}

// get is a GET answering the answer's data.
func (a *App) get(ctx context.Context, k *conn, path string, query url.Values) (any, error) {
	_, v, err := a.call(ctx, k, http.MethodGet, path, query, nil, "")
	if err != nil {
		return nil, err
	}
	return data(v), nil
}

func (a *App) headers(r *pp.RawResponse) {
	fmt.Fprintf(a.Err, "HTTP %d\n", r.StatusCode)
	for _, h := range []string{"X-Request-Id", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After", "Location", "Deprecation", "Sunset"} {
		if v := r.Header.Get(h); v != "" {
			fmt.Fprintf(a.Err, "%s: %s\n", h, v)
		}
	}
	if r.IdempotencyKey != "" {
		fmt.Fprintf(a.Err, "Idempotency-Key: %s\n", r.IdempotencyKey)
	}
}

// data is an answer's data ({"data": ...}), else the answer.
func data(v any) any {
	if o, ok := v.(*output.Object); ok {
		if d, ok := o.Vals["data"]; ok {
			return d
		}
	}
	return v
}

// str is a field of an answer as text ("" when absent or null).
func str(v any, path string) string {
	o, ok := v.(*output.Object)
	if !ok {
		return ""
	}
	x, _ := o.Get(path)
	switch s := x.(type) {
	case nil:
		return ""
	case string:
		return s
	case *output.Object, []any:
		return output.Compact(s)
	default:
		return fmt.Sprint(s)
	}
}

// account is the account of this run: --account, else the profile's default, else the only one there is.
func (a *App) account(ctx context.Context, k *conn) (string, error) {
	if a.flags.account != "" {
		return strings.ToUpper(strings.TrimSpace(a.flags.account)), nil
	}
	if k.p.DefaultAccount != "" && !k.fromEnv {
		return k.p.DefaultAccount, nil
	}
	list, err := a.get(ctx, k, "/v1/accounts", nil)
	if err != nil {
		return "", err
	}
	items, _ := list.([]any)
	var all, open []string
	for _, it := range items {
		id := str(it, "id")
		all = append(all, id)
		if s := str(it, "status"); s == "active" || s == "pending" {
			open = append(open, id)
		}
	}
	switch {
	case len(all) == 1:
		return all[0], nil
	case len(open) == 1:
		return open[0], nil
	case len(all) == 0:
		return "", &exitError{ExitNotFound, "This credential reaches no account."}
	}
	return "", usage("%d accounts: pick one with --account, or make one the default with pperp use ACCOUNT\n  %s", len(all), strings.Join(all, "\n  "))
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
