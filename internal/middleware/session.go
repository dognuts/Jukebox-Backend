package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
)

type contextKey string

const SessionKey contextKey = "session"

const cookieName = "jukebox_session"

// SessionMiddleware ensures every request has an anonymous session.
// If no valid session cookie exists, one is created.
func SessionMiddleware(redis *store.RedisStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			// Try to load existing session from cookie
			var session *models.Session
			if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
				session, _ = redis.GetSession(ctx, cookie.Value)
				if session != nil {
					// Refresh TTL
					_ = redis.RefreshSession(ctx, session.ID)
				}
			}

			// Also check query param (for WebSocket connections where cookies may not be sent cross-origin)
			if session == nil {
				if sid := r.URL.Query().Get("session"); sid != "" {
					session, _ = redis.GetSession(ctx, sid)
					if session != nil {
						_ = redis.RefreshSession(ctx, session.ID)
					}
				}
			}

			// Also check Authorization header (for non-browser clients)
			if session == nil {
				if sid := r.Header.Get("X-Session-ID"); sid != "" {
					session, _ = redis.GetSession(ctx, sid)
					if session != nil {
						_ = redis.RefreshSession(ctx, session.ID)
					}
				}
			}

			// Create new session if none found
			if session == nil {
				var err error
				session, err = redis.CreateSession(ctx)
				if err != nil {
					http.Error(w, "failed to create session", http.StatusInternalServerError)
					return
				}
				http.SetCookie(w, &http.Cookie{
					Name:     cookieName,
					Value:    session.ID,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteNoneMode,
					Secure:   true,
					MaxAge:   int(24 * time.Hour / time.Second),
				})
			}

			ctx = context.WithValue(ctx, SessionKey, session)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetSession retrieves the session from request context.
func GetSession(ctx context.Context) *models.Session {
	s, _ := ctx.Value(SessionKey).(*models.Session)
	return s
}
