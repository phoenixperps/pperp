package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/zalando/go-keyring"
)

/*
Profiles and credentials (design 15 section 3 "Profiles and credentials"). The profiles live and sandbox always exist,
and more can be made (pperp login --profile NAME). config.toml holds the default profile and each profile's settings,
never a secret: the credential of a profile is in the system keychain (service pperp, item profile:<name>), or, only
when signed in with --store file, in credentials.json beside the configuration (readable by its owner only).
PHOENIXPERPS_API_KEY, when set, is used for that one call instead.
*/

const keychainService = "pperp"

// Config is config.toml.
type Config struct {
	Profile  string              `toml:"profile,omitempty"`
	Profiles map[string]*Profile `toml:"profiles,omitempty"`
}

// Profile is one profile's settings.
type Profile struct {
	Env            string `toml:"env"`
	DefaultAccount string `toml:"default_account,omitempty"`
	Output         string `toml:"output,omitempty"`
	ConfirmTrades  *bool  `toml:"confirm_trades,omitempty"`
	Store          string `toml:"store,omitempty"`
	BaseURL        string `toml:"base_url,omitempty"`
}

// confirms tells whether trades are confirmed before they are sent (on by default for live, off for the sandbox).
func (p *Profile) confirms() bool {
	if p.ConfirmTrades != nil {
		return *p.ConfirmTrades
	}
	return p.Env == "live"
}

// Credential is what a profile signs in with.
type Credential struct {
	Token     string `json:"token"`
	Kind      string `json:"kind"`                 // app (pperp login) or key
	ExpiresAt int64  `json:"expires_at,omitempty"` // ms; 0: never
	Scope     string `json:"scope,omitempty"`
}

func (a *App) configDir() (string, error) {
	if d := a.Getenv("PPERP_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		if d := a.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "pperp"), nil
		}
	}
	if d := a.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "pperp"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home folder for the configuration: set PPERP_CONFIG_DIR")
	}
	return filepath.Join(home, ".config", "pperp"), nil
}

func (a *App) loadConfig() error {
	if a.cfg != nil {
		return nil
	}
	dir, err := a.configDir()
	if err != nil {
		return err
	}
	a.cfgDir = dir
	a.cfg = &Config{}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := toml.Decode(string(b), a.cfg); err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(dir, "config.toml"), err)
	}
	return nil
}

// writePrivate writes a file whole (a temporary file renamed over it), readable by its owner only.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *App) saveConfig() error {
	var b bytes.Buffer
	b.WriteString("# pperp settings (no secrets here: they are in the system keychain). pperp config set KEY VALUE changes them.\n")
	if err := toml.NewEncoder(&b).Encode(a.cfg); err != nil {
		return err
	}
	return writePrivate(filepath.Join(a.cfgDir, "config.toml"), b.Bytes())
}

// profileName is the profile of this run: --sandbox, --profile, PPERP_PROFILE, the default, else sandbox.
func (a *App) profileName() string {
	switch {
	case a.flags.sandbox:
		return "sandbox"
	case a.flags.profile != "":
		return a.flags.profile
	case a.Getenv("PPERP_PROFILE") != "":
		return a.Getenv("PPERP_PROFILE")
	case a.cfg != nil && a.cfg.Profile != "":
		return a.cfg.Profile
	}
	return "sandbox"
}

// profile is a profile's settings (nil: no such profile).
func (a *App) profile(name string) *Profile {
	if a.cfg != nil && a.cfg.Profiles[name] != nil {
		p := a.cfg.Profiles[name]
		if p.Env == "" && (name == "live" || name == "sandbox") {
			p.Env = name
		}
		return p
	}
	if name == "live" || name == "sandbox" {
		return &Profile{Env: name}
	}
	return nil
}

func (a *App) currentProfile() *Profile { return a.profile(a.profileName()) }

// keepProfile makes a profile's settings part of config.toml (to change them).
func (a *App) keepProfile(name string, p *Profile) {
	if a.cfg.Profiles == nil {
		a.cfg.Profiles = map[string]*Profile{}
	}
	a.cfg.Profiles[name] = p
}

func (a *App) credentialsFile() string { return filepath.Join(a.cfgDir, "credentials.json") }

func (a *App) readFileStore() (map[string]*Credential, error) {
	m := map[string]*Credential{}
	b, err := os.ReadFile(a.credentialsFile())
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", a.credentialsFile(), err)
	}
	return m, nil
}

// loadCredential is a profile's credential (nil: not signed in).
func (a *App) loadCredential(name string, p *Profile) (*Credential, error) {
	if p.Store == "file" {
		m, err := a.readFileStore()
		if err != nil {
			return nil, err
		}
		return m[name], nil
	}
	s, err := keyring.Get(keychainService, "profile:"+name)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, keychainError(err)
	}
	var c Credential
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return nil, fmt.Errorf("the keychain item of profile %s could not be read: pperp logout --profile %s, then sign in again", name, name)
	}
	return &c, nil
}

func (a *App) saveCredential(name string, p *Profile, c *Credential) error {
	b, _ := json.Marshal(c)
	if p.Store == "file" {
		m, err := a.readFileStore()
		if err != nil {
			return err
		}
		m[name] = c
		out, _ := json.MarshalIndent(m, "", "  ")
		return writePrivate(a.credentialsFile(), out)
	}
	if err := keyring.Set(keychainService, "profile:"+name, string(b)); err != nil {
		return keychainError(err)
	}
	return nil
}

func (a *App) deleteCredential(name string, p *Profile) error {
	if p.Store == "file" {
		m, err := a.readFileStore()
		if err != nil {
			return err
		}
		delete(m, name)
		out, _ := json.MarshalIndent(m, "", "  ")
		return writePrivate(a.credentialsFile(), out)
	}
	if err := keyring.Delete(keychainService, "profile:"+name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return keychainError(err)
	}
	return nil
}

func keychainError(err error) error {
	return fmt.Errorf("the system keychain is not available (%v): sign in with --store file to keep the credential in a file only you can read, or set PHOENIXPERPS_API_KEY", err)
}

// checkBaseURL takes an API address of ours (https, *.phoenixperpsfunding.com) or one on this computer.
func checkBaseURL(raw string) error { return checkAddress(raw, false) }

// checkAddress takes an address of ours or one on this computer; withPath: it may have a path (the releases').
func checkAddress(raw string, withPath bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (!withPath && u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return usage("the address %q is not a plain https://host", raw)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
		if u.Scheme == "http" || u.Scheme == "https" {
			return nil
		}
	}
	if u.Scheme == "https" && (host == "phoenixperpsfunding.com" || strings.HasSuffix(host, ".phoenixperpsfunding.com")) {
		return nil
	}
	return usage("the address must be https://*.phoenixperpsfunding.com or this computer, not %q", raw)
}
