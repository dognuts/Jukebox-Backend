package store

import "testing"

// computeNextPlayIndex owns the "which track plays next?" rule. Pulled out of
// GetNextAutoplayTrack so we can test it without spinning up a real Postgres.
//
// Semantics: currentIndex is the CURRENTLY PLAYING index, or -1 if nothing has
// played yet. First play returns 0, subsequent plays return the next index
// modulo playlist length.
func TestComputeNextPlayIndex(t *testing.T) {
	tests := []struct {
		currentIndex int
		length       int
		want         int
		desc         string
	}{
		{-1, 5, 0, "fresh playlist: sentinel -1 → play track 0"},
		{0, 5, 1, "after playing track 0 → play track 1"},
		{3, 5, 4, "mid-playlist advance"},
		{4, 5, 0, "last track → wrap to 0"},
		{0, 1, 0, "single-track playlist → loop on itself"},
		{-1, 1, 0, "single-track playlist, first play"},
		{-1, 0, 0, "empty playlist: return 0 (caller should short-circuit)"},
		{5, 3, 0, "out-of-bounds currentIndex → wrap via modulo"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := computeNextPlayIndex(tt.currentIndex, tt.length)
			if got != tt.want {
				t.Errorf("computeNextPlayIndex(%d, %d) = %d, want %d",
					tt.currentIndex, tt.length, got, tt.want)
			}
		})
	}
}
