package handlers

import (
	"net/http"
	"strings"
)

// ClientIP returns the client's IP address as seen by our trusted edge
// proxy (Render), falling back to r.RemoteAddr with any port stripped.
//
// X-Forwarded-For is client-influenced: a client can send its own header
// and proxies APPEND the address they saw to the right. Trusting the
// FIRST entry (as this function once did) let anyone spoof a fresh IP
// per request and bypass IP-keyed rate limits. The only entry our edge
// vouches for is the LAST one — the peer address the proxy itself
// observed — so that is what we use.
//
// X-Real-IP is deliberately ignored: nothing in this deployment sets it,
// so honoring it would be another spoofable input.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		entries := strings.Split(xff, ",")
		for i := len(entries) - 1; i >= 0; i-- {
			if ip := strings.TrimSpace(entries[i]); ip != "" {
				return ip
			}
		}
	}
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i != -1 {
		return addr[:i]
	}
	return addr
}
