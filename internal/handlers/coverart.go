package handlers

import (
	"errors"
	"strings"
)

// Cover-art size caps. Rooms may carry cover art as either an external URL
// (https://…) or an inline base64 data: URL. Only inline data URLs bloat the
// JSON payloads that the frontend embeds into server-rendered (ISR) pages, and
// a few multi-MB covers were enough to push a prerendered page past Vercel's
// 20MB limit (FALLBACK_BODY_TOO_LARGE). We cap data URLs at two points:
//
//   - maxDataURLCoverBytes gates writes (create/update reject a larger data
//     cover) and trims LIST payloads, which may embed many covers at once.
//   - maxDataURLDetailBytes trims DETAIL payloads, which embed exactly one
//     cover and can therefore tolerate a larger single image.
//
// External URLs and empty strings are always left untouched.
const (
	maxDataURLCoverBytes  = 128 * 1024
	maxDataURLDetailBytes = 512 * 1024
)

// errCoverArtTooLarge is returned by validateCoverArt for an oversized data
// URL; its message is safe to surface directly to the user in a 400 response.
var errCoverArtTooLarge = errors.New("cover image too large — please choose a smaller image")

// isDataURL reports whether s is an inline data: URL (as opposed to an
// external http(s) URL or an empty string).
func isDataURL(s string) bool {
	return strings.HasPrefix(s, "data:")
}

// validateCoverArt rejects data: cover URLs whose encoded length exceeds
// maxDataURLCoverBytes. External URLs and empty strings always pass — only
// inline base64 data URLs can bloat the payload. Callers surface the returned
// error as a 400.
func validateCoverArt(coverArt string) error {
	if isDataURL(coverArt) && len(coverArt) > maxDataURLCoverBytes {
		return errCoverArtTooLarge
	}
	return nil
}

// stripOversizedCover blanks a data: cover URL whose length exceeds maxBytes so
// oversized legacy covers already in the DB never reach a JSON payload. It runs
// at the wire boundary on a copy of the value — it must never mutate a cached
// or shared DB struct. External URLs and empty strings are returned unchanged.
func stripOversizedCover(coverArt string, maxBytes int) string {
	if isDataURL(coverArt) && len(coverArt) > maxBytes {
		return ""
	}
	return coverArt
}

// stripListCovers blanks oversized data: covers, at the list-context cap, on
// every entry of a rooms-list payload in place. buildRoomsList calls this on
// the freshly assembled wire slice before it is marshaled and cached, so the
// cached bytes never carry an oversized cover.
func stripListCovers(rooms []RoomWithNowPlaying) {
	for i := range rooms {
		rooms[i].CoverArtURL = stripOversizedCover(rooms[i].CoverArtURL, maxDataURLCoverBytes)
	}
}
