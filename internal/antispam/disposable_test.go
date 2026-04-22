package antispam

import "testing"

func TestIsDisposableEmail(t *testing.T) {
	tests := []struct {
		email    string
		expected bool
		desc     string
	}{
		// Known disposable domains
		{"user@mailinator.com", true, "mailinator"},
		{"user@guerrillamail.com", true, "guerrillamail"},
		{"user@tempmail.com", true, "tempmail"},
		{"user@yopmail.com", true, "yopmail"},
		{"user@10minutemail.com", true, "10minutemail"},
		{"user@trashmail.com", true, "trashmail"},
		{"user@maildrop.cc", true, "maildrop"},
		{"user@sharklasers.com", true, "sharklasers"},
		{"user@burnermail.io", true, "burnermail"},

		// Subdomains of disposable domains
		{"user@sub.mailinator.com", true, "subdomain of mailinator"},
		{"user@abc.guerrillamail.com", true, "subdomain of guerrillamail"},

		// Legitimate email providers — must NOT be blocked
		{"user@gmail.com", false, "gmail"},
		{"user@yahoo.com", false, "yahoo"},
		{"user@outlook.com", false, "outlook"},
		{"user@hotmail.com", false, "hotmail"},
		{"user@icloud.com", false, "icloud"},
		{"user@protonmail.com", false, "protonmail (explicitly allowed)"},
		{"user@aol.com", false, "aol"},
		{"user@fastmail.com", false, "fastmail"},
		{"user@hey.com", false, "hey.com"},

		// Edge cases
		{"user@", false, "missing domain"},
		{"@mailinator.com", true, "empty local part but domain is disposable"},
		{"noatsign", false, "no @ sign"},
		{"", false, "empty string"},
		{"user@MAILINATOR.COM", true, "uppercase matched (domains are lowercased in check)"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			result := IsDisposableEmail(tt.email)
			if result != tt.expected {
				t.Errorf("IsDisposableEmail(%q) = %v, want %v", tt.email, result, tt.expected)
			}
		})
	}
}

func TestIsDisposableEmail_CaseInsensitive(t *testing.T) {
	// The function lowercases the domain, so mixed case should still match
	result := IsDisposableEmail("user@Mailinator.Com")
	if !result {
		t.Error("expected mixed-case mailinator to be detected as disposable")
	}
}
