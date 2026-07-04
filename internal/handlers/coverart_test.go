package handlers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jukebox/backend/internal/models"
)

// dataURL builds a data: URL whose total length is exactly n bytes.
func dataURL(n int) string {
	const prefix = "data:image/jpeg;base64,"
	if n < len(prefix) {
		return prefix[:n]
	}
	return prefix + strings.Repeat("A", n-len(prefix))
}

// TestValidateCoverArt covers the write gate: only data: URLs over the cap are
// rejected; external URLs (any length) and empty strings always pass.
func TestValidateCoverArt(t *testing.T) {
	tests := []struct {
		desc    string
		cover   string
		wantErr bool
	}{
		{"empty passes", "", false},
		{"small data url passes", dataURL(1024), false},
		{"data url exactly at cap passes", dataURL(maxDataURLCoverBytes), false},
		{"data url one byte over cap rejected", dataURL(maxDataURLCoverBytes + 1), true},
		{"huge data url rejected", dataURL(4 * 1024 * 1024), true},
		{"long external url passes", "https://cdn.example.com/" + strings.Repeat("a", maxDataURLCoverBytes*2), false},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			err := validateCoverArt(tt.cover)
			if tt.wantErr {
				if !errors.Is(err, errCoverArtTooLarge) {
					t.Fatalf("validateCoverArt(len=%d) err = %v, want errCoverArtTooLarge", len(tt.cover), err)
				}
			} else if err != nil {
				t.Fatalf("validateCoverArt(len=%d) = %v, want nil", len(tt.cover), err)
			}
		})
	}
}

// TestStripOversizedCover covers the read strip helper at an arbitrary
// threshold: oversized data: URLs blank to "", everything else is untouched.
func TestStripOversizedCover(t *testing.T) {
	ext := "https://cdn.example.com/" + strings.Repeat("a", 4096)
	tests := []struct {
		desc  string
		cover string
		max   int
		want  string
	}{
		{"empty stays empty", "", maxDataURLCoverBytes, ""},
		{"data url under cap kept", dataURL(1024), maxDataURLCoverBytes, dataURL(1024)},
		{"data url at cap kept", dataURL(maxDataURLCoverBytes), maxDataURLCoverBytes, dataURL(maxDataURLCoverBytes)},
		{"data url over cap blanked", dataURL(maxDataURLCoverBytes + 1), maxDataURLCoverBytes, ""},
		{"external url over cap kept", ext, 128, ext},
		{"detail cap keeps mid-size data url that list cap would blank",
			dataURL(256 * 1024), maxDataURLDetailBytes, dataURL(256 * 1024)},
		{"list cap blanks the same mid-size data url",
			dataURL(256 * 1024), maxDataURLCoverBytes, ""},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := stripOversizedCover(tt.cover, tt.max); got != tt.want {
				t.Fatalf("stripOversizedCover(len=%d, max=%d) len=%d, want len=%d",
					len(tt.cover), tt.max, len(got), len(tt.want))
			}
		})
	}
}

// TestStripListCovers_JSONBoundary asserts the list-payload strip that
// buildRoomsList applies before marshaling: an oversized legacy data: cover is
// gone from the serialized JSON, while a small data cover and an external URL
// survive. This is the exact path that keeps the homepage ISR payload small.
func TestStripListCovers_JSONBoundary(t *testing.T) {
	big := dataURL(maxDataURLCoverBytes + 1)
	small := "data:image/png;base64,SMALLCOVER"
	ext := "https://cdn.example.com/cover.jpg"

	rooms := []RoomWithNowPlaying{
		{Room: models.Room{ID: "r1", Slug: "r1", CoverArtURL: big}},
		{Room: models.Room{ID: "r2", Slug: "r2", CoverArtURL: small}},
		{Room: models.Room{ID: "r3", Slug: "r3", CoverArtURL: ext}},
	}

	stripListCovers(rooms)

	if rooms[0].CoverArtURL != "" {
		t.Errorf("oversized cover not stripped in place: len=%d", len(rooms[0].CoverArtURL))
	}
	if rooms[1].CoverArtURL != small {
		t.Errorf("small data cover was altered: %q", rooms[1].CoverArtURL)
	}
	if rooms[2].CoverArtURL != ext {
		t.Errorf("external cover was altered: %q", rooms[2].CoverArtURL)
	}

	data, err := json.Marshal(rooms)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(data)
	if strings.Contains(body, big) {
		t.Error("oversized base64 cover leaked into list JSON")
	}
	if !strings.Contains(body, small) {
		t.Error("small data cover missing from list JSON")
	}
	if !strings.Contains(body, ext) {
		t.Error("external cover missing from list JSON")
	}
}
