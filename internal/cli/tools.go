package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	pp "github.com/phoenixperps/sdk/go"
	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"

	"github.com/phoenixperps/pperp/internal/mcp"
	"github.com/phoenixperps/pperp/internal/output"
)

func (a *App) addTools(root *cobra.Command) {
	root.AddCommand(a.profilesCommand(), a.configCommand(), a.doctorCommand(), a.versionCommand(), a.upgradeCommand(), a.apiCommand(), a.mcpCommand())
}

func (a *App) profilesCommand() *cobra.Command {
	profiles := &cobra.Command{
		Use: "profiles", Short: "Your profiles: which is the default, which are signed in", GroupID: "tools", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			names := []string{"live", "sandbox"}
			for n := range a.cfg.Profiles {
				if n != "live" && n != "sandbox" {
					names = append(names, n)
				}
			}
			sort.Strings(names[2:])
			var rows []any
			for _, n := range names {
				p := a.profile(n)
				signed := "no"
				if c, err := a.loadCredential(n, p); err != nil {
					signed = "unknown (keychain)"
				} else if c != nil {
					signed = map[string]string{"app": "signed in", "key": "API key"}[c.Kind]
					if c.ExpiresAt > 0 {
						if c.ExpiresAt <= time.Now().UnixMilli() {
							signed = "ended"
						} else {
							signed += ", until " + time.UnixMilli(c.ExpiresAt).Format("2006-01-02")
						}
					}
				}
				def := ""
				if n == a.profileName() {
					def = "*"
				}
				rows = append(rows, &output.Object{Keys: []string{"default", "profile", "env", "credential", "default_account", "confirm_trades"},
					Vals: map[string]any{"default": def, "profile": n, "env": p.Env, "credential": signed, "default_account": p.DefaultAccount, "confirm_trades": p.confirms()}})
			}
			return a.show("profiles", rows)
		},
	}
	use := &cobra.Command{
		Use: "use <profile>", Short: "Make a profile the default",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usage("pperp profiles use PROFILE")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if a.profile(args[0]) == nil {
				return usage("there is no profile %q", args[0])
			}
			a.cfg.Profile = args[0]
			if err := a.saveConfig(); err != nil {
				return err
			}
			a.note("The default profile is now %s.", args[0])
			return nil
		},
	}
	remove := &cobra.Command{
		Use: "remove <profile>", Short: "Sign a profile out and forget its settings",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usage("pperp profiles remove PROFILE")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			n := args[0]
			p := a.profile(n)
			if p == nil {
				return usage("there is no profile %q", n)
			}
			if c, _ := a.loadCredential(n, p); c != nil && c.Kind == "app" {
				_ = a.revoke(cmd.Context(), c.Token)
			}
			if err := a.deleteCredential(n, p); err != nil {
				return err
			}
			delete(a.cfg.Profiles, n)
			if a.cfg.Profile == n {
				a.cfg.Profile = ""
			}
			if err := a.saveConfig(); err != nil {
				return err
			}
			a.note("Profile %s removed.", n)
			return nil
		},
	}
	profiles.AddCommand(use, remove)
	return profiles
}

var configKeys = map[string]string{
	"default_account": "the account used when --account is not given",
	"output":          "table, json, jsonl or csv (empty: table on a terminal, json when piped)",
	"confirm_trades":  "true: trades are shown and asked about before they are sent (default: true on live)",
	"base_url":        "the API's address (https://*.phoenixperpsfunding.com or this computer; empty: the environment's)",
}

func (a *App) configCommand() *cobra.Command {
	config := &cobra.Command{
		Use: "config", Short: "The profile's settings (config get/set KEY)", GroupID: "tools", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			p := a.currentProfile()
			if p == nil {
				return usage("there is no profile %q", a.profileName())
			}
			ct := strconv.FormatBool(p.confirms())
			return a.show("config", &output.Object{Keys: []string{"profile", "env", "default_account", "output", "confirm_trades", "base_url", "store"},
				Vals: map[string]any{"profile": a.profileName(), "env": p.Env, "default_account": p.DefaultAccount, "output": p.Output, "confirm_trades": ct, "base_url": p.BaseURL, "store": firstOf(p.Store, "keychain")}})
		},
	}
	keys := make([]string, 0, len(configKeys))
	for k := range configKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var help strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&help, "\n  %-16s %s", k, configKeys[k])
	}
	get := &cobra.Command{
		Use: "get <key>", Short: "One setting of the profile", Long: "One setting of the profile:" + help.String(),
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || configKeys[args[0]] == "" {
				return usage("pperp config get KEY, KEY one of %s", strings.Join(keys, ", "))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			p := a.currentProfile()
			if p == nil {
				return usage("there is no profile %q", a.profileName())
			}
			v := map[string]string{"default_account": p.DefaultAccount, "output": p.Output, "confirm_trades": strconv.FormatBool(p.confirms()), "base_url": p.BaseURL}[args[0]]
			fmt.Fprintln(a.Out, v)
			return nil
		},
	}
	set := &cobra.Command{
		Use: "set <key> <value>", Short: "Change one setting of the profile (\"\" clears it)", Long: "Change one setting of the profile:" + help.String(),
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 2 || configKeys[args[0]] == "" {
				return usage("pperp config set KEY VALUE, KEY one of %s", strings.Join(keys, ", "))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			name := a.profileName()
			p := a.profile(name)
			if p == nil {
				return usage("there is no profile %q", name)
			}
			v := strings.TrimSpace(args[1])
			switch args[0] {
			case "default_account":
				p.DefaultAccount = strings.ToUpper(v)
			case "output":
				if v != "" && !slices.Contains(output.Formats, v) {
					return usage("output: table, json, jsonl or csv")
				}
				p.Output = v
			case "confirm_trades":
				if v == "" {
					p.ConfirmTrades = nil
					break
				}
				b, err := strconv.ParseBool(v)
				if err != nil {
					return usage("confirm_trades: true or false")
				}
				p.ConfirmTrades = &b
			case "base_url":
				if v != "" {
					if err := checkBaseURL(v); err != nil {
						return err
					}
				}
				p.BaseURL = strings.TrimRight(v, "/")
			}
			a.keepProfile(name, p)
			return a.saveConfig()
		},
	}
	config.AddCommand(get, set)
	return config
}

func (a *App) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "pperp's version", GroupID: "tools", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintf(a.Out, "pperp %s (%s %s/%s), Go SDK %s, API spec %s\n", a.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH, pp.SDKVersion, pp.SpecVersion)
			return nil
		},
	}
}

// doctor checks what pperp needs, one line each: ok, warn or fail.
func (a *App) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "Check this computer: settings, keychain, sign-in, the API, the clock", GroupID: "tools", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			failed := false
			say := func(state, format string, args ...any) {
				if state == "fail" {
					failed = true
				}
				fmt.Fprintf(a.Out, "%-5s %s\n", state, fmt.Sprintf(format, args...))
			}
			say("ok", "pperp %s on %s/%s", a.Version, runtime.GOOS, runtime.GOARCH)
			if err := os.MkdirAll(a.cfgDir, 0o700); err != nil {
				say("fail", "settings folder %s: %v", a.cfgDir, err)
			} else {
				say("ok", "settings in %s", a.cfgDir)
			}
			name := a.profileName()
			p := a.profile(name)
			if p == nil {
				say("fail", "profile %s does not exist", name)
				return &exitError{ExitFailed, ""}
			}
			if p.Store != "file" {
				if _, err := keyring.Get(keychainService, "doctor:check"); err != nil && !errors.Is(err, keyring.ErrNotFound) {
					say("fail", "the system keychain does not answer (%v): sign in with --store file, or set PHOENIXPERPS_API_KEY", err)
				} else {
					say("ok", "system keychain")
				}
			}
			if a.Getenv("PHOENIXPERPS_API_KEY") != "" {
				say("ok", "PHOENIXPERPS_API_KEY is set: it is used instead of the profile's credential")
			} else if c, err := a.loadCredential(name, p); err != nil || c == nil {
				say("fail", "profile %s (%s) is not signed in: %s", name, p.Env, loginHint(name))
			} else if c.ExpiresAt > 0 {
				left := time.Until(time.UnixMilli(c.ExpiresAt))
				switch {
				case left <= 0:
					say("fail", "the sign-in of profile %s has ended: %s", name, loginHint(name))
				case left < 7*24*time.Hour:
					say("warn", "the sign-in of profile %s ends in %d day(s)", name, int(left.Hours()/24)+1)
				default:
					say("ok", "profile %s (%s) signed in until %s", name, p.Env, time.UnixMilli(c.ExpiresAt).Format("2006-01-02"))
				}
			} else {
				say("ok", "profile %s (%s) has an API key", name, p.Env)
			}
			k, err := a.connect()
			if err != nil {
				say("fail", "%v", err)
				return &exitError{ExitFailed, ""}
			}
			start := time.Now()
			platform, err := a.get(ctx, k, "/v1", nil)
			took := time.Since(start)
			if err != nil {
				say("fail", "%s does not answer: %v", k.c.BaseURL(), err)
				return &exitError{ExitFailed, ""}
			}
			say("ok", "%s answers in %s (API %s)", k.c.BaseURL(), took.Round(time.Millisecond), str(platform, "version"))
			if st, err := strconv.ParseInt(str(platform, "server_time"), 10, 64); err == nil {
				skew := time.Since(time.UnixMilli(st)) - took/2
				if skew < 0 {
					skew = -skew
				}
				if skew > 5*time.Second {
					say("warn", "this computer's clock is %s off the API's: times and expiries may look wrong", skew.Round(time.Second))
				} else {
					say("ok", "clock within %s of the API's", skew.Round(10*time.Millisecond))
				}
			}
			if me, err := a.get(ctx, k, "/v1/me", nil); err != nil {
				say("fail", "the API refuses the credential: %v", err)
			} else {
				say("ok", "signed in: %s %s, %s access", str(me, "credential.kind"), str(me, "credential.name"), str(me, "credential.access"))
			}
			if failed {
				return &exitError{ExitFailed, ""}
			}
			return nil
		},
	}
}

// apiCommand is a raw call to any API path of the profile's host (gh api's way): for operations newer than pperp.
func (a *App) apiCommand() *cobra.Command {
	var dataSpec string
	var fields []string
	cmd := &cobra.Command{
		Use: "api <method> <path>", Short: "Call any API path of the profile's host (newer operations)", GroupID: "tools",
		Example: `  pperp api GET /v1/limits
  pperp api GET /v1/accounts/SANDBOX-00042-001/orders -f status=history -f limit=10
  pperp api POST /v1/accounts/SANDBOX-00042-001/orders/test --data @order.json`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 2 || !strings.HasPrefix(args[1], "/v1") {
				return usage("pperp api METHOD /v1/PATH")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := a.connect()
			if err != nil {
				return err
			}
			method := strings.ToUpper(args[0])
			path, rawQuery, _ := strings.Cut(args[1], "?")
			q, err := url.ParseQuery(rawQuery)
			if err != nil {
				return usage("the path's query: %v", err)
			}
			for _, f := range fields {
				name, v, ok := strings.Cut(f, "=")
				if !ok {
					return usage("-f takes name=value, not %q", f)
				}
				q.Add(name, v)
			}
			var body any
			if dataSpec != "" {
				if body, err = a.readData(dataSpec); err != nil {
					return err
				}
			}
			key := a.flags.idempotencyKey
			if key == "" && method != http.MethodGet && method != http.MethodHead {
				key = newKey()
			}
			_, v, err := a.call(cmd.Context(), k, method, path, q, body, key)
			if err != nil {
				return err
			}
			return a.show("", v)
		},
	}
	cmd.Flags().StringVar(&dataSpec, "data", "", "the JSON body: @file.json, - for stdin, or the JSON itself")
	cmd.Flags().StringArrayVarP(&fields, "field", "f", nil, "a query parameter, name=value")
	return cmd
}

// mcpCommand serves the MCP tools over stdio for an assistant on this computer, with the profile's credential.
func (a *App) mcpCommand() *cobra.Command {
	var allowTrading bool
	cmd := &cobra.Command{
		Use: "mcp", Short: "An MCP server on stdio for an AI assistant on this computer (read only unless --allow-trading)", GroupID: "tools", Args: noArgs,
		Long: `Serves the Phoenix Perpetuals tools (the hosted MCP server's, mcp.phoenixperpsfunding.com) over stdio, with this
profile's credential: read only unless --allow-trading. Trades are previewed first when the profile confirms trades
(confirm_trades, on by default for live). The hosted server is the one to use from web assistants.

  claude mcp add phoenix-perps -- pperp mcp --sandbox`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			k, err := a.connect()
			if err != nil {
				return err
			}
			gw := mcp.NewRemote(map[string]string{k.env: k.c.BaseURL()}, a.httpClient(), "pperp/"+a.Version)
			srv, err := mcp.Local(cmd.Context(), gw, k.env, k.cred.Token, a.Version, allowTrading, k.p.confirms())
			if err != nil {
				return &exitError{ExitAuth, err.Error()}
			}
			if err := srv.Run(cmd.Context(), &sdkmcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowTrading, "allow-trading", false, "let the assistant trade (with a trade credential)")
	return cmd
}
