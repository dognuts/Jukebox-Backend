package models

import (
	"errors"
	"net/url"
	"strings"
)

// Track-submission field caps. Applied on BOTH the HTTP and WS submit
// paths — before this validator existed, the HTTP path accepted multi-MB
// titles that were then rebroadcast to every listener with the full queue.
const (
	MaxTrackTitleLen     = 200
	MaxTrackArtistLen    = 200
	MaxTrackSourceURLLen = 500
	MaxTrackDurationSecs = 3 * 60 * 60
)

var trackSourceHosts = map[TrackSource][]string{
	TrackSourceYouTube: {
		"youtube.com", "youtu.be", "youtube-nocookie.com", "music.youtube.com",
	},
	TrackSourceSoundCloud: {
		"soundcloud.com", "sndcdn.com",
	},
}

// hostAllowed reports whether host equals an allowed domain or is one of
// its subdomains.
func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(host)
	for _, a := range allowed {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// ValidateTrackSubmission enforces field caps, a source allowlist, and a
// per-source host allowlist on the source URL. The host check matters
// beyond hygiene: submitted tracks are fetched/embedded by EVERY listener's
// browser, so an arbitrary URL is a room-wide deanonymization beacon (the
// attacker's server logs each listener's IP and user agent). Direct mp3
// URLs are therefore restricted to the room's DJ, who could only beacon
// their own audience.
func ValidateTrackSubmission(title, artist, source, sourceURL string, duration int, isDJ bool) error {
	if strings.TrimSpace(title) == "" {
		return errors.New("track title is required")
	}
	if len(title) > MaxTrackTitleLen {
		return errors.New("track title is too long")
	}
	if len(artist) > MaxTrackArtistLen {
		return errors.New("track artist is too long")
	}
	if len(sourceURL) > MaxTrackSourceURLLen {
		return errors.New("track URL is too long")
	}
	if duration < 0 || duration > MaxTrackDurationSecs {
		return errors.New("track duration is out of range")
	}

	u, err := url.Parse(sourceURL)
	if err != nil || u.Hostname() == "" {
		return errors.New("track URL is not valid")
	}
	if u.Scheme != "https" {
		return errors.New("track URL must be https")
	}

	switch TrackSource(source) {
	case TrackSourceYouTube, TrackSourceSoundCloud:
		if !hostAllowed(u.Hostname(), trackSourceHosts[TrackSource(source)]) {
			return errors.New("track URL host does not match its source")
		}
	case TrackSourceMP3:
		if !isDJ {
			return errors.New("direct audio URLs can only be added by the DJ")
		}
	default:
		return errors.New("unknown track source")
	}
	return nil
}
