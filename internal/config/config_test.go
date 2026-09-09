package config

import (
	"testing"
	"time"
)

func TestVerifyHoldSeconds(t *testing.T) {
	tests := []struct {
		env  string
		want time.Duration
	}{
		{"", 60 * time.Second},    // default
		{"120", 120 * time.Second},
		{"0", 0},                  // disabled
		{"abc", 60 * time.Second}, // garbage falls back to default
		{"-5", 60 * time.Second},  // negative falls back to default
	}
	for _, tt := range tests {
		t.Run("env="+tt.env, func(t *testing.T) {
			t.Setenv("VERIFY_HOLD_SECONDS", tt.env)
			if got := verifyHoldFromEnv(); got != tt.want {
				t.Errorf("verifyHoldFromEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}
