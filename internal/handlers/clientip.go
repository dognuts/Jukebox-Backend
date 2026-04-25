package handlers

import (
	"net/http"
	"strings"
)

// ClientIP returns the client's IP address, preferring X-Forwarded-For (first
// entry if comma-separated), then X-Real-IP, then falling back to
// r.RemoteAddr with any port stripped. This mirrors the inline logic that
// auth.go used to duplicate across Signup, Login, and password-reset flows.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		return realIP
	}
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i != -1 {
		return addr[:i]
	}
	return addr
}
