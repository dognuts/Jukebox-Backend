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

			// resolve tries cache first, falls back to Redis and stores the
			// result in cache on hit. The Redis TTL refresh is fired only on
			// cache miss — the cache's own TTL bounds how stale the session
			// can get without any refresh, which is acceptable here because
			// Redis sessions outlive the cache by orders of magnitude.
			resolve := func(sid string) *models.Session {
				if s := getCachedSession(sid); s != nil {
					return s
				}
				s, _ := redis.GetSession(ctx, sid)
				if s != nil {
					_ = redis.RefreshSession(ctx, s.ID)
					putCachedSession(sid, s)
				}
				return s
			}

			// Try cookie
			var session *models.Session
			if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
				session = resolve(cookie.Value)
			}

			// Also check query param (for WebSocket connections where cookies may not be sent cross-origin)
			if session == nil {
				if sid := r.URL.Query().Get("session"); sid != "" {
					session = resolve(sid)
				}
			}

			// Also check Authorization header (for non-browser clients)
			if session == nil {
				if sid := r.Header.Get("X-Session-ID"); sid != "" {
					session = resolve(sid)
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
				putCachedSession(session.ID, session)
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
