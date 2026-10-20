// Command release makes a pperp release, on the computer that keeps the release key (design 15 section 7; every
// release waits for the owner):
//
//	go run ./tools/release keygen [-next]            once: the secret key (outside OneDrive, a password), its public key
//	                                                 into internal/release/keys (commit it: pperp checks releases with it)
//	go run ./tools/release build -version 1.2.0      tests, the table against the spec, the six archives and SHA256SUMS
//	go run ./tools/release sign -version 1.2.0       SHA256SUMS.minisig and latest.json(.minisig), checked with the
//	        [-notes TEXT] [-min-supported 1.0.0] [-revoke 1.1.0,1.1.1]   built-in keys
//	go run ./tools/release publish -version 1.2.0 -host root@<docs box>
//	                                                 the version's files (never overwritten), the installers and the keys,
//	                                                 then latest.json; then it reads the release back and checks it
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/phoenixperps/pperp/internal/release"
)

var targets = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}}

const remoteRoot = "/var/www/pperp" // nginx on the docs host serves /install.sh, /install.ps1 and /downloads/cli/ from it

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "release: "+format+"\n", args...)
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func run(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fail("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// password reads the release key's password from the terminal, or, when PPERP_RELEASE_PASSWORD_FILE names a file (a
// release run without a terminal), from that file, kept beside the key and never in OneDrive.
func password(prompt string) string {
	if f := os.Getenv("PPERP_RELEASE_PASSWORD_FILE"); f != "" {
		if strings.Contains(strings.ToLower(f), "onedrive") {
			fail("the password file never goes in OneDrive (it would be copied to the cloud)")
		}
		b, err := os.ReadFile(f)
		must(err)
		return strings.TrimSpace(string(b))
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	must(err)
	return string(b)
}

func defaultKey() string {
	home, err := os.UserHomeDir()
	must(err)
	return filepath.Join(home, ".pperp-release", "pperp.key")
}

func main() {
	if len(os.Args) < 2 {
		fail("keygen, build, sign or publish (see go doc ./tools/release)")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	version := fs.String("version", "", "the version, 1.2.0")
	keyPath := fs.String("key", defaultKey(), "the secret key file")
	next := fs.Bool("next", false, "keygen: the next key (a rotation), written as pperp-next.pub")
	notes := fs.String("notes", "", "sign: a line on what changed")
	minSupported := fs.String("min-supported", "", "sign: the oldest version still supported (default: this major's x.0.0)")
	revoke := fs.String("revoke", "", "sign: versions withdrawn, comma-separated")
	host := fs.String("host", "", "publish: user@host of the docs box")
	must(fs.Parse(os.Args[2:]))
	v := strings.TrimPrefix(*version, "v")
	if os.Args[1] != "keygen" && release.Compare(v, "0.0.0") <= 0 {
		fail("-version X.Y.Z")
	}
	dist := filepath.Join("dist", "v"+v)
	switch os.Args[1] {
	case "keygen":
		keygen(*keyPath, *next)
	case "build":
		build(v, dist)
	case "sign":
		sign(v, dist, *keyPath, *notes, *minSupported, *revoke)
	case "publish":
		if *host == "" {
			fail("-host user@host")
		}
		publish(v, dist, *host)
	default:
		fail("unknown step %q", os.Args[1])
	}
}

func keygen(path string, next bool) {
	if strings.Contains(strings.ToLower(path), "onedrive") {
		fail("the secret key never goes in OneDrive (it would be copied to the cloud): -key elsewhere")
	}
	if _, err := os.Stat(path); err == nil {
		fail("%s exists: a new key needs a rotation (-next) and a new path", path)
	}
	pw := password("Password for the release key (12 characters or more): ")
	if len(pw) < 12 || password("Again: ") != pw {
		fail("the passwords are short or differ")
	}
	fmt.Fprintln(os.Stderr, "Deriving the key's encryption (a few seconds, 1 GB of memory)...")
	pub, sec, err := release.GenerateKey(pw)
	must(err)
	must(os.MkdirAll(filepath.Dir(path), 0o700))
	must(os.WriteFile(path, []byte(sec), 0o600))
	name := "pperp.pub"
	if next {
		name = "pperp-next.pub"
	}
	out := filepath.Join("internal", "release", "keys", name)
	must(os.WriteFile(out, []byte(pub.Encode()), 0o644))
	fmt.Printf("Secret key: %s (back it up with its password, in a password manager).\nPublic key %s: %s (commit it; every pperp built from then on trusts it).\n", path, pub.IDString(), out)
}

func build(v, dist string) {
	if run("git", "status", "--porcelain") != "" {
		fail("commit first: a release is a commit")
	}
	if _, err := os.Stat(dist); err == nil {
		fail("%s exists: a version is built once", dist)
	}
	if len(release.Keys()) == 0 {
		fail("no release key in internal/release/keys: run keygen first")
	}
	fmt.Fprintln(os.Stderr, "Tests, and the operation table against the spec...")
	run("go", "test", "./...")
	run("go", "generate", "./...")
	if run("git", "status", "--porcelain") != "" {
		fail("go generate changed files: the table is stale")
	}
	stamp, err := strconv.ParseInt(run("git", "log", "-1", "--format=%ct"), 10, 64)
	must(err)
	mtime := time.Unix(stamp, 0)
	readme, err := os.ReadFile("README.md")
	must(err)
	must(os.MkdirAll(dist, 0o755))
	tmp, err := os.MkdirTemp("", "pperp-release")
	must(err)
	defer os.RemoveAll(tmp)
	sums := map[string]string{}
	for _, t := range targets {
		goos, goarch := t[0], t[1]
		exe := "pperp"
		if goos == "windows" {
			exe += ".exe"
		}
		bin := filepath.Join(tmp, goos+"-"+goarch, exe)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+v, "-o", bin, "./cmd/pperp")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		cmd.Stderr = os.Stderr
		must(cmd.Run())
		b, err := os.ReadFile(bin)
		must(err)
		name := release.ArchiveName(v, goos, goarch)
		var a []byte
		if goos == "windows" {
			a = zipOf(mtime, map[string][]byte{exe: b, "README.md": readme})
		} else {
			a = tarOf(mtime, map[string][]byte{exe: b, "README.md": readme})
		}
		must(os.WriteFile(filepath.Join(dist, name), a, 0o644))
		h := sha256.Sum256(a)
		sums[name] = hex.EncodeToString(h[:])
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", sums[n], n)
	}
	must(os.WriteFile(filepath.Join(dist, "SHA256SUMS"), []byte(b.String()), 0o644))
	fmt.Printf("Built %s. Next: go run ./tools/release sign -version %s\n", dist, v)
}

func tarOf(mtime time.Time, files map[string][]byte) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(files) {
		mode := int64(0o644)
		if name == "pperp" {
			mode = 0o755
		}
		must(tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(files[name])), ModTime: mtime, Typeflag: tar.TypeReg, Format: tar.FormatPAX}))
		_, err := tw.Write(files[name])
		must(err)
	}
	must(tw.Close())
	must(gz.Close())
	return b.Bytes()
}

func zipOf(mtime time.Time, files map[string][]byte) []byte {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, name := range sortedKeys(files) {
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mtime})
		must(err)
		_, err = w.Write(files[name])
		must(err)
	}
	must(z.Close())
	return b.Bytes()
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sign(v, dist, keyPath, notes, minSupported, revoke string) {
	sums, err := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	if err != nil {
		fail("build it first: %v", err)
	}
	sec, err := os.ReadFile(keyPath)
	must(err)
	k, err := release.OpenSecretKey(string(sec), password("Release key password: "))
	must(err)
	known := false
	for _, p := range release.Keys() {
		known = known || p.IDString() == k.Public().IDString()
	}
	if !known {
		fail("key %s is not built into pperp: commit its public key in internal/release/keys first", k.Public().IDString())
	}
	now := time.Now().UTC()
	stamp := fmt.Sprintf("timestamp:%d", now.Unix())
	sumsSig := k.Sign(sums, "file:SHA256SUMS version:"+v+" "+stamp)
	if minSupported == "" {
		minSupported = strings.Split(v, ".")[0] + ".0.0"
	}
	var revoked []string
	for _, r := range strings.Split(revoke, ",") {
		if r = strings.TrimPrefix(strings.TrimSpace(r), "v"); r != "" {
			revoked = append(revoked, r)
		}
	}
	if revoked == nil {
		revoked = []string{}
	}
	latest, err := json.MarshalIndent(release.Latest{Version: v, PublishedAt: now.Format(time.RFC3339), MinSupported: minSupported, Revoked: revoked, Notes: notes}, "", "  ")
	must(err)
	latest = append(latest, '\n')
	latestSig := k.Sign(latest, "file:latest.json version:"+v+" "+stamp)
	keys := release.Keys()
	if _, err := release.Verify(keys, sums, sumsSig); err != nil {
		fail("SHA256SUMS: %v", err)
	}
	if _, err := release.Verify(keys, latest, latestSig); err != nil {
		fail("latest.json: %v", err)
	}
	must(os.WriteFile(filepath.Join(dist, "SHA256SUMS.minisig"), []byte(sumsSig), 0o644))
	must(os.WriteFile(filepath.Join("dist", "latest.json"), latest, 0o644))
	must(os.WriteFile(filepath.Join("dist", "latest.json.minisig"), []byte(latestSig), 0o644))
	fmt.Printf("Signed %s with key %s. Next: go run ./tools/release publish -version %s -host <user@docs box>\n", v, k.Public().IDString(), v)
}

func publish(v, dist, host string) {
	for _, f := range []string{filepath.Join(dist, "SHA256SUMS.minisig"), filepath.Join("dist", "latest.json.minisig")} {
		if _, err := os.Stat(f); err != nil {
			fail("sign it first: %v", err)
		}
	}
	var latest release.Latest
	b, err := os.ReadFile(filepath.Join("dist", "latest.json"))
	must(err)
	must(json.Unmarshal(b, &latest))
	if latest.Version != v {
		fail("dist/latest.json is for %s, not %s", latest.Version, v)
	}
	cli := remoteRoot + "/downloads/cli"
	ssh := func(script string) { run("ssh", "-o", "BatchMode=yes", host, script) }
	scp := func(dst string, files ...string) {
		run("scp", append(append([]string{"-q", "-o", "BatchMode=yes"}, files...), host+":"+dst)...)
	}
	ssh(fmt.Sprintf("set -e; test ! -e %[1]s/v%[2]s || { echo 'v%[2]s is published already: a version never changes' >&2; exit 1; }; mkdir -p %[1]s/v%[2]s.tmp %[1]s/keys", cli, v))
	versionFiles, err := filepath.Glob(filepath.Join(dist, "*"))
	must(err)
	scp(cli+"/v"+v+".tmp/", versionFiles...)
	keys, err := filepath.Glob(filepath.Join("internal", "release", "keys", "*.pub"))
	must(err)
	scp(cli+"/keys/", keys...)
	scp(remoteRoot+"/", filepath.Join("install", "install.sh"), filepath.Join("install", "install.ps1"))
	ssh(fmt.Sprintf("mkdir -p %s/next", cli))
	scp(cli+"/next/", filepath.Join("dist", "latest.json"), filepath.Join("dist", "latest.json.minisig"))
	// the version first, then latest.json (its signature first), so every reader sees a whole release
	ssh(fmt.Sprintf("set -e; cd %s; mv v%s.tmp v%s; mv next/latest.json.minisig latest.json.minisig; mv next/latest.json latest.json; rmdir next; chmod -R a+rX . %s/install.sh %s/install.ps1", cli, v, v, remoteRoot, remoteRoot))
	fmt.Fprintln(os.Stderr, "Reading the release back...")
	src := &release.Source{URL: release.DefaultURL, Keys: release.Keys()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l, err := src.Latest(ctx)
	if err != nil || l.Version != v {
		fail("latest.json as served: %+v %v", l, err)
	}
	if _, err := src.Fetch(ctx, v, runtime.GOOS, runtime.GOARCH); err != nil {
		fail("the %s/%s build as served: %v", runtime.GOOS, runtime.GOARCH, err)
	}
	fmt.Printf("pperp %s is published: pperp upgrade, or the installers, get it now.\n", v)
}
