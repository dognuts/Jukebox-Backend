package antispam

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type RateLimiter struct {
	rdb        *redis.Client
	maxPerHour int
}

func NewRateLimiter(rdb *redis.Client, maxPerHour int) *RateLimiter {
	if maxPerHour <= 0 {
		maxPerHour = 5
	}
	return &RateLimiter{rdb: rdb, maxPerHour: maxPerHour}
}

// AllowSignup returns true if the IP has not exceeded the signup rate limit.
// It increments the counter on each call.
func (rl *RateLimiter) AllowSignup(ctx context.Context, ip string) (bool, error) {
	if ip == "" || ip == "127.0.0.1" || ip == "::1" {
		return true, nil // skip for localhost
	}

	key := fmt.Sprintf("signup_rate:%s", ip)

	pipe := rl.rdb.Pipeline()
	incrCmd := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 1*time.Hour)
	_, err := pipe.Exec(ctx)
	if err != nil {
		// If Redis is down, allow the request rather than blocking all signups
		return true, err
	}

	count := incrCmd.Val()
	return count <= int64(rl.maxPerHour), nil
}
