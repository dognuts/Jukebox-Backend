package antispam

import (
	_ "embed"
	"strings"
	"sync"
)

// disposableBlocklistRaw is the maintained blocklist from
// https://github.com/disposable-email-domains/disposable-email-domains
// (disposable_email_blocklist.conf), embedded at build time.
// Refresh it by re-downloading the file — no code changes needed.
//
//go:embed disposable_blocklist.txt
var disposableBlocklistRaw string

// extraDisposableDomains supplements the embedded list with domains seen
// abused here before the upstream list carried them.
var extraDisposableDomains = []string{
	"tempmail.com",
	"burnermail.io",
	"throwaway.com",
	"minutemail.com",
	"tempmailaddress.com",
	"mailtemp.net",
	"mail-temporaire.fr",
	"disposableemailaddresses.emailmiser.com",
	"mailzilla.com",
	"uglymail.com",
	"mailnator.com",
	"tempmailer.com",
}

// neverDisposable are major legitimate providers that must never be
// blocked, even if a bad upstream update were to include them.
var neverDisposable = map[string]bool{
	"gmail.com":      true,
	"googlemail.com": true,
	"yahoo.com":      true,
	"outlook.com":    true,
	"hotmail.com":    true,
	"live.com":       true,
	"msn.com":        true,
	"icloud.com":     true,
	"me.com":         true,
	"aol.com":        true,
	"protonmail.com": true,
	"proton.me":      true,
	"fastmail.com":   true,
	"hey.com":        true,
	"zoho.com":       true,
	"gmx.com":        true,
	"gmx.de":         true,
	"web.de":         true,
	"mail.com":       true,
}

var (
	disposableOnce sync.Once
	disposableSet  map[string]struct{}
)

func loadDisposableDomains() {
	disposableSet = make(map[string]struct{}, 9000)
	for _, line := range strings.Split(disposableBlocklistRaw, "\n") {
		d := strings.ToLower(strings.TrimSpace(line))
		if d == "" || strings.HasPrefix(d, "#") {
			continue
		}
		disposableSet[d] = struct{}{}
	}
	for _, d := range extraDisposableDomains {
		disposableSet[strings.ToLower(d)] = struct{}{}
	}
}

// IsDisposableEmail returns true if the email uses a known disposable
// domain, including any subdomain of one (e.g. abc.mailinator.com).
func IsDisposableEmail(email string) bool {
	disposableOnce.Do(loadDisposableDomains)

	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return false
	}
	domain := strings.ToLower(strings.TrimSpace(parts[1]))
	if domain == "" || neverDisposable[domain] {
		return false
	}

	// Check the domain and each parent domain (a.b.c → a.b.c, b.c, c)
	// against the set, so subdomains of blocked domains match without
	// scanning the whole list.
	for d := domain; d != ""; {
		if _, blocked := disposableSet[d]; blocked {
			return true
		}
		idx := strings.Index(d, ".")
		if idx == -1 {
			break
		}
		d = d[idx+1:]
	}
	return false
}
