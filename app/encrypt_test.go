package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCert returns a PEM certificate for a fresh RSA key, and the key file
// written for openssl. The certificate is already expired: dates must not
// matter.
func testCert(t testing.TB, bits int) (certPEM []byte, keyFile string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "pickle"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	keyFile = filepath.Join(t.TempDir(), "pickle.key")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyFile
}

// opensslDecrypt decrypts an enc:<fingerprint>:<blob> value the way the docs
// tell people to.
func opensslDecrypt(t testing.TB, keyFile, value string) string {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 || parts[0] != "enc" {
		t.Fatalf("not an encrypted value: %q", value)
	}
	cmd := exec.Command("sh", "-c", `base64 -d | openssl cms -decrypt -inform DER -inkey "$1"`, "sh", keyFile)
	cmd.Stdin = strings.NewReader(parts[2])
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("openssl could not decrypt: %v\n%s", err, stderr.String())
	}
	return out.String()
}

var testFPKey = []byte("0123456789abcdef0123456789abcdef")

func testEncrypter(t testing.TB) (*encrypter, string) {
	t.Helper()
	certPEM, keyFile := testCert(t, 2048)
	e, err := newEncrypter(certPEM, testFPKey)
	if err != nil {
		t.Fatal(err)
	}
	return e, keyFile
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestSealDecryptsWithOpenSSL(t *testing.T) {
	e, keyFile := testEncrypter(t)
	for _, plain := range []string{"", "s3cret", "exactly16bytes!!", "multi\nline\x00binary ✓", strings.Repeat("-----BEGIN CERTIFICATE-----", 300)} {
		sealed, _, _ := e.seal("ns/db", map[string]any{"k": b64(plain)}, nil)
		v := sealed["k"].(string)
		if !strings.HasPrefix(v, "enc:"+fingerprint(b64(plain), e.fpKey)+":") || strings.ContainsAny(v, " \n") {
			t.Fatalf("want one line enc:<fingerprint>:<blob>, got %q", v)
		}
		if got := opensslDecrypt(t, keyFile, v); got != plain {
			t.Fatalf("decrypted %q, want %q", got, plain)
		}
	}
}

func TestSealKeepsUnchangedLines(t *testing.T) {
	e, _ := testEncrypter(t)
	data := map[string]any{"A": b64("a"), "B": b64("b")}
	first, n, _ := e.seal("ns/db", data, nil)
	if n != 2 {
		t.Fatalf("encrypted %d values, want 2", n)
	}
	if again, _, _ := e.seal("ns/db", data, nil); again["A"] == first["A"] {
		t.Fatal("encryption must be randomized, or equal values would show in Git")
	}
	prev := map[string]string{"A": first["A"].(string), "B": first["B"].(string)}
	next, n, _ := e.seal("ns/db", map[string]any{"A": b64("a"), "B": b64("changed")}, prev)
	if n != 1 {
		t.Errorf("encrypted %d values, want only the changed one", n)
	}
	if next["A"] != first["A"] {
		t.Error("an unchanged value must keep its line")
	}
	if next["B"] == first["B"] {
		t.Error("a changed value must be encrypted again")
	}
	fp := fingerprint(b64("a"), e.fpKey)
	for _, old := range []string{"****:" + fp, "enc:" + fp + ":", "****", ""} {
		if out, _, _ := e.seal("ns/db", data, map[string]string{"A": old}); out["A"] == old {
			t.Errorf("%q must not be reused", old)
		}
	}
}

func TestSealNeverWritesPlain(t *testing.T) {
	e, _ := testEncrypter(t)
	out, _, failed := e.seal("ns/db", map[string]any{"bad": "not base64!", "num": 3}, nil)
	if failed != 2 || out["bad"] != "****:"+fingerprint("not base64!", e.fpKey) || out["num"] != "****" {
		t.Errorf("values that can't be encrypted must be masked: %v", out)
	}
}

func TestNewEncrypter(t *testing.T) {
	good, _ := testCert(t, 2048)
	small, _ := testCert(t, 1024)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &ek.PublicKey, ek)
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for name, tc := range map[string]struct {
		cert, key []byte
		ok        bool
	}{
		"expired RSA 2048":   {good, testFPKey, true},
		"no fingerprint key": {good, nil, false},
		"RSA 1024":           {small, testFPKey, false},
		"ECDSA":              {ecPEM, testFPKey, false},
		"not PEM":            {[]byte("garbage"), testFPKey, false},
	} {
		if _, err := newEncrypter(tc.cert, tc.key); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	other, _ := testCert(t, 2048)
	a, _ := newEncrypter(good, testFPKey)
	b, _ := newEncrypter(other, testFPKey)
	if bytes.Equal(a.fpKey, b.fpKey) {
		t.Error("a new certificate must change every fingerprint")
	}
}

func TestReadEncryptionCert(t *testing.T) {
	dir := t.TempDir()
	if e, err := readEncryptionCert(filepath.Join(dir, "missing"), testFPKey); e != nil || err != nil {
		t.Fatalf("no file must turn encryption off: %v %v", e, err)
	}
	certPEM, _ := testCert(t, 2048)
	p := filepath.Join(dir, "encryption-cert")
	if err := os.WriteFile(p, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEncryptionCert(p, nil); err == nil || !strings.Contains(err.Error(), "fingerprint-key") {
		t.Fatalf("a cert without fingerprint-key must be refused: %v", err)
	}
	if e, err := readEncryptionCert(p, testFPKey); e == nil || err != nil {
		t.Fatalf("expected encryption on: %v", err)
	}
}

// BenchmarkSealUnchanged is the steady state: values already written.
func BenchmarkSealUnchanged(b *testing.B) {
	e, _ := testEncrypter(b)
	data := map[string]any{"A": b64("user"), "B": b64("password"), "C": b64(strings.Repeat("x", 4096))}
	prev := map[string]string{}
	first, _, _ := e.seal("ns/db", data, nil)
	for k, v := range first {
		prev[k] = v.(string)
	}
	b.ReportAllocs()
	for b.Loop() {
		e.seal("ns/db", data, prev)
	}
}

// BenchmarkSealChanged encrypts three values from scratch.
func BenchmarkSealChanged(b *testing.B) {
	e, _ := testEncrypter(b)
	data := map[string]any{"A": b64("user"), "B": b64("password"), "C": b64(strings.Repeat("x", 4096))}
	b.ReportAllocs()
	for b.Loop() {
		e.seal("ns/db", data, nil)
	}
}
