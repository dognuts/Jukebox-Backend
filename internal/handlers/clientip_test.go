package handlers

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		desc        string
		xff         string
		xRealIP     string
		remoteAddr  string
		want        string
	}{
		{"XFF single IP wins", "203.0.113.9", "", "10.0.0.1:4242", "203.0.113.9"},
		{"XFF comma-separated returns first entry", "203.0.113.9, 70.41.3.18", "", "10.0.0.1:4242", "203.0.113.9"},
		{"XFF comma-separated trims whitespace", "203.0.113.9 , 70.41.3.18", "", "10.0.0.1:4242", "203.0.113.9"},
		{"X-Real-IP used when no XFF", "", "198.51.100.7", "10.0.0.1:4242", "198.51.100.7"},
		{"RemoteAddr with port falls back", "", "", "10.0.0.1:4242", "10.0.0.1"},
		{"RemoteAddr without port falls back", "", "", "10.0.0.1", "10.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xRealIP != "" {
				r.Header.Set("X-Real-IP", tt.xRealIP)
			}
			r.RemoteAddr = tt.remoteAddr

			got := ClientIP(r)
			if got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
