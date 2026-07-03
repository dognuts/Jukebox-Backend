package playback

import (
	"strings"
	"testing"
)

// TestAutoplayTrackID guards the dedupe property that stops the tracks table
// from growing on every autoplay advance: the ID must be a pure function of
// (room, source URL) — identical across advances, distinct across rooms and
// URLs — and keep the auto- prefix so synthetic rows stay identifiable.
func TestAutoplayTrackID(t *testing.T) {
	a := autoplayTrackID("room-1", "https://youtu.be/abc")

	if b := autoplayTrackID("room-1", "https://youtu.be/abc"); b != a {
		t.Errorf("same room+URL must derive the same ID: %q vs %q", a, b)
	}
	if b := autoplayTrackID("room-2", "https://youtu.be/abc"); b == a {
		t.Errorf("different rooms must not share an ID: both %q", a)
	}
	if b := autoplayTrackID("room-1", "https://youtu.be/xyz"); b == a {
		t.Errorf("different URLs must not share an ID: both %q", a)
	}
	if !strings.HasPrefix(a, "auto-") {
		t.Errorf("ID %q must keep the auto- prefix", a)
	}
	// Concatenation must be unambiguous: ("ab","c") and ("a","bc") differ.
	if autoplayTrackID("ab", "c") == autoplayTrackID("a", "bc") {
		t.Error("room/URL boundary must be part of the hash input")
	}
}
