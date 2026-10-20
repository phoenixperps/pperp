package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/phoenixperps/pperp/internal/release"
)

// releases is where new pperps come from (PPERP_RELEASES_URL: one of ours, or this computer for tests).
func (a *App) releases() (*release.Source, error) {
	u := firstOf(a.Getenv("PPERP_RELEASES_URL"), release.DefaultURL)
	if err := checkAddress(u, true); err != nil {
		return nil, err
	}
	keys := a.releaseKeys
	if keys == nil {
		keys = release.Keys()
	}
	return &release.Source{URL: u, HTTP: a.httpClient(), Keys: keys}, nil
}

func (a *App) upgradeCommand() *cobra.Command {
	var check bool
	var version string
	cmd := &cobra.Command{
		Use: "upgrade", Short: "Move to the latest pperp (its signatures checked first)", GroupID: "tools", Args: noArgs,
		Long: `Moves to the latest pperp: latest.json and the version's SHA256SUMS must be signed by a key built into
this pperp, the download must match its checksum, and the new pperp must start before it replaces this one.
--version moves to another version (a downgrade too); a withdrawn version is never installed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				exe = real
			}
			if m := release.ManagedBy(exe); m != "" {
				return &exitError{ExitFailed, "A package manager installed this pperp: " + m}
			}
			src, err := a.releases()
			if err != nil {
				return err
			}
			latest, err := src.Latest(ctx)
			if err != nil {
				return &exitError{ExitNetwork, "No release information: " + err.Error()}
			}
			target := firstOf(strings.TrimPrefix(version, "v"), latest.Version)
			if slices.Contains(latest.Revoked, target) {
				return &exitError{ExitFailed, fmt.Sprintf("pperp %s was withdrawn: it is not installed.", target)}
			}
			if release.Compare(a.Version, latest.MinSupported) < 0 {
				a.note("This pperp (%s) is older than the oldest one still supported (%s).", a.Version, latest.MinSupported)
			}
			switch cmp := release.Compare(target, a.Version); {
			case cmp == 0:
				a.note("pperp %s is the latest.", a.Version)
				return nil
			case version == "" && a.Version == "dev":
				return usage("this pperp is a development build: --version %s installs that release", latest.Version)
			case cmp < 0 && version == "":
				a.note("This pperp (%s) is newer than the latest release (%s).", a.Version, latest.Version)
				return nil
			}
			if check {
				fmt.Fprintf(a.Out, "pperp %s is out (you have %s): pperp upgrade\n", target, a.Version)
				return nil
			}
			bin, err := src.Fetch(ctx, target, runtime.GOOS, runtime.GOARCH)
			if err != nil {
				return &exitError{ExitFailed, "Not upgraded: " + err.Error()}
			}
			if err := release.Install(exe, bin, target); err != nil {
				return &exitError{ExitFailed, "Not upgraded: " + err.Error()}
			}
			a.note("pperp %s is installed (was %s).", target, a.Version)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether a newer pperp is out")
	cmd.Flags().StringVar(&version, "version", "", "this version instead of the latest")
	return cmd
}

// updateCheck looks for a newer pperp at most once a day, for a person at a terminal only (not CI, not with
// PPERP_NO_UPDATE_CHECK), and says so on stderr once the command is done.
func (a *App) updateCheck(cmd *cobra.Command) func() {
	if !a.OutTTY || a.Getenv("CI") != "" || a.Getenv("PPERP_NO_UPDATE_CHECK") != "" || a.Version == "dev" || len(release.Keys()) == 0 {
		return nil
	}
	switch cmd.Name() {
	case "upgrade", "mcp", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return nil
	}
	file := filepath.Join(a.cfgDir, "update-check.json")
	var st struct {
		CheckedAt int64  `json:"checked_at"`
		Latest    string `json:"latest"`
	}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	notice := func(latest string) {
		if latest != "" && release.Compare(latest, a.Version) > 0 {
			a.note("pperp %s is out (you have %s): pperp upgrade", latest, a.Version)
		}
	}
	if time.Since(time.UnixMilli(st.CheckedAt)) < 24*time.Hour {
		return func() { notice(st.Latest) }
	}
	done := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		v := ""
		if src, err := a.releases(); err == nil {
			if l, err := src.Latest(ctx); err == nil {
				v = l.Version
			}
		}
		st.CheckedAt, st.Latest = time.Now().UnixMilli(), v
		b, _ := json.Marshal(st)
		_ = writePrivate(file, b)
		done <- v
	}()
	return func() {
		select {
		case v := <-done:
			notice(v)
		default: // not back yet: it is kept for the next command
		}
	}
}
