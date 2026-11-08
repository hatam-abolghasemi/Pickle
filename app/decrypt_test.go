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

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runDecrypt(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// jar lays out a pickle jar clone: two clusters, Secrets in two namespaces,
// and a ConfigMap that must be left alone.
func jar(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	golden := mustRead(t, goldenSecret)
	for _, p := range []string{"stg/payments/secret-golden.yaml", "stg/web/secret-golden.yaml", "prod/payments/secret-golden.yaml"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), golden, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "stg/web/configmap-x.yaml"), []byte("kind: ConfigMap\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755)
	return dir
}

func TestDecryptCommand(t *testing.T) {
	dir := jar(t)
	want := secretData(t, mustRead(t, goldenSource))

	code, out, errOut := runCLI(t, "-key", goldenKey, filepath.Join(dir, "stg"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "decrypted 2 Secrets") {
		t.Errorf("stdout %q", out)
	}
	for _, ns := range []string{"payments", "web"} {
		base := filepath.Join(dir, "stg", ns, "secret-golden")
		b64 := mustRead(t, base+".base64.yaml")
		if !bytes.HasPrefix(b64, []byte(decryptedMark)) {
			t.Error("decrypted files must start with the mark")
		}
		if got := secretData(t, b64); len(got) != len(want) {
			t.Errorf("base64 has %d keys", len(got))
		} else {
			for k, v := range want {
				if got[k] != v && !(v == "" && got[k] == "") {
					t.Errorf("base64 %s = %q, want %q", k, got[k], v)
				}
			}
		}
		if bytes.Contains(b64, []byte("empty: \n")) || !bytes.Contains(b64, []byte(`empty: ""`)) {
			t.Error(`an empty value must be "" in the base64 file, not null`)
		}
		// The raw file holds the same values in plain text, exactly.
		var raw struct {
			Data       map[string]string `json:"data"`
			StringData map[string]string `json:"stringData"`
		}
		if err := yaml.Unmarshal(mustRead(t, base+".raw.yaml"), &raw); err != nil {
			t.Fatal(err)
		}
		for k, v := range want {
			plain, _ := base64.StdEncoding.DecodeString(v)
			if s, ok := raw.StringData[k]; ok {
				if s != string(plain) {
					t.Errorf("raw %s = %q, want %q", k, s, plain)
				}
			} else if raw.Data[k] != v {
				t.Errorf("raw %s: binary value must stay base64 under data, got %q", k, raw.Data[k])
			}
		}
		if _, ok := raw.Data["binary"]; !ok || raw.StringData["tls.crt"] == "" {
			t.Errorf("raw file: binary under data, text under stringData: %+v", raw)
		}
		if st, _ := os.Stat(base + ".raw.yaml"); st.Mode().Perm() != 0o600 {
			t.Errorf("decrypted files must be 0600, got %v", st.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "prod/payments/secret-golden.base64.yaml")); err == nil {
		t.Error("only the given folder must be decrypted")
	}
	if _, err := os.Stat(filepath.Join(dir, "stg/web/configmap-x.base64.yaml")); err == nil {
		t.Error("only Secrets must be decrypted")
	}
	ex := string(mustRead(t, filepath.Join(dir, ".git/info/exclude")))
	if !strings.Contains(ex, "secret-*.raw.yaml") || !strings.Contains(ex, "secret-*.base64.yaml") {
		t.Errorf("decrypted files must be git-ignored: %q", ex)
	}

	// A second run on the whole jar skips what it wrote and adds no second
	// exclude block.
	if code, out, errOut := runCLI(t, "-key", goldenKey, dir); code != 0 || !strings.Contains(out, "decrypted 3 Secrets") {
		t.Fatalf("whole jar: exit %d, %q, %q", code, out, errOut)
	}
	if strings.Count(string(mustRead(t, filepath.Join(dir, ".git/info/exclude"))), "secret-*.raw.yaml") != 1 {
		t.Error("the exclude lines must be added once")
	}

	// One file.
	if code, _, errOut := runCLI(t, "-key", goldenKey, filepath.Join(dir, "stg/web/secret-golden.yaml")); code != 0 {
		t.Errorf("one file: %s", errOut)
	}
	if code, _, _ := runCLI(t, "-key", goldenKey, filepath.Join(dir, "stg/web/secret-golden.raw.yaml")); code != 1 {
		t.Error("a decrypted file is not input")
	}

	// -clean removes exactly what decrypt wrote.
	if code, out, _ := runCLI(t, "-clean", dir); code != 0 || !strings.Contains(out, "removed 6") {
		t.Errorf("clean: exit %d, %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "stg/web/secret-golden.yaml")); err != nil {
		t.Error("-clean must keep the encrypted files")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*/*/*.raw.yaml")); len(m) > 0 {
		t.Errorf("left after -clean: %v", m)
	}
}

// TestDecryptAllOrNothing: one bad value anywhere, and nothing is written.
func TestDecryptAllOrNothing(t *testing.T) {
	dir := jar(t)
	golden := string(mustRead(t, goldenSecret))
	v := secretData(t, []byte(golden))["DB_PASSWORD"]
	der, _ := base64.StdEncoding.DecodeString(v[strings.LastIndex(v, ":")+1:])
	der[len(der)-20] ^= 1
	os.WriteFile(filepath.Join(dir, "stg/web/secret-golden.yaml"), []byte(strings.Replace(golden, v, "enc:273e83a494cc:"+base64.StdEncoding.EncodeToString(der), 1)), 0o644)
	code, _, errOut := runCLI(t, "-key", goldenKey, dir)
	if code != 1 || !strings.Contains(errOut, "key DB_PASSWORD") || !strings.Contains(errOut, "nothing written") {
		t.Errorf("exit %d, %q", code, errOut)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*/*/*.base64.yaml")); len(m) > 0 {
		t.Errorf("written despite a failure: %v", m)
	}
	if code, _, errOut := runCLI(t, "-key", "testdata/decrypt/pickle.crt", dir); code != 1 || !strings.Contains(errOut, "certificate") {
		t.Errorf("a certificate as key: exit %d, %q", code, errOut)
	}
}

// TestDecryptSkipsMasked: Secrets written before encryption was on are
// skipped with a note; the rest are decrypted.
func TestDecryptSkipsMasked(t *testing.T) {
	dir := jar(t)
	os.WriteFile(filepath.Join(dir, "stg/web/secret-old.yaml"), []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: old\ndata:\n  A: '****:3f9a1c7e5b2d'\n"), 0o644)
	code, out, errOut := runCLI(t, "-key", goldenKey, filepath.Join(dir, "stg"))
	if code != 0 || !strings.Contains(out, "decrypted 2") || !strings.Contains(errOut, "skipped") {
		t.Errorf("exit %d, %q, %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "stg/web/secret-old.raw.yaml")); err == nil {
		t.Error("a masked Secret must not get decrypted files")
	}
}

func TestFindKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PICKLE_KEY", "")
	if _, err := findKey(""); err == nil || !strings.Contains(err.Error(), "-key") {
		t.Errorf("no key: %v", err)
	}
	p := filepath.Join(home, ".config", "pickle", "pickle.key")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, mustRead(t, goldenKey), 0o600)
	if got, err := findKey(""); err != nil || got != p {
		t.Errorf("default key: %q, %v", got, err)
	}
	other := filepath.Join(home, "other.key")
	os.WriteFile(other, mustRead(t, goldenKey), 0o600)
	t.Setenv("PICKLE_KEY", other)
	if got, _ := findKey(""); got != other {
		t.Errorf("PICKLE_KEY: %q", got)
	}
	if got, _ := findKey("x.key"); got != "x.key" {
		t.Errorf("-key wins: %q", got)
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
