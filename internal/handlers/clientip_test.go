package handlers

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		desc       string
		xff        string
		remoteAddr string
		want       string
	}{
		{
			desc:       "no headers falls back to RemoteAddr without port",
			remoteAddr: "203.0.113.7:52100",
			want:       "203.0.113.7",
		},
		{
			desc:       "single XFF entry (set by proxy)",
			xff:        "198.51.100.4",
			remoteAddr: "10.0.0.1:443",
			want:       "198.51.100.4",
		},
		{
			desc:       "spoofed client entries are ignored — rightmost wins",
			xff:        "1.2.3.4, 5.6.7.8, 198.51.100.4",
			remoteAddr: "10.0.0.1:443",
			want:       "198.51.100.4",
		},
		{
			desc:       "trailing empty entries are skipped",
			xff:        "198.51.100.4, ",
			remoteAddr: "10.0.0.1:443",
			want:       "198.51.100.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := ClientIP(r); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIP_XRealIPIgnored(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.7:52100"
	r.Header.Set("X-Real-IP", "1.2.3.4")
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Errorf("ClientIP() = %q, want RemoteAddr-derived 203.0.113.7 (X-Real-IP must be ignored)", got)
	}
}
