package middleware

import (
	"net/http"
	"strings"
)

// SecurityHeaders adds standard security headers to every response.
func SecurityHeaders(allowedOrigins []string) func(http.Handler) http.Handler {
	// Build CSP frame-ancestors from allowed origins
	frameAncestors := "'self'"
	if len(allowedOrigins) > 0 {
		frameAncestors = "'self' " + strings.Join(allowedOrigins, " ")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()

			// Prevent clickjacking — only allow framing from our own origins
			h.Set("X-Frame-Options", "SAMEORIGIN")

			// Prevent MIME type sniffing
			h.Set("X-Content-Type-Options", "nosniff")

			// XSS protection (legacy browsers)
			h.Set("X-XSS-Protection", "1; mode=block")

			// Referrer policy — send origin only to same-origin, nothing to cross-origin
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")

			// Permissions policy — disable unnecessary browser APIs
			h.Set("Permissions-Policy", "camera=(), microphone=(self), geolocation=(), payment=()")

			// HSTS — force HTTPS for 1 year (only in production)
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")

			// Content Security Policy
			h.Set("Content-Security-Policy", strings.Join([]string{
				"default-src 'self'",
				"script-src 'self' 'unsafe-inline' 'unsafe-eval' https://challenges.cloudflare.com https://w.soundcloud.com https://www.youtube.com https://s.ytimg.com",
				"style-src 'self' 'unsafe-inline'",
				"img-src 'self' data: blob: https: http:",
				"media-src 'self' blob: https: http:",
				"font-src 'self' data:",
				"connect-src 'self' https: wss:",
				"frame-src https://www.youtube.com https://w.soundcloud.com https://challenges.cloudflare.com",
				"frame-ancestors " + frameAncestors,
				"base-uri 'self'",
				"form-action 'self'",
			}, "; "))

			next.ServeHTTP(w, r)
		})
	}
}

// MaxBodySize limits request body size to prevent abuse.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireJSONContentType rejects mutating requests whose non-empty body
// is not declared as JSON. This is the CSRF backstop for the cross-site
// session cookie (SameSite=None): a hostile page can auto-submit an HTML
// form with enctype=text/plain carrying a JSON-shaped body and the
// browser will attach the cookie — but it cannot set Content-Type:
// application/json without a CORS preflight, which the CORS policy gates.
func RequireJSONContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if r.ContentLength != 0 {
				ct := r.Header.Get("Content-Type")
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
					http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
