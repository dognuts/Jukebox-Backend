package antispam

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

// DomainAcceptsEmail reports whether the email's domain can plausibly
// receive mail: it has MX records, or (per RFC 5321 fallback) an A/AAAA
// record. Lookups that fail for transient reasons (DNS timeout, resolver
// down) return true — a flaky resolver must not block legitimate signups.
// Only a definitive "this domain has no mail infrastructure" answer
// (NXDOMAIN / no records) returns false.
func DomainAcceptsEmail(ctx context.Context, email string) bool {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 || parts[1] == "" {
		return false
	}
	domain := strings.ToLower(strings.TrimSpace(parts[1]))

	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	var resolver net.Resolver
	mxs, err := resolver.LookupMX(ctx, domain)
	if err == nil && len(mxs) > 0 {
		// "Null MX" (RFC 7505): a single record with target "." is an
		// explicit declaration that the domain accepts no mail.
		if len(mxs) == 1 && mxs[0].Host == "." {
			return false
		}
		return true
	}
	if err != nil && !isDefinitiveDNSError(err) {
		return true // transient failure — fail open
	}

	// No MX: fall back to A/AAAA, which SMTP also delivers to.
	addrs, err := resolver.LookupHost(ctx, domain)
	if err != nil {
		return !isDefinitiveDNSError(err) // fail open unless NXDOMAIN
	}
	return len(addrs) > 0
}

// isDefinitiveDNSError reports whether the lookup failed with an
// authoritative "does not exist / no such records" answer rather than a
// transient resolver problem.
func isDefinitiveDNSError(err error) bool {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return false
	}
	if dnsErr.IsTimeout || dnsErr.IsTemporary {
		return false
	}
	return dnsErr.IsNotFound
}
