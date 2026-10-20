// Package cli is pperp, the Phoenix Perpetuals command line (docs/api/design/15-cli-mcp.md section 3, as reconciled in
// docs/api/PLAN.md "SDKs, CLI, MCP"): one command per API operation (internal/ops, the table the MCP server's tools
// come from too) and the friendly ones (accounts, positions, buy, sell, close, flatten, stream...), on the Go SDK.
//
// Exit codes: 0 done, 1 failed or declined, 2 usage, 3 not signed in or not allowed (401, 403), 4 not found, 5 conflict
// (409), 6 refused by a rule (422), 7 rate limited, 8 server (5xx), 9 network or unknown outcome.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	pp "github.com/phoenixperps/sdk/go"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/phoenixperps/pperp/internal/output"
	"github.com/phoenixperps/pperp/internal/release"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitFailed   = 1
	ExitUsage    = 2
	ExitAuth     = 3
	ExitNotFound = 4
	ExitConflict = 5
	ExitRule     = 6
	ExitRate     = 7
	ExitServer   = 8
	ExitNetwork  = 9
)

// App is one run of pperp: its streams, its environment and its flags.
type App struct {
	Version string
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Getenv  func(string) string
	OutTTY  bool // stdout is a terminal: tables, colours, prompts' answers shown
	InTTY   bool // stdin is a terminal: questions may be asked
	Browser func(url string) error
	HTTP    *http.Client // the API's transport (nil: Go's default)

	flags struct {
		profile, output, jq, account, idempotencyKey string
		sandbox, yes, include, verbose, allPages     bool
		maxPages                                     int
		timeout                                      time.Duration
	}
	cfg     *Config
	cfgDir  string
	reader  *bufio.Reader
	printer *output.Printer
	after   func() // once the command is done (the update notice)

	releaseKeys []release.PublicKey // tests: the keys releases are checked with (else the built-in ones)
}

// Main runs pperp with the process's arguments and streams.
func Main(version string) int {
	a := &App{
		Version: version, In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv,
		OutTTY: term.IsTerminal(int(os.Stdout.Fd())), InTTY: term.IsTerminal(int(os.Stdin.Fd())), Browser: openBrowser,
	}
	if exe, err := os.Executable(); err == nil {
		release.Cleanup(exe) // what an upgrade on Windows left
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return a.Run(ctx, os.Args[1:])
}

// usageError is a command used wrongly (exit 2).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usage(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

// exitError ends a command with its own code and words.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// Run runs one command line.
func (a *App) Run(ctx context.Context, args []string) int {
	a.reader = bufio.NewReader(a.In)
	root := a.root()
	root.SetArgs(args)
	root.SetIn(a.In)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	err := root.ExecuteContext(ctx)
	if a.after != nil {
		a.after()
	}
	if err == nil {
		return ExitOK
	}
	return a.fail(err)
}

func (a *App) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "pperp",
		Short: "Phoenix Perpetuals from your terminal",
		Long: `pperp: your Phoenix Perpetuals accounts from the terminal: positions, orders, fills, live risk, trading and the
live streams, plus one command for every API operation.

  pperp login --sandbox        sign in to the sandbox (pperp login for live)
  pperp accounts               your accounts
  pperp buy BTC-USD 0.01       a market order (--limit, --stop, --tp, --sl, --dry-run)
  pperp stream account         every event of your accounts, live

Docs: https://docs.phoenixperpsfunding.com/cli`,
		Version:           a.Version,
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: false},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return a.prepare(cmd) },
	}
	root.SetVersionTemplate("pperp {{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err.Error()} })
	f := root.PersistentFlags()
	f.StringVar(&a.flags.profile, "profile", "", "the profile to use (live, sandbox or one of yours; else PPERP_PROFILE, else the default)")
	f.BoolVar(&a.flags.sandbox, "sandbox", false, "the sandbox profile (--profile sandbox)")
	f.StringVarP(&a.flags.output, "output", "o", "", "table, json, jsonl or csv (default: table on a terminal, json when piped)")
	f.StringVar(&a.flags.jq, "jq", "", "a jq expression applied to the answer")
	f.StringVarP(&a.flags.account, "account", "a", "", "the account (else the profile's default, else your only one)")
	f.BoolVarP(&a.flags.yes, "yes", "y", false, "do it without asking (needed for live trades when no one can be asked)")
	f.BoolVar(&a.flags.include, "include", false, "print the status and rate-limit headers to stderr")
	f.BoolVar(&a.flags.verbose, "verbose", false, "print each request and answer line to stderr (the credential hidden)")
	f.StringVar(&a.flags.idempotencyKey, "idempotency-key", "", "the Idempotency-Key of a write (to repeat one safely)")
	f.BoolVar(&a.flags.allPages, "all-pages", false, "follow a list over every page (at most --max-pages)")
	f.IntVar(&a.flags.maxPages, "max-pages", 50, "the most pages --all-pages reads")
	f.DurationVar(&a.flags.timeout, "timeout", 30*time.Second, "how long one request may take")
	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Getting started:"},
		&cobra.Group{ID: "read", Title: "Your accounts:"},
		&cobra.Group{ID: "trade", Title: "Trading:"},
		&cobra.Group{ID: "stream", Title: "Live:"},
		&cobra.Group{ID: "api", Title: "API operations (one per endpoint):"},
		&cobra.Group{ID: "tools", Title: "Tools:"},
	)
	root.SetHelpCommandGroupID("tools")
	root.SetCompletionCommandGroupID("tools")
	a.addStart(root)
	a.addRead(root)
	a.addTrade(root)
	a.addStream(root)
	a.addTools(root)
	a.addOps(root)
	return root
}

// prepare reads the configuration and sets the printer up, before any command.
func (a *App) prepare(cmd *cobra.Command) error {
	if err := a.loadConfig(); err != nil {
		return err
	}
	format := a.flags.output
	if format == "" {
		if p := a.currentProfile(); p != nil {
			format = p.Output
		}
	}
	if format == "" {
		format = "json"
		if a.OutTTY {
			format = "table"
		}
	}
	if !slices.Contains(output.Formats, format) {
		return usage("-o: table, json, jsonl or csv, not %q", format)
	}
	a.printer = &output.Printer{Out: a.Out, Err: a.Err, Format: format, Color: a.color()}
	if a.flags.jq != "" {
		code, err := output.ParseJQ(a.flags.jq)
		if err != nil {
			return &usageError{err.Error()}
		}
		a.printer.JQ = code
	}
	if a.flags.sandbox && a.flags.profile != "" && a.flags.profile != "sandbox" {
		return usage("--sandbox and --profile %s: pick one", a.flags.profile)
	}
	a.after = a.updateCheck(cmd)
	return nil
}

func (a *App) color() bool {
	return a.OutTTY && a.Getenv("NO_COLOR") == "" && a.Getenv("TERM") != "dumb" && enableColor()
}

// fail prints why a command failed and answers its exit code.
func (a *App) fail(err error) int {
	var ue *usageError
	var ee *exitError
	var pe *pp.Error
	switch {
	case errors.As(err, &ue):
		fmt.Fprintf(a.Err, "Error: %s\n", ue.msg)
		return ExitUsage
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintln(a.Err, ee.msg)
		}
		return ee.code
	case errors.As(err, &pe):
		a.apiError(pe)
		return exitFor(pe)
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(a.Err, "Stopped.")
		return 130
	case strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown flag") || strings.Contains(err.Error(), "arg(s)"):
		fmt.Fprintf(a.Err, "Error: %s\n", err)
		return ExitUsage
	}
	fmt.Fprintf(a.Err, "Error: %s\n", err)
	return ExitFailed
}

func exitFor(e *pp.Error) int {
	switch e.Kind {
	case pp.KindAuthentication, pp.KindPermission:
		return ExitAuth
	case pp.KindNotFound:
		return ExitNotFound
	case pp.KindConflict:
		return ExitConflict
	case pp.KindRuleRejected:
		return ExitRule
	case pp.KindRateLimit:
		return ExitRate
	case pp.KindServer:
		return ExitServer
	case pp.KindConnection, pp.KindTimeout:
		return ExitNetwork
	}
	return ExitUsage
}

// apiError prints an API error: its words, its code and request id, its details, and what to do.
func (a *App) apiError(e *pp.Error) {
	ref := e.Code
	if e.Status != 0 {
		ref += fmt.Sprintf(", HTTP %d", e.Status)
	}
	if e.RequestID != "" {
		ref += ", " + e.RequestID
	}
	fmt.Fprintf(a.Err, "Error: %s (%s)\n", strings.TrimSpace(e.Message), ref)
	if len(e.Details) > 0 {
		if v, err := output.Decode(e.Details); err == nil {
			fmt.Fprintf(a.Err, "  details: %s\n", output.Compact(v))
		}
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(a.Err, "  try again in %s\n", e.RetryAfter.Round(time.Second))
	}
	switch {
	case e.Kind == pp.KindAuthentication && e.Code == "missing_api_key":
	case e.Kind == pp.KindAuthentication:
		fmt.Fprintln(a.Err, "  sign in again: pperp login --profile "+a.profileName())
	case e.IdempotencyKey != "" && (e.Kind == pp.KindConnection || e.Kind == pp.KindTimeout || e.Kind == pp.KindServer):
		fmt.Fprintf(a.Err, "  the outcome is not known: check (pperp orders) before trying again, then repeat with --idempotency-key %s\n", e.IdempotencyKey)
	}
}

// line reads one line from stdin.
func (a *App) line() (string, error) {
	s, err := a.reader.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// confirm asks a yes or no question (no: the command ends, exit 1). With --yes it is not asked; with no one to ask it
// needs --yes.
func (a *App) confirm(question string) error {
	if a.flags.yes {
		return nil
	}
	if !a.InTTY {
		return usage("no one to ask %q: add --yes to go ahead", question)
	}
	fmt.Fprintf(a.Err, "%s [y/N] ", question)
	ans, _ := a.line()
	if strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
		return nil
	}
	return &exitError{ExitFailed, "Nothing was done."}
}

// confirmTyped asks for a word to be typed back (an account's name) before something that cannot be undone.
func (a *App) confirmTyped(what, word string) error {
	if a.flags.yes {
		return nil
	}
	if !a.InTTY {
		return usage("%s: add --yes to go ahead without being asked", what)
	}
	fmt.Fprintf(a.Err, "%s\nType %s to go ahead: ", what, word)
	ans, _ := a.line()
	if ans == word {
		return nil
	}
	return &exitError{ExitFailed, "Nothing was done."}
}

// note is a line for the person at the terminal (stderr, so pipes get only the answer).
func (a *App) note(format string, args ...any) {
	fmt.Fprintf(a.Err, format+"\n", args...)
}
