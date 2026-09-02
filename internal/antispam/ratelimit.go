package antispam

import (
	"context"
	"net"
	"strings"
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

// isLoopbackIP reports whether the string parses to a loopback address.
// This must PARSE the address: the old implementation did a raw suffix
// check for "::1", which exempted every IPv6 client whose address ends in
// ::1 — trivially arranged inside any customer-controlled /64.
func isLoopbackIP(ip string) bool {
	parsed := net.ParseIP(strings.Trim(strings.TrimSpace(ip), "[]"))
	return parsed != nil && parsed.IsLoopback()
}

// AllowSignup returns true if the IP has not exceeded the signup rate limit.
func (rl *RateLimiter) AllowSignup(ctx context.Context, ip string) (bool, error) {
	if isLoopbackIP(ip) {
		return true, nil
	}
	return rl.checkRateWindow(ctx, "signup_rate:"+ip, int64(rl.maxPerHour), time.Hour)
}

// AllowLogin returns true if the IP has not exceeded the login rate limit.
// Login is more generous (2x signup) to avoid locking out legitimate users.
func (rl *RateLimiter) AllowLogin(ctx context.Context, ip string) (bool, error) {
	if isLoopbackIP(ip) {
		return true, nil
	}
	return rl.checkRateWindow(ctx, "login_rate:"+ip, int64(rl.maxPerHour*2), time.Hour)
}

// Per-account login limiting counts FAILURES only, checked without
// incrementing: counting every attempt (or successes) would let an
// attacker who knows a victim's email lock the account out with a cheap
// request loop, and would throttle legitimately active users. The flow is
// LoginAccountBlocked → (on bad password) RecordLoginFailure → (on
// success) ClearLoginFailures.

// LoginAccountBlocked reports whether the account has exhausted its
// failed-login budget for the current window. Read-only — never counts.
func (rl *RateLimiter) LoginAccountBlocked(ctx context.Context, email string) (bool, error) {
	n, err := rl.rdb.Get(ctx, "login_fail:acct:"+email).Int64()
	if err != nil {
		// Missing key or Redis error — don't block (fail open, matching
		// the other limiters).
		return false, nil
	}
	return n >= int64(rl.maxPerHour*2), nil
}

// RecordLoginFailure counts one failed password attempt against the account.
func (rl *RateLimiter) RecordLoginFailure(ctx context.Context, email string) {
	pipe := rl.rdb.Pipeline()
	pipe.Incr(ctx, "login_fail:acct:"+email)
	pipe.ExpireNX(ctx, "login_fail:acct:"+email, time.Hour)
	_, _ = pipe.Exec(ctx)
}

// ClearLoginFailures resets the account's failure budget after a
// successful login.
func (rl *RateLimiter) ClearLoginFailures(ctx context.Context, email string) {
	_ = rl.rdb.Del(ctx, "login_fail:acct:"+email).Err()
}

// AllowSupportReport returns true if the IP has not exceeded the
// listener-support-report rate limit (maxPerHour, same as signup).
func (rl *RateLimiter) AllowSupportReport(ctx context.Context, ip string) (bool, error) {
	if isLoopbackIP(ip) {
		return true, nil
	}
	return rl.checkRateWindow(ctx, "support_report_rate:"+ip, int64(rl.maxPerHour), time.Hour)
}

// AllowEmailSend limits outbound email per (kind, target address) — the
// defense against using forgot-password / resend-verification to bomb a
// victim's inbox and drain the Resend quota. Deliberately keyed on the
// TARGET, not the caller.
func (rl *RateLimiter) AllowEmailSend(ctx context.Context, kind, email string) (bool, error) {
	// Two windows: a short one that stops rapid-fire, and an hourly cap.
	ok, err := rl.checkRateWindow(ctx, "email_burst:"+kind+":"+email, 1, time.Minute)
	if err != nil || !ok {
		return ok, err
	}
	return rl.checkRateWindow(ctx, "email_rate:"+kind+":"+email, 5, time.Hour)
}

// AllowDM limits outbound direct messages per sender.
func (rl *RateLimiter) AllowDM(ctx context.Context, userID string) (bool, error) {
	return rl.checkRateWindow(ctx, "dm_rate:"+userID, 60, time.Hour)
}

// AllowRoomCreate limits room creation per user.
func (rl *RateLimiter) AllowRoomCreate(ctx context.Context, userID string) (bool, error) {
	return rl.checkRateWindow(ctx, "room_create_rate:"+userID, 10, time.Hour)
}

// AllowQueueSubmit limits track submissions per session per room.
func (rl *RateLimiter) AllowQueueSubmit(ctx context.Context, sessionID, roomID string) (bool, error) {
	return rl.checkRateWindow(ctx, "queue_rate:"+roomID+":"+sessionID, 10, time.Minute)
}

// AllowSessionCreate limits anonymous-session creation per IP so a bare
// request loop can't fill Redis with throwaway session keys. Generous:
// a venue NAT can front hundreds of first-time listeners.
func (rl *RateLimiter) AllowSessionCreate(ctx context.Context, ip string) (bool, error) {
	if isLoopbackIP(ip) {
		return true, nil
	}
	return rl.checkRateWindow(ctx, "session_create_rate:"+ip, 300, time.Hour)
}

// AllowPasswordResetIP limits forgot-password requests per source IP, in
// its own bucket — sharing the signup bucket let a handful of reset
// requests block signups from the same NAT and vice versa.
func (rl *RateLimiter) AllowPasswordResetIP(ctx context.Context, ip string) (bool, error) {
	if isLoopbackIP(ip) {
		return true, nil
	}
	return rl.checkRateWindow(ctx, "pwreset_rate:"+ip, int64(rl.maxPerHour), time.Hour)
}

// AllowWSTicket limits ws-ticket minting per IP. Each mint costs a
// Postgres room lookup + a Redis SET, and reconnect loops mint one per
// attempt — generous enough for a NAT full of listeners reconnecting,
// tight enough to stop a mint flood.
func (rl *RateLimiter) AllowWSTicket(ctx context.Context, ip string) (bool, error) {
	if isLoopbackIP(ip) {
		return true, nil
	}
	return rl.checkRateWindow(ctx, "wsticket_rate:"+ip, 900, time.Hour)
}

// checkRateWindow implements a fixed-window counter: INCR + ExpireNX so
// the window's TTL is set exactly once, when the window opens. A plain
// EXPIRE here would refresh the TTL on every hit — including denied ones —
// turning "N per window" into "N total until a full window of silence",
// which converts temporary lockouts on target-keyed limiters into
// attacker-sustainable permanent ones.
func (rl *RateLimiter) checkRateWindow(ctx context.Context, key string, max int64, window time.Duration) (bool, error) {
	if key == "" {
		return true, nil
	}

	pipe := rl.rdb.Pipeline()
	incrCmd := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, window)
	_, err := pipe.Exec(ctx)
	if err != nil {
		// If Redis is down, allow the request rather than blocking everyone
		return true, err
	}

	count := incrCmd.Val()
	return count <= max, nil
}
