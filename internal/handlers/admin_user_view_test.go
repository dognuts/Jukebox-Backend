package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jukebox/backend/internal/models"
)

func TestAdminUserViewExposesForensics(t *testing.T) {
	created := time.Date(2026, 9, 7, 15, 25, 23, 0, time.UTC)
	verified := created.Add(12*time.Second + 600*time.Millisecond)
	held := created.Add(13 * time.Second)
	u := models.User{
		ID: "u1", Email: "a@b.c", CreatedAt: created,
		SignupIP: "203.0.113.9", SignupUserAgent: "curl/8.0",
		VerifiedAt: &verified, VerifyHeldAt: &held,
	}
	view := newAdminUserView(u)
	if view.SignupIP != "203.0.113.9" || view.SignupUserAgent != "curl/8.0" {
		t.Errorf("forensics not copied: %+v", view)
	}
	if view.SecondsToVerify == nil || *view.SecondsToVerify != 12 {
		t.Errorf("SecondsToVerify = %v, want 12 (rounded down)", view.SecondsToVerify)
	}
	b, _ := json.Marshal(view)
	for _, key := range []string{`"signupIp"`, `"signupUserAgent"`, `"verifiedAt"`, `"verifyHeldAt"`, `"secondsToVerify":12`, `"email"`, `"createdAt"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("admin JSON missing %s: %s", key, b)
		}
	}
}

func TestAdminUserViewNullsWhenNeverVerified(t *testing.T) {
	view := newAdminUserView(models.User{ID: "u2", CreatedAt: time.Now()})
	if view.SecondsToVerify != nil {
		t.Errorf("SecondsToVerify = %v, want nil", *view.SecondsToVerify)
	}
	b, _ := json.Marshal(view)
	if !strings.Contains(string(b), `"secondsToVerify":null`) || !strings.Contains(string(b), `"verifiedAt":null`) {
		t.Errorf("want explicit nulls, got %s", b)
	}
}

// The plain User model backs /api/auth/me and profiles; forensics must not leak.
func TestUserModelHidesForensics(t *testing.T) {
	now := time.Now()
	b, _ := json.Marshal(models.User{ID: "u", SignupIP: "203.0.113.9", SignupUserAgent: "curl", VerifiedAt: &now, VerifyHeldAt: &now})
	for _, leak := range []string{"203.0.113.9", "curl", "verifiedAt", "verifyHeldAt", "signupIp"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("models.User JSON leaks %q: %s", leak, b)
		}
	}
}
