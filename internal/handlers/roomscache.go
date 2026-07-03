package handlers

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// roomsListCacheTTL is how long the assembled rooms-list payload is reused
// before being rebuilt. Keep it short — listener counts and now-playing
// tracks go stale after this window.
const roomsListCacheTTL = 5 * time.Second

// maxPayloadCacheEntries bounds the cache: keys include the caller-supplied
// genre filter, so an attacker could otherwise grow the map without limit.
const maxPayloadCacheEntries = 256

// maxPayloadCacheKeyLen caps how long a key may be and still be cached.
// Keys embed attacker-controlled query params (the genre filter), so without
// a cap each of the maxPayloadCacheEntries slots could hold a string as large
// as the request line allows, and oversized junk keys would churn out the hot
// homepage entry. Longer keys are served uncached — worst case is the
// pre-cache per-request rebuild, never memory growth or eviction pressure.
const maxPayloadCacheKeyLen = 64

// payloadCache is a tiny in-process TTL cache for marshaled JSON payloads
// with singleflight semantics: concurrent misses on the same key share one
// build instead of stampeding Postgres/Redis.
type payloadCache struct {
	ttl     time.Duration
	group   singleflight.Group
	mu      sync.Mutex
	entries map[string]payloadEntry
}

type payloadEntry struct {
	data    []byte
	expires time.Time
}

func newPayloadCache(ttl time.Duration) *payloadCache {
	return &payloadCache{ttl: ttl, entries: make(map[string]payloadEntry)}
}

func (c *payloadCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.data, true
}

func (c *payloadCache) set(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.entries) >= maxPayloadCacheEntries {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxPayloadCacheEntries {
			// Still full of live entries — drop everything. The cache is
			// best-effort; the next requests simply rebuild.
			c.entries = make(map[string]payloadEntry)
		}
	}
	c.entries[key] = payloadEntry{data: data, expires: now.Add(c.ttl)}
}

// getOrBuild returns the cached payload for key, or runs build exactly once
// for all concurrent callers and caches its result. Build errors are not
// cached — the next request retries. Oversized keys (see
// maxPayloadCacheKeyLen) bypass the cache entirely.
func (c *payloadCache) getOrBuild(key string, build func() ([]byte, error)) ([]byte, error) {
	if len(key) > maxPayloadCacheKeyLen {
		return build()
	}
	if data, ok := c.get(key); ok {
		return data, nil
	}
	v, err, _ := c.group.Do(key, func() (interface{}, error) {
		// Re-check: another flight may have filled the cache between our
		// miss and acquiring the flight.
		if data, ok := c.get(key); ok {
			return data, nil
		}
		data, err := build()
		if err != nil {
			return nil, err
		}
		c.set(key, data)
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}
