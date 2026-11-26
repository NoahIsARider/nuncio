package events

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Signature errors. The receiver maps all of them to 403, but they are distinct
// so logs can name what was wrong without echoing a credential.
var (
	ErrNoSecret         = errors.New("webhook secret is not configured")
	ErrMissingSignature = errors.New("delivery is unsigned")
	ErrBadSignature     = errors.New("signature does not match the body")
	ErrStaleDelivery    = errors.New("delivery has already been seen")
)

// VerifyGitHub checks X-Hub-Signature-256: sha256=<hex hmac of body>.
func VerifyGitHub(secret string, body []byte, header string) error {
	if secret == "" {
		return ErrNoSecret
	}
	if header == "" {
		return ErrMissingSignature
	}
	const prefix = "sha256="
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return fmt.Errorf("%w: expected sha256= prefix", ErrBadSignature)
	}
	want, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return ErrBadSignature
	}
	return nil
}

// VerifyForgejo checks Forgejo/Gitea's hex HMAC-SHA256. Some versions include
// the sha256= prefix, some do not, so both are accepted.
func VerifyForgejo(secret string, body []byte, header string) error {
	if secret == "" {
		return ErrNoSecret
	}
	if header == "" {
		return ErrMissingSignature
	}
	header = strings.TrimPrefix(strings.TrimPrefix(header, "sha256="), "sha256=")
	want, err := hex.DecodeString(header)
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return ErrBadSignature
	}
	return nil
}

// VerifyGitLab checks X-Gitlab-Token, which is the shared secret itself.
func VerifyGitLab(token string, header string) error {
	if token == "" {
		return ErrNoSecret
	}
	if header == "" {
		return ErrMissingSignature
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(header)) != 1 {
		return ErrBadSignature
	}
	return nil
}

// VerifySourceHut checks the ed25519 signature over body+nonce. git.sr.ht signs
// the request body concatenated with X-Payload-Nonce.
func VerifySourceHut(pub ed25519.PublicKey, body []byte, nonce, signature string) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrNoSecret
	}
	if nonce == "" {
		return fmt.Errorf("%w: X-Payload-Nonce is missing", ErrMissingSignature)
	}
	if signature == "" {
		return fmt.Errorf("%w: X-Payload-Signature is missing", ErrMissingSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		if sig, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(signature)); err != nil {
			return fmt.Errorf("%w: signature is not base64", ErrBadSignature)
		}
	}
	message := append(append([]byte{}, body...), []byte(nonce)...)
	if !ed25519.Verify(pub, message, sig) {
		return ErrBadSignature
	}
	return nil
}

// ParseEd25519PublicKey accepts a base64 (standard or raw) or hex public key.
func ParseEd25519PublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrNoSecret
	}
	candidates := [][]byte{}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		candidates = append(candidates, b)
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		candidates = append(candidates, b)
	}
	if b, err := hex.DecodeString(s); err == nil {
		candidates = append(candidates, b)
	}
	for _, c := range candidates {
		if len(c) == ed25519.PublicKeySize {
			return ed25519.PublicKey(c), nil
		}
	}
	return nil, fmt.Errorf("public key is not a 32-byte ed25519 key (base64 or hex)")
}

// SignSourceHut is the inverse of VerifySourceHut. It exists so tests and
// operators can produce a valid delivery without a real git.sr.ht.
func SignSourceHut(priv ed25519.PrivateKey, body []byte, nonce string) string {
	message := append(append([]byte{}, body...), []byte(nonce)...)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message))
}
