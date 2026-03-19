package moderation

import (
	"regexp"
	"strings"
)

var profanityPatterns []*regexp.Regexp

func init() {
	patterns := []string{
		`(?i)f+[u]+[c]+[k]+`,
		`(?i)s+h+[i!1]+[t]+`,
		`(?i)b+[i!1]+[t]+[c]+h+`,
		`(?i)a+[s]+[s]+h+[o0]+[l]+[e3]+`,
		`(?i)d+[i!1]+[c]+k+`,
		`(?i)p+[u]+[s]+[s]+[y]+`,
		`(?i)c+[u]+n+[t]+`,
		`(?i)w+h+[o0]+r+[e3]+`,
		`(?i)f+[a@]+[gq]+[gq]*`,
		`(?i)r+[e3]+t+[a@]+r+d+`,
		`(?i)c+[o0]+[c]+k+`,
		`(?i)n+[i!1l|]+[gq]+[gq]+[e3a@]+[r]+`,
		`(?i)n+[i!1l|]+[gq]+[gq]+[a@4]+[sz]*`,
		`(?i)n+[i!1l|]+[gq]+[a@4]+[sz]*`,
		`(?i)n+[e3]+[gq]+[r]+[o0]+[sz]*`,
	}
	for _, p := range patterns {
		profanityPatterns = append(profanityPatterns, regexp.MustCompile(p))
	}
}

func normalize(text string) string {
	s := strings.ToLower(text)
	replacer := strings.NewReplacer(
		" ", "", "_", "", "-", "", ".", "", "*", "", "+", "",
		"0", "o", "1", "i", "3", "e", "4", "a",
		"@", "a", "$", "s", "!", "i", "|", "l",
	)
	return replacer.Replace(s)
}

// ContainsProfanity checks if text contains profanity or slurs.
func ContainsProfanity(text string) bool {
	cleaned := normalize(text)
	for _, p := range profanityPatterns {
		if p.MatchString(cleaned) {
			return true
		}
	}
	return false
}
