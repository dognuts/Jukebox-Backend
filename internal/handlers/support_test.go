package handlers

import (
	"testing"
	"time"
)

func TestValidateListenerReport(t *testing.T) {
	validAnon := listenerReportRequest{
		Category:     "gated",
		Message:      "My video is showing the signin thing and I cant hear anything please help",
		ContactEmail: "listener@example.com",
		RoomSlug:     "friday-night-funk",
		TrackID:      "track-1",
	}

	tests := []struct {
		desc       string
		req        listenerReportRequest
		hasSession bool
		wantOK     bool
	}{
		{"valid anon", validAnon, false, true},
		{"valid logged-in without email", func() listenerReportRequest { r := validAnon; r.ContactEmail = ""; return r }(), true, true},
		{"bad category", func() listenerReportRequest { r := validAnon; r.Category = "random"; return r }(), false, false},
		{"message too short", func() listenerReportRequest { r := validAnon; r.Message = "short"; return r }(), false, false},
		{"message too long", func() listenerReportRequest {
			r := validAnon
			b := make([]byte, 2001)
			for i := range b {
				b[i] = 'a'
			}
			r.Message = string(b)
			return r
		}(), false, false},
		{"anon requires email", func() listenerReportRequest { r := validAnon; r.ContactEmail = ""; return r }(), false, false},
		{"anon bad email", func() listenerReportRequest { r := validAnon; r.ContactEmail = "not-an-email"; return r }(), false, false},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			err := validateListenerReport(tt.req, tt.hasSession)
			if tt.wantOK && err != "" {
				t.Errorf("expected valid, got error: %s", err)
			}
			if !tt.wantOK && err == "" {
				t.Errorf("expected error, got valid")
			}
		})
	}
}

func TestIsLikelyBot(t *testing.T) {
	now := time.Now()
	nowMs := now.UnixMilli()

	tests := []struct {
		desc string
		req  listenerReportRequest
		want bool
	}{
		{"honeypot filled", listenerReportRequest{Website: "http://spam.example", OpenedAt: nowMs - 5000}, true},
		{"honeypot empty and time ok", listenerReportRequest{Website: "", OpenedAt: nowMs - 5000}, false},
		{"submitted instantly", listenerReportRequest{Website: "", OpenedAt: nowMs - 200}, true},
		{"openedAt in the future (clock skew)", listenerReportRequest{Website: "", OpenedAt: nowMs + 10000}, true},
		{"openedAt zero (missing)", listenerReportRequest{Website: "", OpenedAt: 0}, true},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := isLikelyBot(tt.req, now)
			if got != tt.want {
				t.Errorf("isLikelyBot = %v, want %v", got, tt.want)
			}
		})
	}
}
