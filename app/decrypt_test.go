package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	goldenKey    = "testdata/decrypt/pickle.key"
	goldenSecret = "testdata/decrypt/secret-golden.yaml"
	goldenSource = "testdata/decrypt/source.yaml"
)

func secretData(t testing.TB, content []byte) map[string]string {
	t.Helper()
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal(content, &s); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, content)
	}
	return s.Data
}

func mustKey(t testing.TB, p string) *rsa.PrivateKey {
	t.Helper()
	k, err := readPrivateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestGoldenDecrypts pins the format: a file pickle wrote must decrypt to
// the original values for good, whatever changes in the code that writes.
func TestGoldenDecrypts(t *testing.T) {
	want := secretData(t, mustRead(t, goldenSource))
	plain, masked, errs := decryptFile(mustKey(t, goldenKey), mustRead(t, goldenSecret))
	if len(errs) > 0 || masked != 0 {
		t.Fatalf("errors %v, masked %d", errs, masked)
	}
	got := secretData(t, plain)
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if !bytes.Contains(plain, []byte("name: golden")) || !bytes.Contains(plain, []byte("app: payments")) {
		t.Error("lines outside data must stay as they are")
	}
}

// TestGoldenDecryptsWithOpenSSL decrypts every golden value with plain
// openssl, the documented way.
func TestGoldenDecryptsWithOpenSSL(t *testing.T) {
	want := secretData(t, mustRead(t, goldenSource))
	for k, v := range secretData(t, mustRead(t, goldenSecret)) {
		plain, _ := base64.StdEncoding.DecodeString(want[k])
		if got := opensslDecrypt(t, goldenKey, v); got != string(plain) {
			t.Errorf("%s: openssl gave %q", k, got)
		}
	}
}

// TestDocsDecryptFunction runs pickle_decrypt exactly as docs/restore.md
// defines it, on the golden file.
func TestDocsDecryptFunction(t *testing.T) {
	for _, tool := range []string{"bash", "openssl", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	doc := string(mustRead(t, "../docs/restore.md"))
	fn := regexp.MustCompile(`(?m)^pickle_decrypt\(\) .*$`).FindString(doc)
	if fn == "" {
		t.Fatal("pickle_decrypt not found in docs/restore.md")
	}
	key, _ := filepath.Abs(goldenKey)
	fn = strings.ReplaceAll(fn, "~/.config/pickle/pickle.key", key)
	cmd := exec.Command("bash", "-c", fn+"\npickle_decrypt \"$1\"", "bash", goldenSecret)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	want := secretData(t, mustRead(t, goldenSource))
	got := secretData(t, out)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if !bytes.Equal(out, must(decryptFile(mustKey(t, goldenKey), mustRead(t, goldenSecret)))) {
		t.Error("pickle_decrypt and pickle decrypt must print the same bytes")
	}

	// A damaged value: the function fails and prints nothing.
	golden := string(mustRead(t, goldenSecret))
	v := secretData(t, []byte(golden))["DB_PASSWORD"]
	der, _ := base64.StdEncoding.DecodeString(v[strings.LastIndex(v, ":")+1:])
	der[len(der)-20] ^= 1
	bad := filepath.Join(t.TempDir(), "secret-bad.yaml")
	os.WriteFile(bad, []byte(strings.Replace(golden, v, "enc:273e83a494cc:"+base64.StdEncoding.EncodeToString(der), 1)), 0o644)
	cmd = exec.Command("bash", "-c", fn+"\npickle_decrypt \"$1\"", "bash", bad)
	out, err = cmd.Output()
	if err == nil || len(out) > 0 {
		t.Errorf("a damaged value must fail and print nothing: err %v, %d bytes printed", err, len(out))
	}
}

func must(b []byte, _ int, errs []error) []byte {
	if len(errs) > 0 {
		panic(errs[0])
	}
	return b
}

// TestDecryptOpenSSLBlobs: values encrypted with openssl itself open too.
func TestDecryptOpenSSLBlobs(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	key := mustKey(t, goldenKey)
	for name, args := range map[string][]string{
		"pkcs1":       {"-aes-256-gcm"},
		"aes128":      {"-aes-128-gcm"},
		"oaep-sha256": {"-aes-256-gcm", "-keyopt", "rsa_padding_mode:oaep", "-keyopt", "rsa_oaep_md:sha256", "-keyopt", "rsa_mgf1_md:sha256"},
		"oaep-sha1":   {"-aes-192-gcm", "-keyopt", "rsa_padding_mode:oaep"},
	} {
		cmd := exec.Command("openssl", append([]string{"cms", "-encrypt", "-binary", "-outform", "DER", "-recip", "testdata/decrypt/pickle.crt"}, args...)...)
		cmd.Stdin = strings.NewReader("from openssl")
		der, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: openssl: %v", name, err)
		}
		got, err := cmsDecrypt(key, der)
		if err != nil || string(got) != "from openssl" {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
}

func TestDecryptRoundTrip(t *testing.T) {
	e, keyFile := testEncrypter(t)
	key := mustKey(t, keyFile)
	for _, n := range []int{0, 1, 15, 16, 17, 255, 4096, 1 << 20} {
		plain := make([]byte, n)
		rand.Read(plain)
		sealed, _, _ := e.seal("ns/db", map[string]any{"k": base64.StdEncoding.EncodeToString(plain)}, nil)
		got, err := decryptValue(key, sealed["k"].(string))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%d bytes: %v", n, err)
		}
	}
}

func TestDecryptFailures(t *testing.T) {
	e, keyFile := testEncrypter(t)
	_, otherKey := testEncrypter(t)
	sealed, _, _ := e.seal("ns/db", map[string]any{"k": b64("a value long enough to damage")}, nil)
	v := sealed["k"].(string)
	der, _ := base64.StdEncoding.DecodeString(v[strings.LastIndex(v, ":")+1:])
	for name, tc := range map[string]struct {
		key, value, want string
	}{
		"wrong key":  {otherKey, v, "wrong key"},
		"not base64": {keyFile, "enc:abc:!!!", "not base64"},
		"not CMS":    {keyFile, "enc:abc:" + b64("hello"), "not CMS"},
		"no fields":  {keyFile, "enc:abc", "enc:<fingerprint>:<blob>"},
		"masked":     {keyFile, "****:abc", "not base64"},
		"truncated":  {keyFile, "enc:abc:" + base64.StdEncoding.EncodeToString(der[:len(der)-1]), "damaged"},
	} {
		if _, err := decryptValue(mustKey(t, tc.key), tc.value); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// Any single flipped byte is caught: never wrong bytes without an error.
	key := mustKey(t, keyFile)
	for i := range der {
		bad := bytes.Clone(der)
		bad[i] ^= 0x01
		if got, err := cmsDecrypt(key, bad); err == nil && string(got) != "a value long enough to damage" {
			t.Fatalf("byte %d flipped: decrypted to %q without an error", i, got)
		}
	}
}

func TestReadPrivateKey(t *testing.T) {
	dir := t.TempDir()
	k := mustKey(t, goldenKey)
	pkcs1 := filepath.Join(dir, "pkcs1.key")
	os.WriteFile(pkcs1, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600)
	if got, err := readPrivateKey(pkcs1); err != nil || !got.Equal(k) {
		t.Errorf("PKCS#1: %v", err)
	}
	enc := filepath.Join(dir, "enc.key")
	os.WriteFile(enc, []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n"), 0o600)
	if _, err := readPrivateKey(enc); err == nil || !strings.Contains(err.Error(), "password-protected") {
		t.Errorf("encrypted key: %v", err)
	}
	if _, err := readPrivateKey("testdata/decrypt/pickle.crt"); err == nil {
		t.Error("a certificate is not a private key")
	}
}

func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runDecrypt(args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDecryptCommand(t *testing.T) {
	want := secretData(t, mustRead(t, goldenSource))
	golden := string(mustRead(t, goldenSecret))

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "stg", "payments"), 0o755)
	os.MkdirAll(filepath.Join(dir, "stg", "web"), 0o755)
	os.WriteFile(filepath.Join(dir, "stg", "payments", "secret-golden.yaml"), []byte(golden), 0o644)
	os.WriteFile(filepath.Join(dir, "stg", "web", "secret-golden.yaml"), []byte(golden), 0o644)
	os.WriteFile(filepath.Join(dir, "stg", "web", "configmap-x.yaml"), []byte("kind: ConfigMap\n"), 0o644)

	code, out, errOut := runCLI(t, "", "-key", goldenKey, filepath.Join(dir, "stg"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := strings.Split(out, "---\n")
	if len(docs) != 2 || strings.Contains(out, "ConfigMap") {
		t.Fatalf("want the 2 Secrets of the directory, got:\n%s", out)
	}
	for _, d := range docs {
		if got := secretData(t, []byte(d)); got["DB_PASSWORD"] != want["DB_PASSWORD"] {
			t.Errorf("DB_PASSWORD = %q", got["DB_PASSWORD"])
		}
	}

	if code, out, _ := runCLI(t, golden, "-key", goldenKey); code != 0 || secretData(t, []byte(out))["unicode"] != want["unicode"] {
		t.Errorf("stdin: exit %d", code)
	}

	v := secretData(t, []byte(golden))["DB_PASSWORD"]
	plain, _ := base64.StdEncoding.DecodeString(want["DB_PASSWORD"])
	if code, out, _ := runCLI(t, "", "-key", goldenKey, "-value", v); code != 0 || out != string(plain) {
		t.Errorf("-value: exit %d, %q", code, out)
	}

	// One bad value: nothing at all is printed, so a pipe into kubectl
	// apply applies nothing half-decrypted.
	bad := strings.Replace(golden, v, "enc:273e83a494cc:"+b64("garbage"), 1)
	os.WriteFile(filepath.Join(dir, "stg", "web", "secret-golden.yaml"), []byte(bad), 0o644)
	code, out, errOut = runCLI(t, "", "-key", goldenKey, filepath.Join(dir, "stg"))
	if code != 1 || out != "" || !strings.Contains(errOut, "key DB_PASSWORD") || !strings.Contains(errOut, "nothing printed") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	masked := "apiVersion: v1\ndata:\n  A: '****:3f9a1c7e5b2d'\nkind: Secret\n"
	if code, out, errOut := runCLI(t, masked, "-key", goldenKey); code != 0 || out != masked || !strings.Contains(errOut, "masked") {
		t.Errorf("masked file: exit %d, %q, %q", code, out, errOut)
	}

	if code, _, _ := runCLI(t, "", filepath.Join(dir, "stg")); code != 2 {
		t.Errorf("without -key: exit %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "", "-key", goldenKey, filepath.Join(dir, "nope")); code != 1 {
		t.Errorf("missing path: exit %d, want 1", code)
	}
}

// TestDecryptOnlyTouchesData: an enc:-looking line outside data stays.
func TestDecryptOnlyTouchesData(t *testing.T) {
	golden := string(mustRead(t, goldenSecret))
	v := secretData(t, []byte(golden))["DB_PASSWORD"]
	in := strings.Replace(golden, "  labels:\n", "  annotations:\n    note: "+v+"\n  labels:\n", 1)
	out, _, errs := decryptFile(mustKey(t, goldenKey), []byte(in))
	if len(errs) > 0 || !strings.Contains(string(out), "note: "+v) {
		t.Errorf("a metadata line was changed: %v", errs)
	}
}
