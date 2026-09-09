package handlers

import (
	"strings"
	"testing"
	"time"
)

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		password string
		valid    bool
		desc     string
	}{
		// Valid passwords
		{"Password1", true, "meets all requirements"},
		{"MyP4ssword", true, "uppercase, lowercase, digit"},
		{"abcDEF123", true, "mixed case with digits"},
		{"Aa1" + "xxxxx", true, "exactly 8 chars"},
		{"Aa1" + strings.Repeat("x", 125), true, "128 chars (upper boundary)"},

		// Too short
		{"Pass1", false, "too short (5 chars)"},
		{"Aa1xxxx", false, "too short (7 chars)"},
		{"", false, "empty"},

		// Too long
		{"Aa1" + string(make([]byte, 126)), false, "129 chars"},

		// Missing character classes
		{"password1", false, "no uppercase"},
		{"PASSWORD1", false, "no lowercase"},
		{"Password", false, "no digit"},
		{"12345678", false, "digits only"},
		{"abcdefgh", false, "lowercase only"},
		{"ABCDEFGH", false, "uppercase only"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			err := validatePassword(tt.password)
			if tt.valid && err != nil {
				t.Errorf("validatePassword(%q) returned error %v, expected nil", tt.password, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("validatePassword(%q) returned nil, expected error", tt.password)
			}
		})
	}
}

func TestGenerateSecureToken(t *testing.T) {
	token1 := generateSecureToken()
	token2 := generateSecureToken()

	if len(token1) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("token length = %d, want 64", len(token1))
	}

	if token1 == token2 {
		t.Error("two generated tokens should not be equal")
	}
}

func TestShouldHoldVerification(t *testing.T) {
	created := time.Date(2026, 9, 7, 15, 25, 23, 0, time.UTC)
	tests := []struct {
		desc      string
		elapsed   time.Duration
		threshold time.Duration
		want      bool
	}{
		{"bot-speed click is held", 12 * time.Second, 60 * time.Second, true},
		{"just under threshold is held", 59 * time.Second, 60 * time.Second, true},
		{"exactly threshold is not held", 60 * time.Second, 60 * time.Second, false},
		{"human-speed click is not held", 30 * time.Minute, 60 * time.Second, false},
		{"threshold 0 disables hold", 1 * time.Second, 0, false},
		{"clock skew (negative elapsed) is held", -2 * time.Second, 60 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := shouldHoldVerification(created, created.Add(tt.elapsed), tt.threshold)
			if got != tt.want {
				t.Errorf("shouldHoldVerification(elapsed=%v, threshold=%v) = %v, want %v", tt.elapsed, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestTruncateUserAgent(t *testing.T) {
	short := "Mozilla/5.0"
	if got := truncateUserAgent(short); got != short {
		t.Errorf("short UA changed: %q", got)
	}
	long := strings.Repeat("x", 600)
	if got := truncateUserAgent(long); len(got) != maxUserAgentLen {
		t.Errorf("long UA len = %d, want %d", len(got), maxUserAgentLen)
	}
	if got := truncateUserAgent(""); got != "" {
		t.Errorf("empty UA = %q, want empty", got)
	}
}
