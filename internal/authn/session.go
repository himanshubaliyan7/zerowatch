package authn

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// sealer makes the stateless cookies: AES-256-GCM encrypts the JSON, then an
// HMAC-SHA256 over the ciphertext signs it (encrypt-then-MAC). Both keys are
// derived from the one session key, and the purpose ("session", "flow") is
// bound into the AAD and the MAC so one cookie can't be replayed as the other.
type sealer struct {
	aead cipher.AEAD
	mac  []byte
}

var errBadSeal = errors.New("invalid or tampered cookie")

func newSealer(key []byte) (*sealer, error) {
	if len(key) < 32 {
		return nil, errors.New("session key must be at least 32 bytes")
	}
	block, err := aes.NewCipher(derive(key, "zerowatch session enc v1"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead, mac: derive(key, "zerowatch session mac v1")}, nil
}

func derive(key []byte, label string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

func (s *sealer) seal(purpose string, v any) (string, error) {
	pt, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, pt, []byte(purpose)))
	return body + "." + base64.RawURLEncoding.EncodeToString(s.tag(purpose, body)), nil
}

func (s *sealer) open(purpose, sealed string, v any) error {
	body, tagB64, ok := strings.Cut(sealed, ".")
	if !ok {
		return errBadSeal
	}
	tag, err := base64.RawURLEncoding.DecodeString(tagB64)
	if err != nil || !hmac.Equal(tag, s.tag(purpose, body)) {
		return errBadSeal
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return errBadSeal
	}
	pt, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(purpose))
	if err != nil {
		return errBadSeal
	}
	if json.Unmarshal(pt, v) != nil {
		return errBadSeal
	}
	return nil
}

func (s *sealer) tag(purpose, body string) []byte {
	m := hmac.New(sha256.New, s.mac)
	m.Write([]byte(purpose + "." + body))
	return m.Sum(nil)
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
