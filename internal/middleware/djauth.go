package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

// GenerateDJKey creates a random 16-byte hex string to use as a DJ key.
func GenerateDJKey() (plainKey string, hash string, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plainKey = hex.EncodeToString(b)

	hashed, err := bcrypt.GenerateFromPassword([]byte(plainKey), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}
	return plainKey, string(hashed), nil
}

// VerifyDJKey checks a plaintext key against a bcrypt hash.
func VerifyDJKey(plainKey, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainKey)) == nil
}

// ExtractDJKey pulls the DJ key from the request.
// Checks query param ?djKey=... first, then X-DJ-Key header.
func ExtractDJKey(r *http.Request) string {
	if key := r.URL.Query().Get("djKey"); key != "" {
		return key
	}
	return r.Header.Get("X-DJ-Key")
}
