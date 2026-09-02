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

// SessionMiddleware resolves an existing anonymous session from the
// request (cookie or X-Session-ID header) into context. It deliberately
// does NOT create sessions: when every bare request minted a 24h Redis
// key, a cookieless request loop was a trivial Redis-exhaustion vector —
// and a full Redis fail-opens the rate limiters. Creation happens only in
// EnsureSession (GET /api/session), which the frontend calls at bootstrap.
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

			// Also check header (the frontend's normal transport; cookies
			// are unreliable cross-site). The old ?session= query-param
			// branch is gone — bearer credentials do not belong in URLs,
			// which end up in request logs; WebSocket connections carry
			// identity via single-use tickets instead.
			if session == nil {
				if sid := r.Header.Get("X-Session-ID"); sid != "" {
					session = resolve(sid)
				}
			}

			if session != nil {
				ctx = context.WithValue(ctx, SessionKey, session)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// EnsureSession returns the request's session, creating one (and setting
// the cookie) if none exists. Only session-bootstrap endpoints call this.
func EnsureSession(w http.ResponseWriter, r *http.Request, redis *store.RedisStore) (*models.Session, error) {
	if s := GetSession(r.Context()); s != nil {
		return s, nil
	}
	session, err := redis.CreateSession(r.Context())
	if err != nil {
		return nil, err
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
	return session, nil
}

// GetSession retrieves the session from request context.
func GetSession(ctx context.Context) *models.Session {
	s, _ := ctx.Value(SessionKey).(*models.Session)
	return s
}
