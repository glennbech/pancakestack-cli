package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// pkcePair is an RFC 7636 (PKCE) code verifier + its S256 challenge.
// Verifier: 43-128 chars from unreserved-URI alphabet, we use 64 random bytes
// base64url-encoded (no padding) = 86 chars.
// Challenge: base64url(SHA256(verifier)) with no padding.
type pkcePair struct {
	verifier  string
	challenge string
}

func newPKCEPair() (pkcePair, error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return pkcePair{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return pkcePair{verifier: verifier, challenge: challenge}, nil
}

// randomState returns a random opaque string used as OAuth `state`. Same
// entropy budget as the PKCE verifier — plenty.
func randomState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
