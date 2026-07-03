package handlers

import (
	"testing"

	"github.com/jukebox/backend/internal/models"
)

// TestAutoplayTrackIndexBySourceURL covers the now-playing → playlist-row
// mapping used by UpdateLiveSnippets. Autoplay track IDs no longer encode a
// playlist index (they're stable hashes), so the match runs on source URL —
// the same key UpdateLiveTracks reconciles with.
func TestAutoplayTrackIndexBySourceURL(t *testing.T) {
	tracks := []models.AutoplayTrack{
		{SourceURL: "https://a"},
		{SourceURL: "https://b"},
		{SourceURL: "https://b"}, // duplicate URL — first occurrence wins
	}

	tests := []struct {
		desc      string
		tracks    []models.AutoplayTrack
		sourceURL string
		want      int
	}{
		{"first track", tracks, "https://a", 0},
		{"duplicate URL resolves to first occurrence", tracks, "https://b", 1},
		{"unknown URL", tracks, "https://zzz", -1},
		{"empty URL never matches", tracks, "", -1},
		{"empty playlist", nil, "https://a", -1},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := autoplayTrackIndexBySourceURL(tt.tracks, tt.sourceURL); got != tt.want {
				t.Errorf("autoplayTrackIndexBySourceURL(%q) = %d, want %d", tt.sourceURL, got, tt.want)
			}
		})
	}
}
