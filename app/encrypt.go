package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
)

// encPrefix starts every encrypted Secret value: enc:<fingerprint>:<blob>.
// The blob is the base64 of a DER CMS AuthEnvelopedData (RFC 5083) holding
// the plain value, so it opens with OpenSSL 3 and the private key:
//
//	echo '<blob>' | base64 -d | openssl cms -decrypt -inform DER -inkey pickle.key
const encPrefix = "enc:"

// minRSABits: values stay in Git history for good, so no weak keys.
const minRSABits = 2048

// encrypter seals Secret values to one RSA certificate. Pickle only has the
// public key, so it can encrypt but never decrypt. Certificate dates are not
// checked: the certificate only carries the key.
type encrypter struct {
	pub    *rsa.PublicKey
	issuer asn1.RawValue
	serial *big.Int
	// fpKey keys the fingerprints. It mixes in the certificate, so a new
	// certificate changes every fingerprint and every value is encrypted
	// again, instead of keeping blobs only the old key can open.
	fpKey []byte
}

func newEncrypter(certPEM, fingerprintKey []byte) (*encrypter, error) {
	if len(fingerprintKey) == 0 {
		return nil, errors.New("needs fingerprint-key in the pickle-git Secret too (openssl rand -hex 32)")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate (-----BEGIN CERTIFICATE-----)")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("the certificate key must be RSA")
	}
	if pub.N.BitLen() < minRSABits {
		return nil, fmt.Errorf("the RSA key must have at least %d bits, not %d", minRSABits, pub.N.BitLen())
	}
	sum := sha256.Sum256(cert.Raw)
	mac := hmac.New(sha256.New, fingerprintKey)
	mac.Write(sum[:])
	return &encrypter{
		pub:    pub,
		issuer: asn1.RawValue{FullBytes: cert.RawIssuer},
		serial: cert.SerialNumber,
		fpKey:  mac.Sum(nil),
	}, nil
}

// seal returns data with every value as enc:<fingerprint>:<blob>. A value
// whose fingerprint matches its line in prev (the file as last written)
// keeps that line, so only changed keys show up in a diff and only they cost
// an encryption. A value that can't be encrypted is masked, never left plain.
// encrypted and failed count the values encrypted now and those masked.
func (e *encrypter) seal(secret string, data map[string]any, prev map[string]string) (out map[string]any, encrypted, failed int) {
	out = make(map[string]any, len(data))
	for k, v := range data {
		s, ok := v.(string)
		if !ok {
			out[k] = secretMask
			failed++
			continue
		}
		fp := fingerprint(s, e.fpKey)
		head := encPrefix + fp + ":"
		if old := prev[k]; len(old) > len(head) && strings.HasPrefix(old, head) {
			out[k] = old
			continue
		}
		blob, err := e.sealValue(s)
		if err != nil {
			slog.Warn("secret value not encrypted, written masked", "secret", secret, "key", k, "err", err)
			out[k] = secretMask + ":" + fp
			failed++
			continue
		}
		out[k] = head + blob
		encrypted++
	}
	return out, encrypted, failed
}

// sealValue encrypts the plain value inside v, the API's base64 form, and
// returns the blob as base64.
func (e *encrypter) sealValue(v string) (string, error) {
	plain, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", fmt.Errorf("value is not base64: %w", err)
	}
	der, err := e.encrypt(plain)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

var (
	oidData              = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidAuthEnvelopedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 23}
	oidRSAOAEP           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 7}
	oidMGF1              = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 8}
	oidSHA256            = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidAES256GCM         = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 46}
)

type algID struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type oaepParams struct {
	Hash algID `asn1:"explicit,tag:0"`
	MGF  algID `asn1:"explicit,tag:1"`
}

type issuerAndSerial struct {
	Issuer asn1.RawValue
	Serial *big.Int
}

type keyTransRecipient struct {
	Version      int
	RID          issuerAndSerial
	KeyAlgorithm algID
	EncryptedKey []byte
}

type encryptedContentInfo struct {
	ContentType      asn1.ObjectIdentifier
	Algorithm        algID
	EncryptedContent []byte `asn1:"tag:0"`
}

type gcmParams struct {
	Nonce  []byte
	ICVLen int
}

type authEnvelopedData struct {
	Version    int
	Recipients []keyTransRecipient `asn1:"set"`
	Content    encryptedContentInfo
	MAC        []byte
}

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue
}

func rawDER(v any) (asn1.RawValue, error) {
	b, err := asn1.Marshal(v)
	return asn1.RawValue{FullBytes: b}, err
}

// encrypt seals plain as DER CMS AuthEnvelopedData: a fresh AES-256-GCM key
// encrypts and authenticates the value, and RSA-OAEP (SHA-256) encrypts that
// key to the certificate. A damaged blob fails to decrypt instead of giving
// wrong bytes. The output is different every time, even for the same value.
func (e *encrypter) encrypt(plain []byte) ([]byte, error) {
	key := make([]byte, 32+gcmNonceLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	key, nonce := key[:32], key[32:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plain, nil)
	ct, tag := sealed[:len(plain)], sealed[len(plain):]

	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, e.pub, key, nil)
	if err != nil {
		return nil, err
	}
	sha := algID{Algorithm: oidSHA256}
	mgfParams, err := rawDER(sha)
	if err != nil {
		return nil, err
	}
	oaep, err := rawDER(oaepParams{Hash: sha, MGF: algID{Algorithm: oidMGF1, Parameters: mgfParams}})
	if err != nil {
		return nil, err
	}
	gcmParam, err := rawDER(gcmParams{Nonce: nonce, ICVLen: len(tag)})
	if err != nil {
		return nil, err
	}
	env, err := asn1.Marshal(authEnvelopedData{
		Recipients: []keyTransRecipient{{
			RID:          issuerAndSerial{Issuer: e.issuer, Serial: e.serial},
			KeyAlgorithm: algID{Algorithm: oidRSAOAEP, Parameters: oaep},
			EncryptedKey: wrapped,
		}},
		Content: encryptedContentInfo{
			ContentType:      oidData,
			Algorithm:        algID{Algorithm: oidAES256GCM, Parameters: gcmParam},
			EncryptedContent: ct,
		},
		MAC: tag,
	})
	if err != nil {
		return nil, err
	}
	// [0] EXPLICIT: asn1 ignores struct tags on a RawValue, so tag it here.
	return asn1.Marshal(contentInfo{
		ContentType: oidAuthEnvelopedData,
		Content:     asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: env},
	})
}

const gcmNonceLen = 12
