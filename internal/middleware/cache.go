package middleware

import (
	"sync"
	"time"

	"github.com/jukebox/backend/internal/models"
)

// Tiny TTL caches for session + user lookups. These live in-process so
// horizontally-scaled deployments will each keep their own copy, which
// is fine for a short TTL — stale-by-a-few-seconds beats a Redis/PG
// round-trip on every request.

type sessionCacheEntry struct {
	session   *models.Session
	expiresAt time.Time
}

type userCacheEntry struct {
	user      *models.User
	expiresAt time.Time
}

var (
	sessionCache   sync.Map // sessionID -> sessionCacheEntry
	userCache      sync.Map // userID -> userCacheEntry
	sessionCacheMu sync.Mutex
	userCacheMu    sync.Mutex
	lastSessionGC  time.Time
	lastUserGC     time.Time
)

const (
	sessionCacheTTL = 20 * time.Second
	userCacheTTL    = 20 * time.Second
	cacheGCInterval = 2 * time.Minute
)

func getCachedSession(sid string) *models.Session {
	v, ok := sessionCache.Load(sid)
	if !ok {
		return nil
	}
	e := v.(sessionCacheEntry)
	if time.Now().After(e.expiresAt) {
		sessionCache.Delete(sid)
		return nil
	}
	return e.session
}

func putCachedSession(sid string, s *models.Session) {
	sessionCache.Store(sid, sessionCacheEntry{
		session:   s,
		expiresAt: time.Now().Add(sessionCacheTTL),
	})
	maybeGCSessionCache()
}

func invalidateCachedSession(sid string) {
	sessionCache.Delete(sid)
}

func getCachedUser(uid string) *models.User {
	v, ok := userCache.Load(uid)
	if !ok {
		return nil
	}
	e := v.(userCacheEntry)
	if time.Now().After(e.expiresAt) {
		userCache.Delete(uid)
		return nil
	}
	return e.user
}

func putCachedUser(uid string, u *models.User) {
	userCache.Store(uid, userCacheEntry{
		user:      u,
		expiresAt: time.Now().Add(userCacheTTL),
	})
	maybeGCUserCache()
}

// InvalidateCachedUser drops the cached user. Call after a mutation to
// user-owned state (profile update, neon balance change, etc.) so the
// next request sees fresh data instead of the stale cached copy.
func InvalidateCachedUser(uid string) {
	userCache.Delete(uid)
}

func maybeGCSessionCache() {
	sessionCacheMu.Lock()
	defer sessionCacheMu.Unlock()
	if time.Since(lastSessionGC) < cacheGCInterval {
		return
	}
	lastSessionGC = time.Now()
	now := time.Now()
	sessionCache.Range(func(k, v any) bool {
		if now.After(v.(sessionCacheEntry).expiresAt) {
			sessionCache.Delete(k)
		}
		return true
	})
}

func maybeGCUserCache() {
	userCacheMu.Lock()
	defer userCacheMu.Unlock()
	if time.Since(lastUserGC) < cacheGCInterval {
		return
	}
	lastUserGC = time.Now()
	now := time.Now()
	userCache.Range(func(k, v any) bool {
		if now.After(v.(userCacheEntry).expiresAt) {
			userCache.Delete(k)
		}
		return true
	})
}
