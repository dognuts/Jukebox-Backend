package email

import (
	"strings"
	"testing"
	"time"
)

func TestComposeListenerReportHTML_IncludesAllContext(t *testing.T) {
	ctx := ListenerReportContext{
		Category:            "gated",
		Message:             "Video says sign in to confirm you're not a bot.",
		ContactEmail:        "listener@example.com",
		CanContactBack:      true,
		UserID:              "user-42",
		RoomSlug:            "friday-night-funk",
		RoomName:            "Friday Night Funk",
		TrackID:             "track-1",
		TrackTitle:          "Apache",
		TrackArtist:         "Incredible Bongo Band",
		PlaybackPositionSec: 42.5,
		UserAgent:           "Mozilla/5.0",
		ClientIP:            "203.0.113.9",
		SubmittedAt:         time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC),
	}

	html := composeListenerReportHTML(ctx)

	for _, want := range []string{
		"gated",
		"Friday Night Funk",
		"friday-night-funk",
		"Apache",
		"Incredible Bongo Band",
		"42.5",
		"user-42",
		"listener@example.com",
		"203.0.113.9",
		"Mozilla/5.0",
		"Contact back: yes",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("composeListenerReportHTML output missing %q\nGot:\n%s", want, html)
		}
	}
}

func TestComposeListenerReportHTML_EscapesUserMessage(t *testing.T) {
	ctx := ListenerReportContext{
		Category:    "other",
		Message:     `<script>alert("xss")</script>`,
		RoomSlug:    "r", RoomName: "R",
		TrackID:     "t", TrackTitle: "T", TrackArtist: "A",
		SubmittedAt: time.Now(),
	}

	html := composeListenerReportHTML(ctx)

	if strings.Contains(html, `<script>`) {
		t.Errorf("raw <script> tag leaked into HTML output:\n%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Errorf("expected HTML-escaped <script> in output:\n%s", html)
	}
}

func TestComposeListenerReportHTML_ContactBackNo(t *testing.T) {
	ctx := ListenerReportContext{
		Category:       "other",
		Message:        "hello",
		CanContactBack: false,
		RoomName:       "R",
		TrackTitle:     "T",
		SubmittedAt:    time.Now(),
	}

	html := composeListenerReportHTML(ctx)

	if !strings.Contains(html, "Contact back: no") {
		t.Errorf("expected 'Contact back: no' in output:\n%s", html)
	}
}
