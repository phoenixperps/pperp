package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	pp "github.com/phoenixperps/sdk/go"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

/*
pperp login (design 15 section 3 "Login", as reconciled in PLAN.md): "Sign in with Phoenix Perpetuals", OAuth 2.1
with PKCE S256 for the first-party app pperp-cli, the resource being the environment's REST host. The browser comes
back to http://127.0.0.1:<port>/callback (RFC 8252), state and iss checked; --manual uses the site's code page instead
(PKCE makes a code useless to anyone else). The access token (ppo_) is kept in the system keychain. A sandbox key can
be kept instead (--api-key); a live key never is: on live, sign in, or set PHOENIXPERPS_API_KEY for one call.
*/

const (
	clientID     = "pperp-cli"
	authOrigin   = "https://phoenixperpsfunding.com"
	loginTimeout = 5 * time.Minute
)

var restOrigins = map[string]string{"live": "https://api.phoenixperpsfunding.com", "sandbox": "https://sandbox-api.phoenixperpsfunding.com"}

// pkce is a new code verifier and its S256 challenge (RFC 7636).
func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	return verifier, challengeOf(verifier)
}

func challengeOf(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func randomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *App) authServer() (string, error) {
	o := a.Getenv("PPERP_AUTH_URL")
	if o == "" {
		return authOrigin, nil
	}
	o = strings.TrimRight(o, "/")
	return o, checkBaseURL(o)
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	}
	return exec.Command("xdg-open", u).Start()
}

// readSecret reads a secret without showing it on a terminal.
func (a *App) readSecret(prompt string) (string, error) {
	if f, ok := a.In.(*os.File); ok && a.InTTY {
		fmt.Fprint(a.Err, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.Err)
		return strings.TrimSpace(string(b)), err
	}
	return a.line()
}

func (a *App) addStart(root *cobra.Command) {
	var f struct {
		env, store                              string
		readOnly, manual, apiKey, keyStdin, nob bool
	}
	login := &cobra.Command{
		Use:     "login",
		Short:   "Sign in (a browser opens; --sandbox for the sandbox)",
		GroupID: "start",
		Example: `  pperp login                     sign in to live
  pperp login --sandbox           sign in to the sandbox
  pperp login --sandbox --api-key keep a sandbox key instead (asked for, never shown)
  pperp login --manual            no browser here: open the address anywhere and paste the code`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name := a.profileName()
			p := a.profile(name)
			if p == nil {
				if f.env != "live" && f.env != "sandbox" {
					return usage("a new profile needs --env live or --env sandbox")
				}
				p = &Profile{Env: f.env}
			} else if f.env != "" && f.env != p.Env {
				return usage("profile %s is %s, not %s", name, p.Env, f.env)
			}
			if f.store != "" {
				if f.store != "keychain" && f.store != "file" {
					return usage("--store keychain or --store file")
				}
				p.Store = f.store
				if f.store == "file" {
					a.note("The credential will be kept in %s (readable by you only), not in the system keychain.", a.credentialsFile())
				}
			}
			var cred *Credential
			var err error
			if f.apiKey || f.keyStdin {
				cred, err = a.keyLogin(p, f.keyStdin)
			} else {
				cred, err = a.oauthLogin(cmd.Context(), name, p, f.readOnly, f.manual, f.nob)
			}
			if err != nil {
				return err
			}
			if err := a.saveCredential(name, p, cred); err != nil {
				return err
			}
			a.keepProfile(name, p)
			if a.cfg.Profile == "" {
				a.cfg.Profile = name
			}
			if err := a.saveConfig(); err != nil {
				return err
			}
			if err := a.welcome(cmd.Context(), name); err != nil {
				var pe *pp.Error
				if errors.As(err, &pe) && pe.Kind == pp.KindAuthentication { // refused: not kept
					_ = a.deleteCredential(name, p)
				}
				return err
			}
			return nil
		},
	}
	fl := login.Flags()
	fl.BoolVar(&f.readOnly, "read-only", false, "ask for read access only (no trading)")
	fl.BoolVar(&f.manual, "manual", false, "no browser on this computer: sign in anywhere and paste the code shown")
	fl.BoolVar(&f.nob, "no-browser", false, "print the sign-in address instead of opening a browser")
	fl.BoolVar(&f.apiKey, "api-key", false, "keep a sandbox API key instead (asked for, never shown)")
	fl.BoolVar(&f.keyStdin, "api-key-stdin", false, "keep a sandbox API key read from stdin (for scripts)")
	fl.StringVar(&f.env, "env", "", "a new profile's environment: live or sandbox")
	fl.StringVar(&f.store, "store", "", "where the credential is kept: keychain (the default) or file")

	logout := &cobra.Command{
		Use: "logout", Short: "Sign out: end this computer's sign-in and forget it", GroupID: "start", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name := a.profileName()
			p := a.profile(name)
			if p == nil {
				return usage("there is no profile %q", name)
			}
			cred, err := a.loadCredential(name, p)
			if err != nil {
				return err
			}
			if cred == nil {
				a.note("Profile %s is not signed in.", name)
				return nil
			}
			if cred.Kind == "app" {
				if err := a.revoke(cmd.Context(), cred.Token); err != nil {
					a.note("The sign-in could not be ended on the server (%v): end it in the dashboard (Developers, Apps).", err)
				}
			}
			if err := a.deleteCredential(name, p); err != nil {
				return err
			}
			if cred.Kind == "key" {
				a.note("Profile %s no longer has the key. The key itself still works: revoke it in the dashboard (Developers, API keys) if no one needs it.", name)
				return nil
			}
			a.note("Signed out of profile %s.", name)
			return nil
		},
	}
	root.AddCommand(login, logout)
}

// keyLogin takes a sandbox API key, checked with the API before it is kept.
func (a *App) keyLogin(p *Profile, fromStdin bool) (*Credential, error) {
	if p.Env == "live" {
		return nil, usage("on live, sign in through the browser (pperp login): pperp does not keep live keys. On a server, set PHOENIXPERPS_API_KEY")
	}
	var key string
	var err error
	if fromStdin {
		b, rerr := io.ReadAll(io.LimitReader(a.reader, 4096))
		key, err = strings.TrimSpace(string(b)), rerr
	} else {
		if !a.InTTY {
			return nil, usage("no terminal to ask for the key: use --api-key-stdin")
		}
		key, err = a.readSecret("Sandbox API key (ppk_test_...): ")
	}
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(key, "ppk_test_") {
		return nil, usage("that is not a sandbox API key (they start with ppk_test_)")
	}
	return &Credential{Token: key, Kind: "key"}, nil
}

// oauthLogin signs in through the browser and answers the access token.
func (a *App) oauthLogin(ctx context.Context, name string, p *Profile, readOnly, manual, noBrowser bool) (*Credential, error) {
	auth, err := a.authServer()
	if err != nil {
		return nil, err
	}
	resource := restOrigins[p.Env]
	if base := firstOf(a.Getenv("PHOENIXPERPS_BASE_URL"), p.BaseURL); base != "" {
		if err := checkBaseURL(base); err != nil {
			return nil, err
		}
		resource = strings.TrimRight(base, "/")
	}
	verifier, challenge := pkce()
	state := randomState()
	scope := "read trade"
	if readOnly {
		scope = "read"
	}
	device, _ := os.Hostname()
	var ln net.Listener
	redirect := auth + "/oauth/manual"
	if !manual {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, fmt.Errorf("no local port for the browser to come back to (%v): use --manual", err)
		}
		defer ln.Close()
		redirect = fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	}
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "scope": {scope},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state}, "resource": {resource}, "device_name": {device},
	}
	address := auth + "/oauth/authorize?" + q.Encode()
	var code string
	if manual {
		a.note("Open this address on any device, sign in, approve, and paste the code it shows:\n\n  %s\n", address)
		if !a.InTTY {
			return nil, usage("--manual needs a terminal to paste the code into")
		}
		fmt.Fprint(a.Err, "Code: ")
		if code, err = a.line(); err != nil || code == "" {
			return nil, &exitError{ExitAuth, "No code: not signed in."}
		}
	} else {
		a.note("Signing in to profile %s (%s) in your browser. If it does not open, open:\n\n  %s\n", name, p.Env, address)
		if !noBrowser && a.Browser != nil {
			_ = a.Browser(address)
		}
		if code, err = a.awaitCallback(ctx, ln, state, auth); err != nil {
			return nil, err
		}
	}
	return a.exchange(ctx, auth, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "client_id": {clientID},
		"code_verifier": {verifier}, "resource": {resource},
	})
}

// awaitCallback is the browser's return to the loopback address: its code, once state and issuer are right.
func (a *App) awaitCallback(ctx context.Context, ln net.Listener, state, issuer string) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	page := func(w http.ResponseWriter, status int, title, text string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>pperp</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 16px;color:#1d2433">
<h1 style="font-size:1.4rem">%s</h1><p>%s</p></body>`, html.EscapeString(title), html.EscapeString(text))
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		v := r.URL.Query()
		var res result
		switch {
		case v.Get("state") != state:
			page(w, 400, "Not signed in", "This answer was not for this sign-in. Run pperp login again.")
			return // a stray request: keep waiting for the right one
		case v.Get("iss") != issuer:
			res.err = &exitError{ExitAuth, "The sign-in answer came from another issuer: not signed in."}
			page(w, 400, "Not signed in", "The answer came from somewhere else. Run pperp login again.")
		case v.Get("error") != "":
			msg := firstOf(v.Get("error_description"), v.Get("error"))
			res.err = &exitError{ExitAuth, "Not signed in: " + msg}
			page(w, 400, "Not signed in", msg)
		case v.Get("code") == "":
			res.err = &exitError{ExitAuth, "The sign-in answer had no code: not signed in."}
			page(w, 400, "Not signed in", "No code came back. Run pperp login again.")
		default:
			res.code = v.Get("code")
			page(w, 200, "Signed in", "You can close this tab and go back to the terminal.")
		}
		select {
		case done <- res:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	select {
	case r := <-done:
		return r.code, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", &exitError{ExitAuth, "No answer from the browser in 5 minutes: not signed in."}
		}
		return "", ctx.Err()
	}
}

// exchange trades the code for the access token.
func (a *App) exchange(ctx context.Context, auth string, form url.Values) (*Credential, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	status, err := a.postForm(ctx, auth+"/oauth/token", form, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || out.AccessToken == "" {
		return nil, &exitError{ExitAuth, "Not signed in: " + firstOf(out.Description, firstOf(out.Error, fmt.Sprintf("HTTP %d", status)))}
	}
	c := &Credential{Token: out.AccessToken, Kind: "app", Scope: out.Scope}
	if out.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UnixMilli()
	}
	return c, nil
}

// revoke ends an app sign-in on the server (RFC 7009).
func (a *App) revoke(ctx context.Context, token string) error {
	auth, err := a.authServer()
	if err != nil {
		return err
	}
	status, err := a.postForm(ctx, auth+"/oauth/revoke", url.Values{"token": {token}, "client_id": {clientID}}, nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("HTTP %d", status)
	}
	return err
}

func (a *App) postForm(ctx context.Context, u string, form url.Values, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pperp/"+a.Version)
	res, err := a.httpClient().Do(req)
	if err != nil {
		return 0, &pp.Error{Kind: pp.KindConnection, Code: "connection_failed", Message: "The sign-in server could not be reached: " + err.Error()}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	return res.StatusCode, nil
}

// welcome says who signed in, from the API itself.
func (a *App) welcome(ctx context.Context, name string) error {
	k, err := a.connect()
	if err != nil {
		return err
	}
	me, err := a.get(ctx, k, "/v1/me", nil)
	if err != nil {
		return err
	}
	accounts, _ := lookupAny(me, "accounts").([]any)
	a.note("Signed in on profile %s (%s): %s access, %d account(s). pperp accounts lists them.", name, k.env, str(me, "credential.access"), len(accounts))
	return nil
}
