package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	minisign "github.com/jedisct1/go-minisign"
)

// A copy of this test binary stands in for a new pperp: with PPERP_FAKE_VERSION set it only says that version.
func TestMain(m *testing.M) {
	if v := os.Getenv("PPERP_FAKE_VERSION"); v != "" {
		fmt.Printf("pperp %s (test)\n", v)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func testKey(t *testing.T) (*SecretKey, PublicKey) {
	t.Helper()
	pub, sec, err := generateKey("correct horse", 32768, 16<<20) // cheap scrypt for tests; releases use minisign's own
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSecretKey(sec, "wrong horse"); err == nil {
		t.Fatal("a wrong password opened the key")
	}
	k, err := OpenSecretKey(sec, "correct horse")
	if err != nil || !bytes.Equal(k.Public().Key, pub.Key) {
		t.Fatalf("open: %v", err)
	}
	return k, pub
}

func TestMinisign(t *testing.T) {
	k, pub := testKey(t)
	msg := []byte("abc123  pperp_1.2.0_linux_amd64.tar.gz\n")
	sig := k.Sign(msg, "file:SHA256SUMS version:1.2.0")
	if c, err := Verify([]PublicKey{pub}, msg, sig); err != nil || c != "file:SHA256SUMS version:1.2.0" {
		t.Fatalf("verify: %q %v", c, err)
	}
	// the minisign tool's own reading (the reference Go implementation) takes our key and signature
	rp, err := minisign.DecodePublicKey(pub.Encode())
	if err != nil {
		t.Fatal(err)
	}
	rs, err := minisign.DecodeSignature(sig)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := rp.Verify(msg, rs); !ok || err != nil {
		t.Fatalf("reference verify: %v %v", ok, err)
	}
	if again, err := ParsePublicKey(pub.Encode()); err != nil || again.IDString() != pub.IDString() {
		t.Fatal("public key round trip")
	}
	// a changed file, a changed trusted comment, another key: refused
	if _, err := Verify([]PublicKey{pub}, append(msg, 'x'), sig); err == nil {
		t.Fatal("changed file taken")
	}
	if _, err := Verify([]PublicKey{pub}, msg, strings.Replace(sig, "version:1.2.0", "version:9.9.9", 1)); err == nil {
		t.Fatal("changed trusted comment taken")
	}
	_, other := testKey(t)
	if _, err := Verify([]PublicKey{other}, msg, sig); err == nil {
		t.Fatal("another key taken")
	}
	if n, r, p := scryptParams(opsLimit, memLimit); n != 1<<20 || r != 8 || p != 1 {
		t.Fatalf("minisign's cost: N %d r %d p %d", n, r, p)
	}
}

// release is a release site: latest.json and a version's archive and SHA256SUMS, signed.
type release struct {
	files map[string][]byte
}

func (r *release) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/")]
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(b)
}

func archive(t *testing.T, goos string, bin []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if goos == "windows" {
		z := zip.NewWriter(&b)
		f, _ := z.Create("pperp.exe")
		_, _ = f.Write(bin)
		_ = z.Close()
		return b.Bytes()
	}
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "pperp", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(bin)
	_ = tw.Close()
	_ = gz.Close()
	return b.Bytes()
}

func publish(t *testing.T, k *SecretKey, version string, bin []byte) *release {
	t.Helper()
	name := ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	a := archive(t, runtime.GOOS, bin)
	sum := sha256.Sum256(a)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	latest := []byte(`{"version":"` + version + `","published_at":"2026-10-11T00:00:00Z","min_supported":"1.0.0","revoked":["1.1.0"],"notes":""}`)
	return &release{files: map[string][]byte{
		"latest.json":                         latest,
		"latest.json.minisig":                 []byte(k.Sign(latest, "file:latest.json version:"+version)),
		"v" + version + "/SHA256SUMS":         sums,
		"v" + version + "/SHA256SUMS.minisig": []byte(k.Sign(sums, "file:SHA256SUMS version:"+version)),
		"v" + version + "/" + name:            a,
	}}
}

func TestUpgrade(t *testing.T) {
	k, pub := testKey(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	site := publish(t, k, "1.2.0", bin)
	srv := httptest.NewServer(site)
	defer srv.Close()
	src := &Source{URL: srv.URL, Keys: []PublicKey{pub}}
	ctx := context.Background()

	l, err := src.Latest(ctx)
	if err != nil || l.Version != "1.2.0" || l.Revoked[0] != "1.1.0" {
		t.Fatalf("latest: %+v %v", l, err)
	}
	got, err := src.Fetch(ctx, "1.2.0", runtime.GOOS, runtime.GOARCH)
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("fetch: %v", err)
	}

	// what an upgrade must refuse: a tampered SHA256SUMS, an archive that is not the one summed, an unknown key,
	// a signature of another file
	sums := site.files["v1.2.0/SHA256SUMS"]
	site.files["v1.2.0/SHA256SUMS"] = bytes.Replace(sums, []byte("  "), []byte("0  "), 1)
	if _, err := src.Fetch(ctx, "1.2.0", runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("tampered SHA256SUMS taken")
	}
	site.files["v1.2.0/SHA256SUMS"] = sums
	name := "v1.2.0/" + ArchiveName("1.2.0", runtime.GOOS, runtime.GOARCH)
	good := site.files[name]
	site.files[name] = archive(t, runtime.GOOS, []byte("something else"))
	if _, err := src.Fetch(ctx, "1.2.0", runtime.GOOS, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("other archive taken: %v", err)
	}
	site.files[name] = good
	_, other := testKey(t)
	if _, err := (&Source{URL: srv.URL, Keys: []PublicKey{other}}).Latest(ctx); err == nil {
		t.Fatal("unknown key taken")
	}
	site.files["latest.json.minisig"] = site.files["v1.2.0/SHA256SUMS.minisig"]
	if _, err := src.Latest(ctx); err == nil {
		t.Fatal("another file's signature taken")
	}
	if _, err := (&Source{URL: srv.URL}).Latest(ctx); err == nil || !strings.Contains(err.Error(), "without a release key") {
		t.Fatalf("no key built in: %v", err)
	}

	// the swap: the new one runs once, then takes the old one's place
	dir := t.TempDir()
	exe := filepath.Join(dir, "pperp"+map[bool]string{true: ".exe"}[runtime.GOOS == "windows"])
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PPERP_FAKE_VERSION", "1.2.0")
	if err := Install(exe, bin, "9.9.9"); err == nil {
		t.Fatal("a binary saying another version was installed")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("a failed install changed pperp")
	}
	if err := Install(exe, bin, "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, bin) {
		t.Fatal("not swapped in")
	}
	Cleanup(exe)
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatal(".old left")
	}
}

func TestCompareAndManagers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"1.2.10", "1.2.9", 1}, {"1.0.0", "1.0.0", 0}, {"v1.0.0", "1.0.1", -1}, {"dev", "0.0.1", -1}} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Fatalf("%s vs %s: %d", c.a, c.b, got)
		}
	}
	if ManagedBy("/opt/homebrew/Cellar/pperp/1.0.0/bin/pperp") != "brew upgrade pperp" || ManagedBy(`C:\Users\x\scoop\apps\pperp\current\pperp.exe`) != "scoop update pperp" || ManagedBy("/home/x/.local/bin/pperp") != "" {
		t.Fatal("package managers")
	}
}
