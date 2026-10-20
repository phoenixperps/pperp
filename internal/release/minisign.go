// Package release is how pperp's releases are signed and checked (design 15 section 7): minisign signatures (Ed25519
// over BLAKE2b-512, the format the minisign tool reads and writes) on SHA256SUMS and latest.json, made with an offline
// key on the owner's computer, checked by pperp upgrade against the public keys built into it.
package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/scrypt"
)

// PublicKey is a minisign public key.
type PublicKey struct {
	ID  [8]byte
	Key ed25519.PublicKey
}

// IDString is the key id as minisign prints it.
func (p PublicKey) IDString() string {
	return fmt.Sprintf("%016X", binary.LittleEndian.Uint64(p.ID[:]))
}

// payload is line `which` of a minisign file (1: the base64 after its untrusted comment); a key may be its line alone.
func payload(text string, which int) (string, error) {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) == 1 && which == 1 && !strings.HasPrefix(lines[0], "untrusted comment:") {
		return lines[0], nil
	}
	if len(lines) <= which || !strings.HasPrefix(lines[0], "untrusted comment:") {
		return "", errors.New("not a minisign file")
	}
	return lines[which], nil
}

// ParsePublicKey reads a public key file (or its base64 line alone).
func ParsePublicKey(text string) (PublicKey, error) {
	var p PublicKey
	line, err := payload(text, 1)
	if err != nil {
		return p, err
	}
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil || len(b) != 42 || string(b[:2]) != "Ed" {
		return p, errors.New("not a minisign public key")
	}
	copy(p.ID[:], b[2:10])
	p.Key = ed25519.PublicKey(append([]byte(nil), b[10:]...))
	return p, nil
}

// Encode is the public key file.
func (p PublicKey) Encode() string {
	b := append(append([]byte("Ed"), p.ID[:]...), p.Key...)
	return "untrusted comment: minisign public key " + p.IDString() + "\n" + base64.StdEncoding.EncodeToString(b) + "\n"
}

// Verify checks a signature file over msg with one of keys, and answers its trusted comment.
func Verify(keys []PublicKey, msg []byte, sigText string) (string, error) {
	sigLine, err := payload(sigText, 1)
	if err != nil {
		return "", err
	}
	trusted, err := payload(sigText, 2)
	if err != nil || !strings.HasPrefix(trusted, "trusted comment: ") {
		return "", errors.New("the signature has no trusted comment")
	}
	globalLine, err := payload(sigText, 3)
	if err != nil {
		return "", err
	}
	sig, err := base64.StdEncoding.DecodeString(sigLine)
	if err != nil || len(sig) != 74 {
		return "", errors.New("not a minisign signature")
	}
	global, err := base64.StdEncoding.DecodeString(globalLine)
	if err != nil || len(global) != 64 {
		return "", errors.New("not a minisign signature")
	}
	var key *PublicKey
	for i := range keys {
		if bytes.Equal(keys[i].ID[:], sig[2:10]) {
			key = &keys[i]
		}
	}
	if key == nil {
		return "", errors.New("signed with a key this pperp does not know")
	}
	signed := msg
	switch string(sig[:2]) {
	case "ED":
		h := blake2b.Sum512(msg)
		signed = h[:]
	case "Ed":
	default:
		return "", errors.New("an unknown signature algorithm")
	}
	if !ed25519.Verify(key.Key, signed, sig[10:]) {
		return "", errors.New("the signature does not match")
	}
	comment := strings.TrimPrefix(trusted, "trusted comment: ")
	if !ed25519.Verify(key.Key, append(append([]byte(nil), sig[10:]...), comment...), global) {
		return "", errors.New("the trusted comment's signature does not match")
	}
	return comment, nil
}

/* ------------------------------------------------------------------ signing (tools/release, on the owner's computer) */

const (
	opsLimit = 33554432   // libsodium's scrypt OPSLIMIT_SENSITIVE, minisign's default
	memLimit = 1073741824 // MEMLIMIT_SENSITIVE
)

// scryptParams is libsodium's pickparams: the scrypt cost of an ops and memory limit (so minisign reads our keys).
func scryptParams(ops, mem uint64) (n, r, p int) {
	if ops < 32768 {
		ops = 32768
	}
	r = 8
	var logN uint
	if ops < mem/32 {
		p = 1
		maxN := ops / uint64(r*4)
		for logN = 1; logN < 63; logN++ {
			if uint64(1)<<logN > maxN/2 {
				break
			}
		}
	} else {
		maxN := mem / uint64(r*128)
		for logN = 1; logN < 63; logN++ {
			if uint64(1)<<logN > maxN/2 {
				break
			}
		}
		maxrp := (ops / 4) / (uint64(1) << logN)
		if maxrp > 0x3fffffff {
			maxrp = 0x3fffffff
		}
		p = int(maxrp) / r
	}
	return 1 << logN, r, p
}

// SecretKey is a minisign secret key, opened.
type SecretKey struct {
	ID  [8]byte
	Key ed25519.PrivateKey
}

func checksum(id [8]byte, sk []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("Ed"))
	h.Write(id[:])
	h.Write(sk)
	return h.Sum(nil)
}

func keyStream(password string, salt []byte, ops, mem uint64) ([]byte, error) {
	n, r, p := scryptParams(ops, mem)
	return scrypt.Key([]byte(password), salt, n, r, p, 104)
}

// GenerateKey makes a key pair; the secret key file is encrypted with the password (scrypt, minisign's own cost).
func GenerateKey(password string) (PublicKey, string, error) {
	return generateKey(password, opsLimit, memLimit)
}

// NewSecretKey is a new key, in memory only.
func NewSecretKey() (*SecretKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	k := &SecretKey{Key: priv}
	if _, err := rand.Read(k.ID[:]); err != nil {
		return nil, err
	}
	return k, nil
}

func generateKey(password string, ops, mem uint64) (PublicKey, string, error) {
	k, err := NewSecretKey()
	if err != nil {
		return PublicKey{}, "", err
	}
	id, priv := k.ID, k.Key
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return PublicKey{}, "", err
	}
	keynum := append(append(append([]byte(nil), id[:]...), priv...), checksum(id, priv)...)
	stream, err := keyStream(password, salt, ops, mem)
	if err != nil {
		return PublicKey{}, "", err
	}
	for i := range keynum {
		keynum[i] ^= stream[i]
	}
	var b bytes.Buffer
	b.WriteString("EdScB2")
	b.Write(salt)
	_ = binary.Write(&b, binary.LittleEndian, ops)
	_ = binary.Write(&b, binary.LittleEndian, mem)
	b.Write(keynum)
	p := k.Public()
	sec := "untrusted comment: pperp release secret key " + p.IDString() + "\n" + base64.StdEncoding.EncodeToString(b.Bytes()) + "\n"
	return p, sec, nil
}

// OpenSecretKey decrypts a secret key file with its password.
func OpenSecretKey(text, password string) (*SecretKey, error) {
	line, err := payload(text, 1)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil || len(b) != 158 || string(b[:2]) != "Ed" || string(b[4:6]) != "B2" {
		return nil, errors.New("not a minisign secret key")
	}
	keynum := append([]byte(nil), b[54:]...)
	switch string(b[2:4]) {
	case "Sc":
		stream, err := keyStream(password, b[6:38], binary.LittleEndian.Uint64(b[38:46]), binary.LittleEndian.Uint64(b[46:54]))
		if err != nil {
			return nil, err
		}
		for i := range keynum {
			keynum[i] ^= stream[i]
		}
	case "\x00\x00":
	default:
		return nil, errors.New("an unknown key encryption")
	}
	var k SecretKey
	copy(k.ID[:], keynum[:8])
	k.Key = ed25519.PrivateKey(append([]byte(nil), keynum[8:72]...))
	if !bytes.Equal(checksum(k.ID, k.Key), keynum[72:]) {
		return nil, errors.New("wrong password, or a damaged key")
	}
	return &k, nil
}

// Public is the secret key's public key.
func (k *SecretKey) Public() PublicKey {
	return PublicKey{ID: k.ID, Key: k.Key.Public().(ed25519.PublicKey)}
}

// Sign is a signature file over msg (prehashed, as minisign signs by default), with its trusted comment.
func (k *SecretKey) Sign(msg []byte, trustedComment string) string {
	h := blake2b.Sum512(msg)
	sig := ed25519.Sign(k.Key, h[:])
	global := ed25519.Sign(k.Key, append(append([]byte(nil), sig...), trustedComment...))
	line := append(append([]byte("ED"), k.ID[:]...), sig...)
	return "untrusted comment: signature from pperp release key " + k.Public().IDString() + "\n" +
		base64.StdEncoding.EncodeToString(line) + "\n" +
		"trusted comment: " + trustedComment + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n"
}
