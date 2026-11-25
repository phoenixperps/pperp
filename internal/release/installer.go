package release

// The installers' signature (Q50): install.sh and install.ps1 run where minisign is rarely installed and PowerShell
// 5.1 has no Ed25519, so SHA256SUMS also carries an ECDSA P-256 signature that openssl, LibreSSL and .NET 4.7+ all
// check, with the public key pinned in the installers themselves (never fetched from the server the files come from).
// The P-256 key is derived from the release key (HKDF-SHA256 over its seed): kept, protected and rotated with it, one
// password for both signatures.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
)

const installerInfo = "pperp installer signature P-256 v1"

// InstallerKey is the release key's P-256 key for the installers' signature.
func (k *SecretKey) InstallerKey() (*ecdsa.PrivateKey, error) {
	return installerKeyOf(k.Key.Seed())
}

func installerKeyOf(seed []byte) (*ecdsa.PrivateKey, error) {
	wide, err := hkdf.Key(sha256.New, seed, nil, installerInfo, 48) // 384 bits reduced mod n: no measurable bias
	if err != nil {
		return nil, err
	}
	n := elliptic.P256().Params().N
	d := new(big.Int).SetBytes(wide)
	d.Mod(d, new(big.Int).Sub(n, big.NewInt(1)))
	d.Add(d, big.NewInt(1))
	return ecdsa.ParseRawPrivateKey(elliptic.P256(), d.FillBytes(make([]byte, 32)))
}

// NewInstallerKey is a throwaway P-256 key (the installers' tests).
func NewInstallerKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// SignInstaller signs msg (SHA-256): DER for openssl and LibreSSL, r||s (IEEE P1363) for .NET.
func SignInstaller(k *ecdsa.PrivateKey, msg []byte) (der, p1363 []byte, err error) {
	h := sha256.Sum256(msg)
	der, err = ecdsa.SignASN1(rand.Reader, k, h[:])
	if err != nil {
		return nil, nil, err
	}
	var rs struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &rs); err != nil {
		return nil, nil, err
	}
	p1363 = make([]byte, 64)
	rs.R.FillBytes(p1363[:32])
	rs.S.FillBytes(p1363[32:])
	return der, p1363, nil
}

// InstallerPEM is the public key as install.sh pins it (PKIX, PEM).
func InstallerPEM(k *ecdsa.PublicKey) (string, error) {
	b, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b})), nil
}

// InstallerXY is the public key as install.ps1 pins it: X and Y, base64.
func InstallerXY(k *ecdsa.PublicKey) (x, y string, err error) {
	raw, err := k.Bytes() // 0x04 || X || Y
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(raw[1:33]), base64.StdEncoding.EncodeToString(raw[33:]), nil
}

// VerifyInstaller checks a DER signature against a pinned PEM key, as install.sh does with openssl.
func VerifyInstaller(pemText string, msg, der []byte) error {
	b, _ := pem.Decode([]byte(pemText))
	if b == nil {
		return errors.New("not a PEM public key")
	}
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return err
	}
	k, ok := pub.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return errors.New("not a P-256 key")
	}
	h := sha256.Sum256(msg)
	if !ecdsa.VerifyASN1(k, h[:], der) {
		return errors.New("the installer signature does not match")
	}
	return nil
}
