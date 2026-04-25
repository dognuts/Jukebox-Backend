package antispam

import (
	"context"
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
func (rl *RateLimiter) AllowSignup(ctx context.Context, ip string) (bool, error) {
	return rl.checkRate(ctx, "signup_rate:"+ip, int64(rl.maxPerHour))
}

// AllowLogin returns true if the IP has not exceeded the login rate limit.
// Login is more generous (2x signup) to avoid locking out legitimate users.
func (rl *RateLimiter) AllowLogin(ctx context.Context, ip string) (bool, error) {
	return rl.checkRate(ctx, "login_rate:"+ip, int64(rl.maxPerHour*2))
}

// AllowSupportReport returns true if the IP has not exceeded the
// listener-support-report rate limit (maxPerHour, same as signup).
// Uses the same fixed-window Redis INCR+EXPIRE approach as the other limiters.
func (rl *RateLimiter) AllowSupportReport(ctx context.Context, ip string) (bool, error) {
	return rl.checkRate(ctx, "support_report_rate:"+ip, int64(rl.maxPerHour))
}

func (rl *RateLimiter) checkRate(ctx context.Context, key string, max int64) (bool, error) {
	if key == "" {
		return true, nil
	}
	// Check if key contains localhost IPs
	if len(key) > 5 {
		suffix := key[len(key)-9:]
		if suffix == "127.0.0.1" || key[len(key)-3:] == "::1" {
			return true, nil
		}
	}

	pipe := rl.rdb.Pipeline()
	incrCmd := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 1*time.Hour)
	_, err := pipe.Exec(ctx)
	if err != nil {
		// If Redis is down, allow the request rather than blocking everyone
		return true, err
	}

	count := incrCmd.Val()
	return count <= max, nil
}
