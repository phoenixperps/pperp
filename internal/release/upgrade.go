package release

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is where releases are published (docs host, nginx static: tools/release publish).
const DefaultURL = "https://docs.phoenixperpsfunding.com/downloads/cli"

const maxArchive = 100 << 20

//go:embed keys
var keyFiles embed.FS

// Keys are the release keys built into this pperp: the current one, and the next one during a rotation.
func Keys() []PublicKey {
	var out []PublicKey
	entries, _ := keyFiles.ReadDir("keys")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		b, err := keyFiles.ReadFile("keys/" + e.Name())
		if err != nil {
			continue
		}
		if k, err := ParsePublicKey(string(b)); err == nil {
			out = append(out, k)
		}
	}
	return out
}

// Latest is latest.json: the version to have, the oldest one still served, the ones withdrawn.
type Latest struct {
	Version      string   `json:"version"`
	PublishedAt  string   `json:"published_at"`
	MinSupported string   `json:"min_supported"`
	Revoked      []string `json:"revoked"`
	Notes        string   `json:"notes"`
}

// Source is a release site and the keys its files must be signed with.
type Source struct {
	URL  string
	HTTP *http.Client
	Keys []PublicKey
}

func (s *Source) get(ctx context.Context, path string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.URL, "/")+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	h := s.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	res, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", path, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than any release file", path)
	}
	return b, nil
}

// signed is a file and its signature, checked; the trusted comment must name the file (and the version).
func (s *Source) signed(ctx context.Context, path, want string) ([]byte, error) {
	if len(s.Keys) == 0 {
		return nil, errors.New("this pperp was built without a release key: install it again from https://docs.phoenixperpsfunding.com/cli")
	}
	body, err := s.get(ctx, path, 1<<20)
	if err != nil {
		return nil, err
	}
	sig, err := s.get(ctx, path+".minisig", 4096)
	if err != nil {
		return nil, err
	}
	comment, err := Verify(s.Keys, body, string(sig))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !strings.Contains(" "+comment+" ", " "+want+" ") {
		return nil, fmt.Errorf("%s: its signature is for another file (%q)", path, comment)
	}
	return body, nil
}

// Latest is the signed latest.json.
func (s *Source) Latest(ctx context.Context) (*Latest, error) {
	b, err := s.signed(ctx, "latest.json", "file:latest.json")
	if err != nil {
		return nil, err
	}
	var l Latest
	if err := json.Unmarshal(b, &l); err != nil || Compare(l.Version, "0.0.0") <= 0 {
		return nil, errors.New("latest.json could not be read")
	}
	return &l, nil
}

// ArchiveName is a release's archive for a system.
func ArchiveName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("pperp_%s_%s_%s%s", version, goos, goarch, ext)
}

// Fetch is the pperp binary of a version for this system: its archive's hash checked against the signed SHA256SUMS.
func (s *Source) Fetch(ctx context.Context, version, goos, goarch string) ([]byte, error) {
	sums, err := s.signed(ctx, "v"+version+"/SHA256SUMS", "version:"+version)
	if err != nil {
		return nil, err
	}
	name := ArchiveName(version, goos, goarch)
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("version %s has no build for %s/%s", version, goos, goarch)
	}
	archive, err := s.get(ctx, "v"+version+"/"+name, maxArchive)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != strings.ToLower(want) {
		return nil, fmt.Errorf("%s does not match its signed checksum: nothing was changed", name)
	}
	return extract(archive, goos)
}

// extract is the pperp binary in an archive.
func extract(archive []byte, goos string) ([]byte, error) {
	if goos == "windows" {
		z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range z.File {
			if filepath.Base(f.Name) == "pperp.exe" && f.UncompressedSize64 <= maxArchive {
				r, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer r.Close()
				return io.ReadAll(io.LimitReader(r, maxArchive))
			}
		}
		return nil, errors.New("the archive has no pperp.exe")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New("the archive has no pperp")
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "pperp" && h.Size <= maxArchive {
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

// Compare orders two versions (1.2.10 > 1.2.9); an unreadable one is the oldest.
func Compare(a, b string) int {
	pa, pb := parts(a), parts(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parts(v string) [3]int {
	var out [3]int
	core, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	f := strings.Split(core, ".")
	if len(f) != 3 {
		return [3]int{-1, -1, -1}
	}
	for i, s := range f {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return [3]int{-1, -1, -1}
		}
		out[i] = n
	}
	return out
}

// ManagedBy is the command of the package manager that installed pperp at exe ("": the installer or by hand).
func ManagedBy(exe string) string {
	p := strings.ToLower(strings.ReplaceAll(exe, `\`, "/"))
	switch {
	case strings.Contains(p, "/cellar/") || strings.Contains(p, "/homebrew/") || strings.Contains(p, "/linuxbrew/"):
		return "brew upgrade pperp"
	case strings.Contains(p, "/scoop/apps/"):
		return "scoop update pperp"
	case strings.Contains(p, "/winget/"):
		return "winget upgrade pperp"
	}
	return ""
}

// Install puts a new binary in place of exe: written beside it, run once (it must say its version), then swapped in.
// On Windows the running file is renamed to .old (removed by Cleanup at the next start).
func Install(exe string, bin []byte, version string) error {
	dir := filepath.Dir(exe)
	staged := exe + ".new"
	if err := os.WriteFile(staged, bin, 0o755); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("pperp is in a folder you cannot write to (%s): run the installer again rather than this as administrator", dir)
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, staged, "version").Output()
	if err != nil || !strings.Contains(string(out), "pperp "+version) {
		_ = os.Remove(staged)
		return fmt.Errorf("the new pperp did not start on this computer (%v): nothing was changed", err)
	}
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			_ = os.Remove(staged)
			return err
		}
		if err := os.Rename(staged, exe); err != nil {
			_ = os.Rename(old, exe)
			return err
		}
		return nil
	}
	return os.Rename(staged, exe)
}

// Cleanup removes what an upgrade on Windows left beside pperp.
func Cleanup(exe string) {
	if runtime.GOOS == "windows" {
		_ = os.Remove(exe + ".old")
	}
}
