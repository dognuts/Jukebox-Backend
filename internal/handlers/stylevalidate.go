package handlers

import (
	"errors"
	"regexp"
	"strings"
)

// cssValueRe is the character grammar for user-supplied CSS color/gradient
// values: color functions (oklch, rgb, hsl), hex colors, and gradient
// syntax. Deliberately excludes quotes, backslashes, semicolons, and
// colons — nothing a color or gradient needs, everything an injection does.
var cssValueRe = regexp.MustCompile(`^[a-zA-Z0-9#%.,()\s/\-]*$`)

// validateCSSValue vets a user-supplied string destined for an inline
// style attribute (cover gradients, avatar colors). These render in OTHER
// users' browsers, so url(...) here is a stored tracking beacon that leaks
// every viewer's IP and user agent to an attacker-chosen host.
func validateCSSValue(v string, maxLen int) error {
	if v == "" {
		return nil
	}
	if len(v) > maxLen {
		return errors.New("style value is too long")
	}
	lower := strings.ToLower(v)
	for _, banned := range []string{"url", "image-set", "expression", "javascript", "@import"} {
		if strings.Contains(lower, banned) {
			return errors.New("style value contains disallowed content")
		}
	}
	if !cssValueRe.MatchString(v) {
		return errors.New("style value contains disallowed characters")
	}
	return nil
}

// sanitizeLogValue makes a user-controlled string safe to interpolate into
// a log line: CR/LF (and other control chars) are stripped so injected
// newlines can't forge additional log entries, and the value is truncated.
func sanitizeLogValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
