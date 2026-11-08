package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

const decryptUsage = `Usage:
  pickle decrypt [-key FILE] [PATH ...]
  pickle decrypt -clean [PATH ...]

PATH is a pickle jar repo, a cluster folder, a namespace folder or one
secret-*.yaml file; the default is the current folder. Next to every
encrypted Secret it writes two files you can kubectl apply:

  secret-<name>.base64.yaml   the Secret with its values under data (base64)
  secret-<name>.raw.yaml      the same Secret with plain values under stringData

They are kept out of git through the clone's .git/info/exclude. If any value
fails to decrypt, nothing is written. -clean removes the written files.

The key is -key, $PICKLE_KEY, ~/.config/pickle/pickle.key or /pickle.key.
`

// decryptedMark starts every file pickle decrypt writes: it tells them apart
// from the jar's own files, for -clean and for the next run.
const decryptedMark = "# pickle decrypt: plain Secret values. Never commit this file.\n"

// runDecrypt is the decrypt command. It works offline, without a cluster or
// any config: only the private key and the jar repo.
func runDecrypt(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("decrypt", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() { fmt.Fprint(stderr, decryptUsage) }
	keyFile := fl.String("key", "", "private key (PEM, RSA)")
	clean := fl.Bool("clean", false, "remove the files pickle decrypt wrote")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	paths := fl.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "pickle decrypt: "+format+"\n", a...)
		return 1
	}

	var files []string
	for _, p := range paths {
		f, err := secretFiles(p)
		if err != nil {
			return fail("%v", err)
		}
		files = append(files, f...)
	}

	if *clean {
		n := 0
		for _, f := range files {
			for _, out := range decryptedPaths(f) {
				if b, err := os.ReadFile(out); err == nil && bytes.HasPrefix(b, []byte(decryptedMark)) {
					if err := os.Remove(out); err != nil {
						return fail("%v", err)
					}
					n++
				}
			}
		}
		fmt.Fprintf(stdout, "removed %s\n", plural(n, "decrypted file"))
		return 0
	}

	kp, err := findKey(*keyFile)
	if err != nil {
		return fail("%v", err)
	}
	key, err := readPrivateKey(kp)
	if err != nil {
		return fail("%v", err)
	}

	// Decrypt everything first; write only if every value opened.
	type output struct{ path, content string }
	var outs []output
	var errs []string
	masked := 0
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		b64, nMasked, ferrs := decryptFile(key, content)
		for _, e := range ferrs {
			errs = append(errs, fmt.Sprintf("%s: %v", f, e))
		}
		if nMasked > 0 {
			masked++
			fmt.Fprintf(stderr, "pickle decrypt: skipped %s: its values are masked (****), not encrypted\n", f)
			continue
		}
		if len(ferrs) > 0 {
			continue
		}
		raw, err := rawSecret(b64)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		p := decryptedPaths(f)
		outs = append(outs, output{p[0], decryptedMark + string(b64)}, output{p[1], decryptedMark + string(raw)})
	}
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(stderr, "pickle decrypt: %s\n", e)
		}
		return fail("nothing written")
	}
	for _, o := range outs {
		if err := os.WriteFile(o.path, []byte(o.content), 0o600); err != nil {
			return fail("%v", err)
		}
	}
	ignored := excludeFromGit(paths[0])
	fmt.Fprintf(stdout, "decrypted %s: *.base64.yaml and *.raw.yaml next to each\n", plural(len(outs)/2, "Secret"))
	if !ignored {
		fmt.Fprintln(stderr, "pickle decrypt: not inside a git clone, so the files are not git-ignored; never commit them")
	}
	if masked > 0 {
		fmt.Fprintf(stderr, "pickle decrypt: %s skipped: written masked, before encryption was on\n", plural(masked, "Secret"))
	}
	return 0
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// decryptedPaths are the two files written for secret-<name>.yaml.
func decryptedPaths(f string) [2]string {
	base := strings.TrimSuffix(f, ".yaml")
	return [2]string{base + ".base64.yaml", base + ".raw.yaml"}
}

func findKey(flagKey string) (string, error) {
	if flagKey != "" {
		return flagKey, nil
	}
	tried := []string{}
	if p := os.Getenv("PICKLE_KEY"); p != "" {
		tried = append(tried, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		tried = append(tried, filepath.Join(home, ".config", "pickle", "pickle.key"))
	}
	tried = append(tried, "/pickle.key")
	for _, p := range tried {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no private key found (tried %s); pass -key", strings.Join(tried, ", "))
}

// secretFiles expands p to the jar's secret-*.yaml files under it, leaving
// out what pickle decrypt wrote.
func secretFiles(p string) ([]string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		if isDecrypted(p) {
			return nil, fmt.Errorf("%s is a decrypted file; pass the encrypted secret-*.yaml", p)
		}
		return []string{p}, nil
	}
	var files []string
	err = filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "secret-") && strings.HasSuffix(d.Name(), ".yaml") && !isDecrypted(f) {
			files = append(files, f)
		}
		return nil
	})
	sort.Strings(files)
	if err == nil && len(files) == 0 {
		err = fmt.Errorf("%s: no secret-*.yaml files", p)
	}
	return files, err
}

func isDecrypted(f string) bool {
	fh, err := os.Open(f)
	if err != nil {
		return false
	}
	defer fh.Close()
	head := make([]byte, len(decryptedMark))
	n, _ := io.ReadFull(fh, head)
	return string(head[:n]) == decryptedMark
}

// excludeFromGit adds the decrypted file names to the clone's
// .git/info/exclude, which is local and never pushed. It reports whether p
// is inside a git clone.
func excludeFromGit(p string) bool {
	dir, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		gitDir := filepath.Join(dir, ".git")
		if st, err := os.Stat(gitDir); err == nil && st.IsDir() {
			ex := filepath.Join(gitDir, "info", "exclude")
			b, _ := os.ReadFile(ex)
			if bytes.Contains(b, []byte(excludeLines)) {
				return true
			}
			if err := os.MkdirAll(filepath.Dir(ex), 0o755); err != nil {
				return false
			}
			if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
				b = append(b, '\n')
			}
			return os.WriteFile(ex, append(b, excludeLines...), 0o644) == nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

const excludeLines = "# pickle decrypt: plain Secret values\nsecret-*.base64.yaml\nsecret-*.raw.yaml\n"

// rawSecret turns a decrypted Secret into the same Secret with plain values
// under stringData. A value that isn't text stays under data, as base64.
func rawSecret(b64 []byte) ([]byte, error) {
	var obj map[string]any
	if err := yaml.Unmarshal(b64, &obj); err != nil {
		return nil, err
	}
	data, _ := obj["data"].(map[string]any)
	plain := map[string]any{}
	for k, v := range data {
		s, _ := v.(string)
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("key %s: %w", k, err)
		}
		if utf8.Valid(b) {
			plain[k] = string(b)
			delete(data, k)
		}
	}
	if len(data) == 0 {
		delete(obj, "data")
	}
	if len(plain) > 0 {
		obj["stringData"] = plain
	}
	j, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return toYAML(j)
}

// encLine is a data line holding an encrypted value: key, then the value,
// quoted or not.
var encLine = regexp.MustCompile(`^(\s+(\S+?):\s+)['"]?(enc:[0-9a-f]+:[A-Za-z0-9+/=]+)['"]?\s*$`)

// maskedLine is a data line pickle wrote masked (****, ****:<fingerprint>).
var maskedLine = regexp.MustCompile(`^\s+\S+:\s+['"]?\*\*\*\*(:[0-9a-f]+)?['"]?\s*$`)

// decryptFile rewrites the enc: values under data: to the API's base64 form
// and leaves every other line as it is. It counts masked values and returns
// every value that failed.
func decryptFile(key *rsa.PrivateKey, content []byte) (out []byte, masked int, errs []error) {
	var b bytes.Buffer
	inData := false
	for _, line := range strings.SplitAfter(string(content), "\n") {
		body := strings.TrimRight(line, "\r\n")
		switch {
		case body == "data:":
			inData = true
		case body != "" && body[0] != ' ' && body[0] != '#':
			inData = false
		case inData:
			if m := encLine.FindStringSubmatch(body); m != nil {
				plain, err := decryptValue(key, m[3])
				if err != nil {
					errs = append(errs, fmt.Errorf("key %s: %w", m[2], err))
					continue
				}
				v := base64.StdEncoding.EncodeToString(plain)
				if v == "" {
					v = `""`
				}
				b.WriteString(m[1] + v + "\n")
				continue
			}
			if maskedLine.MatchString(body) {
				masked++
			}
		}
		b.WriteString(line)
	}
	return b.Bytes(), masked, errs
}

// decryptValue opens one enc:<fingerprint>:<blob> value. A bare blob works too.
func decryptValue(key *rsa.PrivateKey, v string) ([]byte, error) {
	v = strings.Trim(strings.TrimSpace(v), `'"`)
	if strings.HasPrefix(v, encPrefix) {
		parts := strings.SplitN(v, ":", 3)
		if len(parts) != 3 {
			return nil, errors.New("not enc:<fingerprint>:<blob>")
		}
		v = parts[2]
	}
	der, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("blob is not base64: %w", err)
	}
	return cmsDecrypt(key, der)
}

func readPrivateKey(p string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM private key", p)
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an RSA key", p)
		}
		return rk, nil
	case "CERTIFICATE":
		return nil, fmt.Errorf("%s is the certificate; -key needs the private key (pickle.key)", p)
	case "ENCRYPTED PRIVATE KEY":
		return nil, fmt.Errorf("%s is password-protected; write an unprotected copy first: openssl pkey -in %s -out plain.key", p, p)
	}
	return nil, fmt.Errorf("%s: unsupported PEM block %q", p, block.Type)
}

var (
	oidRSAEncryption = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidSHA1          = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidSHA384        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidSHA512        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}
	oidAES128GCM     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 6}
	oidAES192GCM     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 26}
)

type authEnvelopedDataIn struct {
	Version    int
	Originator asn1.RawValue   `asn1:"optional,tag:0"`
	Recipients []asn1.RawValue `asn1:"set"`
	Content    encryptedContentInfo
	AuthAttrs  asn1.RawValue `asn1:"optional,tag:1"`
	MAC        []byte
}

// cmsDecrypt opens a DER CMS AuthEnvelopedData with AES-GCM, as pickle
// writes it and as openssl cms -encrypt -aes-256-gcm makes it. It returns the
// exact value or an error: GCM refuses a damaged or altered blob.
func cmsDecrypt(key *rsa.PrivateKey, der []byte) ([]byte, error) {
	var ci contentInfo
	if rest, err := asn1.Unmarshal(der, &ci); err != nil || len(rest) > 0 {
		return nil, errors.New("blob is damaged or not CMS")
	}
	if !ci.ContentType.Equal(oidAuthEnvelopedData) {
		return nil, errors.New("blob is not CMS AuthEnvelopedData")
	}
	var env authEnvelopedDataIn
	if rest, err := asn1.Unmarshal(ci.Content.Bytes, &env); err != nil || len(rest) > 0 {
		return nil, errors.New("blob is damaged: not a whole AuthEnvelopedData")
	}
	if len(env.AuthAttrs.FullBytes) > 0 {
		return nil, errors.New("unsupported: authenticated attributes")
	}

	var cek []byte
	tried := 0
	for _, r := range env.Recipients {
		var kt keyTransRecipient
		if r.Class != asn1.ClassUniversal || r.Tag != asn1.TagSequence {
			continue // not a key-transport recipient
		}
		if _, err := asn1.Unmarshal(r.FullBytes, &kt); err != nil {
			continue
		}
		tried++
		if k, err := unwrapKey(key, kt); err == nil {
			cek = k
			break
		}
	}
	if cek == nil {
		if tried == 0 {
			return nil, errors.New("blob has no RSA recipient")
		}
		return nil, errors.New("wrong key: this value was encrypted to another certificate")
	}

	ec := env.Content
	keyLen := map[string]int{oidAES256GCM.String(): 32, oidAES192GCM.String(): 24, oidAES128GCM.String(): 16}[ec.Algorithm.Algorithm.String()]
	if keyLen == 0 {
		return nil, fmt.Errorf("unsupported content cipher %v", ec.Algorithm.Algorithm)
	}
	var p gcmParams
	if _, err := asn1.Unmarshal(ec.Algorithm.Parameters.FullBytes, &p); err != nil || len(p.Nonce) == 0 {
		return nil, errors.New("blob is damaged: bad AES-GCM parameters")
	}
	if p.ICVLen == 0 {
		p.ICVLen = 12 // RFC 5084 default
	}
	if len(cek) != keyLen || len(env.MAC) != p.ICVLen {
		return nil, errors.New("blob is damaged: key or tag has the wrong length")
	}
	if len(p.Nonce) != gcmNonceLen {
		return nil, fmt.Errorf("unsupported AES-GCM nonce of %d bytes", len(p.Nonce))
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithTagSize(block, p.ICVLen)
	if err != nil {
		return nil, fmt.Errorf("unsupported AES-GCM parameters: %w", err)
	}
	sealed := append(append([]byte(nil), ec.EncryptedContent...), env.MAC...)
	plain, err := gcm.Open(nil, p.Nonce, sealed, nil)
	if err != nil {
		return nil, errors.New("blob is damaged or altered: AES-GCM authentication failed")
	}
	if plain == nil {
		plain = []byte{}
	}
	return plain, nil
}

func unwrapKey(key *rsa.PrivateKey, kt keyTransRecipient) ([]byte, error) {
	switch alg := kt.KeyAlgorithm; {
	case alg.Algorithm.Equal(oidRSAOAEP):
		h, err := oaepHash(alg.Parameters)
		if err != nil {
			return nil, err
		}
		return rsa.DecryptOAEP(h, nil, key, kt.EncryptedKey, nil)
	case alg.Algorithm.Equal(oidRSAEncryption):
		return rsa.DecryptPKCS1v15(nil, key, kt.EncryptedKey)
	default:
		return nil, fmt.Errorf("unsupported key transport %v", alg.Algorithm)
	}
}

// oaepHash reads RSAES-OAEP-params (RFC 4055), where an absent hash means
// SHA-1. Pickle writes SHA-256 for both the hash and MGF1.
func oaepHash(params asn1.RawValue) (hash.Hash, error) {
	hashOID, mgfOID := oidSHA1, oidSHA1
	if len(params.FullBytes) > 0 {
		var p struct {
			Hash algID `asn1:"optional,explicit,tag:0"`
			MGF  algID `asn1:"optional,explicit,tag:1"`
		}
		if _, err := asn1.Unmarshal(params.FullBytes, &p); err != nil {
			return nil, fmt.Errorf("bad RSA-OAEP parameters: %w", err)
		}
		if p.Hash.Algorithm != nil {
			hashOID = p.Hash.Algorithm
		}
		if p.MGF.Algorithm != nil {
			var mh algID
			if _, err := asn1.Unmarshal(p.MGF.Parameters.FullBytes, &mh); err != nil {
				return nil, fmt.Errorf("bad RSA-OAEP MGF1 parameters: %w", err)
			}
			mgfOID = mh.Algorithm
		}
	}
	if !hashOID.Equal(mgfOID) {
		return nil, errors.New("unsupported RSA-OAEP: MGF1 uses another hash")
	}
	switch {
	case hashOID.Equal(oidSHA256):
		return sha256.New(), nil
	case hashOID.Equal(oidSHA384):
		return sha512.New384(), nil
	case hashOID.Equal(oidSHA512):
		return sha512.New(), nil
	case hashOID.Equal(oidSHA1):
		return sha1.New(), nil
	}
	return nil, fmt.Errorf("unsupported RSA-OAEP hash %v", hashOID)
}
