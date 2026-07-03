package middleware

import (
	"testing"
	"time"
)

func TestVerifyDJKey(t *testing.T) {
	key, hash, err := GenerateDJKey()
	if err != nil {
		t.Fatalf("GenerateDJKey: %v", err)
	}

	if !VerifyDJKey(key, hash) {
		t.Error("valid key rejected")
	}
	// Second verification hits the in-memory cache — must still succeed.
	if !VerifyDJKey(key, hash) {
		t.Error("valid key rejected on cached verification")
	}
	if VerifyDJKey("not-the-key", hash) {
		t.Error("wrong key accepted")
	}
	if VerifyDJKey("", hash) {
		t.Error("empty key accepted")
	}
	if VerifyDJKey(key, "") {
		t.Error("key accepted against empty hash")
	}
}

// TestVerifyDJKeyCacheIsPerHash guards the cache key derivation: a key that
// was successfully verified against room A's hash must not be accepted
// against room B's hash via a cache hit.
func TestVerifyDJKeyCacheIsPerHash(t *testing.T) {
	keyA, hashA, err := GenerateDJKey()
	if err != nil {
		t.Fatalf("GenerateDJKey: %v", err)
	}
	_, hashB, err := GenerateDJKey()
	if err != nil {
		t.Fatalf("GenerateDJKey: %v", err)
	}

	if !VerifyDJKey(keyA, hashA) {
		t.Fatal("valid key rejected")
	}
	if VerifyDJKey(keyA, hashB) {
		t.Error("room A's key accepted against room B's hash")
	}
}

// TestVerifyDJKeyEmptyKeyIsFast pins the empty-key short-circuit: anonymous
// listeners hit VerifyDJKey on every track submission, and each full bcrypt
// compare costs ~60-100ms. 100 empty-key checks running bcrypt would take
// several seconds; short-circuited they are effectively instant.
func TestVerifyDJKeyEmptyKeyIsFast(t *testing.T) {
	_, hash, err := GenerateDJKey()
	if err != nil {
		t.Fatalf("GenerateDJKey: %v", err)
	}

	start := time.Now()
	for i := 0; i < 100; i++ {
		if VerifyDJKey("", hash) {
			t.Fatal("empty key accepted")
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("100 empty-key verifications took %v — the bcrypt short-circuit is gone", elapsed)
	}
}
