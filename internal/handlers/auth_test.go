package handlers

import "testing"

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
		{"Aa1xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", true, "128 chars"},

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
