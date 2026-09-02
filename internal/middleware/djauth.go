package middleware

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

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

// djKeyCacheTTL bounds how long a successful bcrypt verification is reused
// before the full compare runs again.
const djKeyCacheTTL = 15 * time.Minute

// maxDJKeyCacheEntries bounds the cache. Entries are only added on successful
// verification, so it grows with the number of active DJs — not with
// attacker-controlled input.
const maxDJKeyCacheEntries = 4096

// djKeyCache remembers successful (hash, key) verifications so hot paths
// (track submissions, DJ dashboard polls) don't re-run a ~60-100ms bcrypt
// compare on every request. Lookups are keyed by a SHA-256 digest of
// hash+key: the map lookup itself is not constant-time, but because the
// input is pre-hashed, any timing signal reveals nothing about the plaintext
// key. Including the stored bcrypt hash in the digest scopes each entry to
// the room it was verified for (equivalent to keying by (roomID, sha256(key))
// since each room has a unique hash), so no invalidation is needed if a
// room's key ever changes.
var djKeyCache = struct {
	sync.RWMutex
	entries map[[sha256.Size]byte]time.Time // digest -> expiry
}{entries: make(map[[sha256.Size]byte]time.Time)}

func djKeyCacheKey(plainKey, hash string) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(hash))
	h.Write([]byte{0}) // domain separator between hash and key
	h.Write([]byte(plainKey))
	var k [sha256.Size]byte
	copy(k[:], h.Sum(nil))
	return k
}

// VerifyDJKey checks a plaintext key against a bcrypt hash.
// Empty keys short-circuit without running bcrypt: anonymous listeners hit
// this on every track submission and must not pay the full-cost compare.
// Successful verifications are cached in memory for djKeyCacheTTL.
func VerifyDJKey(plainKey, hash string) bool {
	if plainKey == "" || hash == "" {
		return false
	}

	k := djKeyCacheKey(plainKey, hash)
	now := time.Now()

	djKeyCache.RLock()
	exp, ok := djKeyCache.entries[k]
	djKeyCache.RUnlock()
	if ok && now.Before(exp) {
		return true
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainKey)) != nil {
		return false
	}

	djKeyCache.Lock()
	if len(djKeyCache.entries) >= maxDJKeyCacheEntries {
		for k2, exp2 := range djKeyCache.entries {
			if now.After(exp2) {
				delete(djKeyCache.entries, k2)
			}
		}
		if len(djKeyCache.entries) >= maxDJKeyCacheEntries {
			// Still full of live entries — drop everything. The cache is a
			// best-effort bcrypt saver, never a source of truth.
			djKeyCache.entries = make(map[[sha256.Size]byte]time.Time)
		}
	}
	djKeyCache.entries[k] = now.Add(djKeyCacheTTL)
	djKeyCache.Unlock()
	return true
}

// ExtractDJKey pulls the DJ key from the X-DJ-Key header. The old
// ?djKey=... query-param branch is gone: the DJ key is a permanent,
// non-rotating room credential, and query strings are written verbatim
// into request logs (chi logger, reverse proxies). WebSocket handshakes —
// the one place headers aren't available — authenticate via single-use
// tickets instead (see handlers.WSTicketHandler).
func ExtractDJKey(r *http.Request) string {
	return r.Header.Get("X-DJ-Key")
}
