package antispam

import "strings"

// disposableDomains is a set of known disposable/temporary email providers.
// Expand this list as needed, or replace with an external API lookup.
var disposableDomains = map[string]bool{
	"mailinator.com":       true,
	"guerrillamail.com":    true,
	"guerrillamail.de":     true,
	"guerrillamail.net":    true,
	"guerrillamail.org":    true,
	"grr.la":               true,
	"guerrillamailblock.com": true,
	"tempmail.com":         true,
	"temp-mail.org":        true,
	"temp-mail.io":         true,
	"throwaway.email":      true,
	"throwaway.com":        true,
	"yopmail.com":          true,
	"yopmail.fr":           true,
	"sharklasers.com":      true,
	"guerrillamail.info":   true,
	"spam4.me":             true,
	"trashmail.com":        true,
	"trashmail.me":         true,
	"trashmail.net":        true,
	"trashmail.org":        true,
	"trashmail.io":         true,
	"10minutemail.com":     true,
	"10minutemail.net":     true,
	"10minutemail.org":     true,
	"10minute.email":       true,
	"minutemail.com":       true,
	"tempail.com":          true,
	"tempr.email":          true,
	"dispostable.com":      true,
	"maildrop.cc":          true,
	"mailnesia.com":        true,
	"mailcatch.com":        true,
	"fakeinbox.com":        true,
	"fakemail.net":         true,
	"mailsac.com":          true,
	"mohmal.com":           true,
	"getnada.com":          true,
	"emailondeck.com":      true,
	"inboxkitten.com":      true,
	"harakirimail.com":     true,
	"crazymailing.com":     true,
	"burnermail.io":        true,
	"jetable.org":          true,
	"mytemp.email":         true,
	"tempinbox.com":        true,
	"tempmailaddress.com":  true,
	"mailtemp.net":         true,
	"mail-temporaire.fr":   true,
	"getairmail.com":       true,
	"filzmail.com":         true,
	"emailfake.com":        true,
	"generator.email":      true,
	"guerrillamail.biz":    true,
	"disposableemailaddresses.emailmiser.com": true,
	"mailexpire.com":       true,
	"mailforspam.com":      true,
	"mailnull.com":         true,
	"mailshell.com":        true,
	"mailzilla.com":        true,
	"nomail.xl.cx":         true,
	"nospam.ze.tc":         true,
	"pookmail.com":         true,
	"safetymail.info":      true,
	"spambox.us":           true,
	"spamfree24.org":       true,
	"spamgourmet.com":      true,
	"spamhole.com":         true,
	"spaml.com":            true,
	"uglymail.com":         true,
	"mailnator.com":        true,
	"binkmail.com":         true,
	"bobmail.info":         true,
	"chammy.info":          true,
	"devnullmail.com":      true,
	"dingbone.com":         true,
	"fudgerub.com":         true,
	"lookugly.com":         true,
	"mailinater.com":       true,
	"mailinator.net":       true,
	"mailinator.org":       true,
	"mailinator2.com":      true,
	"notmailinator.com":    true,
	"reallymymail.com":     true,
	"reconmail.com":        true,
	"safetypost.de":        true,
	"slipry.net":           true,
	"suremail.info":        true,
	"tempmailer.com":       true,
	"tempmailer.de":        true,
	"wegwerfmail.de":       true,
	"wegwerfmail.net":      true,
	"wegwerfmail.org":      true,
	"zoemail.org":          true,
	"protonmail.com":       false, // NOT disposable — legit provider
}

// IsDisposableEmail returns true if the email uses a known disposable domain.
func IsDisposableEmail(email string) bool {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return false
	}
	domain := strings.ToLower(strings.TrimSpace(parts[1]))

	// Check exact match
	if disposableDomains[domain] {
		return true
	}

	// Check if it's a subdomain of a disposable domain (e.g. abc.mailinator.com)
	for d, blocked := range disposableDomains {
		if blocked && strings.HasSuffix(domain, "."+d) {
			return true
		}
	}

	return false
}
