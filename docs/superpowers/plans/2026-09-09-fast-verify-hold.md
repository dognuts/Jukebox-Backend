# Fast-Verification Hold Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record signup IP/user-agent and verification time per account, hold accounts that verify faster than a threshold, and surface all of it in the admin users page.

**Architecture:** One additive migration with a backfill. Store methods gain the new columns. The auth handler makes the hold decision through a pure, unit-tested helper and returns the normal success body either way. Admin endpoints wrap `models.User` in an admin-only view so forensic fields never reach `/api/auth/me`. The Next.js admin page renders the new fields.

**Tech Stack:** Go, chi, pgx v5, Postgres, Next.js/React (TypeScript). Spec: `docs/superpowers/specs/2026-09-09-fast-verify-hold-design.md`.

Backend root: `Jukebox-Backend/`. Frontend root: `Jukebox-Frontend/`. Run Go tests without `-race` (toolchain limitation). DB-backed tests need `TEST_DATABASE_URL`; local ports 5432/6379 are taken, so use a throwaway Postgres on 55432:
`docker run -d --name jb-migtest -e POSTGRES_USER=jukebox -e POSTGRES_PASSWORD=jukebox -e POSTGRES_DB=jukebox -p 55432:5432 postgres:16`

---

### Task 1: Migration 015 with backfill

**Files:**
- Create: `migrations/015_signup_forensics.up.sql`
- Create: `migrations/015_signup_forensics.down.sql`
- Test: `internal/store/migrations_test.go`

- [ ] **Step 1: Write the failing DB-backed test** (append to `internal/store/migrations_test.go`)

```go
// TestMigration015BackfillsVerifiedAt: 015 must copy the newest used
// verification token's used_at onto users.verified_at, for verified AND
// admin-unverified users, and must ignore tokens that were only marked
// used by a resend (older token) — the newest row per user wins.
func TestMigration015BackfillsVerifiedAt(t *testing.T) {
	s := newMigrationTestDB(t)
	ctx := context.Background()

	// Apply everything before 015 from a temp copy of the migrations dir.
	pre := t.TempDir()
	files, err := listUpMigrations(testMigrationsDir)
	if err != nil {
		t.Fatalf("listUpMigrations: %v", err)
	}
	for _, f := range files {
		if f >= "015" {
			break
		}
		b, err := os.ReadFile(filepath.Join(testMigrationsDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pre, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RunMigrations(ctx, pre); err != nil {
		t.Fatalf("RunMigrations pre-015: %v", err)
	}

	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// u1: verified, resent once. Old token marked used at resend (+5s),
	// new token used at +15s. Backfill must pick +15s.
	mustExec(`INSERT INTO users (id, email, password_hash, display_name, avatar_color, created_at, updated_at, email_verified)
	          VALUES ('u1','u1@x.com','h','u1','c',$1,$1,TRUE)`, base)
	mustExec(`INSERT INTO email_verifications (id, user_id, token, expires_at, used_at, created_at)
	          VALUES ('t1a','u1','tok1a',$1,$2,$3)`, base.Add(24*time.Hour), base.Add(5*time.Second), base)
	mustExec(`INSERT INTO email_verifications (id, user_id, token, expires_at, used_at, created_at)
	          VALUES ('t1b','u1','tok1b',$1,$2,$3)`, base.Add(24*time.Hour), base.Add(15*time.Second), base.Add(5*time.Second))
	// u2: admin un-verified later, but token was used at +20m. Still backfilled.
	mustExec(`INSERT INTO users (id, email, password_hash, display_name, avatar_color, created_at, updated_at, email_verified)
	          VALUES ('u2','u2@x.com','h','u2','c',$1,$1,FALSE)`, base)
	mustExec(`INSERT INTO email_verifications (id, user_id, token, expires_at, used_at, created_at)
	          VALUES ('t2','u2','tok2',$1,$2,$3)`, base.Add(24*time.Hour), base.Add(20*time.Minute), base)
	// u3: never clicked. verified_at stays NULL.
	mustExec(`INSERT INTO users (id, email, password_hash, display_name, avatar_color, created_at, updated_at, email_verified)
	          VALUES ('u3','u3@x.com','h','u3','c',$1,$1,FALSE)`, base)
	mustExec(`INSERT INTO email_verifications (id, user_id, token, expires_at, created_at)
	          VALUES ('t3','u3','tok3',$1,$2)`, base.Add(24*time.Hour), base)

	if err := s.RunMigrations(ctx, testMigrationsDir); err != nil {
		t.Fatalf("RunMigrations full: %v", err)
	}
	if !migrationRecorded(t, s, "015_signup_forensics.up.sql") {
		t.Fatal("015 not recorded")
	}

	got := map[string]*time.Time{}
	rows, err := s.pool.Query(ctx, `SELECT id, verified_at FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var v *time.Time
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		got[id] = v
	}
	want := map[string]*time.Time{
		"u1": ptrTime(base.Add(15 * time.Second)),
		"u2": ptrTime(base.Add(20 * time.Minute)),
		"u3": nil,
	}
	for id, w := range want {
		g := got[id]
		switch {
		case w == nil && g != nil:
			t.Errorf("%s: verified_at = %v, want NULL", id, *g)
		case w != nil && g == nil:
			t.Errorf("%s: verified_at = NULL, want %v", id, *w)
		case w != nil && g != nil && !g.Equal(*w):
			t.Errorf("%s: verified_at = %v, want %v", id, *g, *w)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
```

- [ ] **Step 2: Run it to verify it fails**

Run (PowerShell): `$env:TEST_DATABASE_URL="postgres://jukebox:jukebox@localhost:55432/jukebox?sslmode=disable"; go test ./internal/store/ -run TestMigration015 -v`
Expected: FAIL with "015 not recorded" (file does not exist yet).

- [ ] **Step 3: Write the migration**

`migrations/015_signup_forensics.up.sql`:
```sql
-- Signup forensics + fast-verification hold.
-- Bots verify their email 12-19s after signup; humans take 17+ minutes.
-- Record where a signup came from and when it verified so admins can see
-- it, and give the verify handler a place to park suspicious accounts.
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_ip         TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS verified_at       TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS verify_held_at    TIMESTAMPTZ;
ALTER TABLE email_verifications ADD COLUMN IF NOT EXISTS used_ip TEXT NOT NULL DEFAULT '';

-- Backfill verified_at from the NEWEST token per user. A resend marks the
-- previous token used at resend time, so older rows are not real clicks.
-- Includes users an admin has since un-verified: verified_at records the
-- click, not the current flag.
UPDATE users u
SET verified_at = v.used_at
FROM (
    SELECT DISTINCT ON (user_id) user_id, used_at
    FROM email_verifications
    ORDER BY user_id, created_at DESC
) v
WHERE v.user_id = u.id AND v.used_at IS NOT NULL AND u.verified_at IS NULL;
```

`migrations/015_signup_forensics.down.sql`:
```sql
ALTER TABLE email_verifications DROP COLUMN IF EXISTS used_ip;
ALTER TABLE users DROP COLUMN IF EXISTS verify_held_at;
ALTER TABLE users DROP COLUMN IF EXISTS verified_at;
ALTER TABLE users DROP COLUMN IF EXISTS signup_user_agent;
ALTER TABLE users DROP COLUMN IF EXISTS signup_ip;
```

- [ ] **Step 4: Run the test to verify it passes**

Same command as Step 2. Expected: PASS. Also run `go test ./internal/store/ -run TestListUpMigrations -v` and expect PASS.

- [ ] **Step 5: Commit**

```bash
git add migrations/015_signup_forensics.up.sql migrations/015_signup_forensics.down.sql internal/store/migrations_test.go
git commit -m "feat(db): signup forensics columns and verified_at backfill"
```

---

### Task 2: Model and store

**Files:**
- Modify: `internal/models/models.go:123-146` (User struct)
- Modify: `internal/store/users.go` (CreateUser, GetUserByEmail, GetUserByID, SetEmailVerified, MarkEmailVerificationUsed, AdminListUsers, AdminGetUser; add HoldEmailVerification, AdminSetEmailVerified)

No unit test for this task (all SQL). The DB-backed round-trip is covered by the migration harness in Step 3.

- [ ] **Step 1: Add fields to `models.User`** (after `IsBanned`)

```go
	// Signup forensics: admin-only. json:"-" keeps them out of /api/auth/me
	// and profile responses; the admin handler copies them into its own view.
	SignupIP        string     `json:"-"`
	SignupUserAgent string     `json:"-"`
	VerifiedAt      *time.Time `json:"-"`
	VerifyHeldAt    *time.Time `json:"-"`
```

- [ ] **Step 2: Update store queries in `internal/store/users.go`**

CreateUser:
```go
func (s *PGStore) CreateUser(ctx context.Context, user *models.User) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, email, email_verified, password_hash, display_name, avatar_color, avatar_url, bio, favorite_genres, created_at, updated_at, stage_name, signup_ip, signup_user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		user.ID, user.Email, user.EmailVerified, user.PasswordHash, user.DisplayName,
		user.AvatarColor, user.AvatarURL, user.Bio, user.FavoriteGenres, user.CreatedAt, user.UpdatedAt, user.StageName,
		user.SignupIP, user.SignupUserAgent,
	)
	return err
}
```

GetUserByEmail and GetUserByID: append `, signup_ip, signup_user_agent, verified_at, verify_held_at` to the SELECT list (after `is_banned`) and `, &u.SignupIP, &u.SignupUserAgent, &u.VerifiedAt, &u.VerifyHeldAt` to the Scan.

SetEmailVerified and the new hold method:
```go
// SetEmailVerified marks the account verified, stamps verified_at (first
// click only), and clears any fast-verify hold.
func (s *PGStore) SetEmailVerified(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users
		SET email_verified = TRUE, verified_at = COALESCE(verified_at, NOW()), verify_held_at = NULL, updated_at = NOW()
		WHERE id = $1`, userID)
	return err
}

// HoldEmailVerification records that a verification click was suspiciously
// fast. The account stays unverified until an admin releases it.
func (s *PGStore) HoldEmailVerification(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users
		SET verified_at = COALESCE(verified_at, NOW()), verify_held_at = NOW(), updated_at = NOW()
		WHERE id = $1`, userID)
	return err
}
```

MarkEmailVerificationUsed gains the clicking IP:
```go
func (s *PGStore) MarkEmailVerificationUsed(ctx context.Context, id, ip string) error {
	_, err := s.pool.Exec(ctx, `UPDATE email_verifications SET used_at = NOW(), used_ip = $2 WHERE id = $1`, id, ip)
	return err
}
```

AdminListUsers and AdminGetUser: append `, signup_ip, signup_user_agent, verified_at, verify_held_at` to both SELECT lists (after `is_banned`) and `, &u.SignupIP, &u.SignupUserAgent, &u.VerifiedAt, &u.VerifyHeldAt` to both Scans.

Add after AdminSetField:
```go
// AdminSetEmailVerified flips the flag by hand. Verifying also releases a
// fast-verify hold; un-verifying leaves verified_at alone (it records the
// click, not the flag).
func (s *PGStore) AdminSetEmailVerified(ctx context.Context, userID string, verified bool) error {
	if verified {
		_, err := s.pool.Exec(ctx, `UPDATE users SET email_verified = TRUE, verify_held_at = NULL WHERE id = $1`, userID)
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET email_verified = FALSE WHERE id = $1`, userID)
	return err
}
```

- [ ] **Step 3: Build and fix the one caller**

Run: `go build ./...`
Expected: one error in `internal/handlers/auth.go` because `MarkEmailVerificationUsed` now takes an IP. Change that call to `h.pg.MarkEmailVerificationUsed(ctx, v.ID, ClientIP(r))` for now (Task 4 rewrites this block). Rebuild until clean. Run `go test ./internal/store/ -run TestRunMigrationsFreshDB -v` with `TEST_DATABASE_URL` set and expect PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/models/models.go internal/store/users.go internal/handlers/auth.go
git commit -m "feat(store): persist signup forensics, verified_at, and verify hold"
```

---

### Task 3: Config `VERIFY_HOLD_SECONDS`

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
package config

import (
	"testing"
	"time"
)

func TestVerifyHoldSeconds(t *testing.T) {
	tests := []struct {
		env  string
		want time.Duration
	}{
		{"", 60 * time.Second},    // default
		{"120", 120 * time.Second},
		{"0", 0},                  // disabled
		{"abc", 60 * time.Second}, // garbage falls back to default
		{"-5", 60 * time.Second},  // negative falls back to default
	}
	for _, tt := range tests {
		t.Run("env="+tt.env, func(t *testing.T) {
			t.Setenv("VERIFY_HOLD_SECONDS", tt.env)
			if got := verifyHoldFromEnv(); got != tt.want {
				t.Errorf("verifyHoldFromEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL, undefined `verifyHoldFromEnv`.

- [ ] **Step 3: Implement**

Add to the `Config` struct (after `DevPayments`):
```go
	// VerifyHold: email verifications that arrive sooner than this after
	// signup are held for admin review instead of verifying. Bots verify in
	// 12-19s; the fastest human on record took 17 minutes. 0 disables.
	VerifyHold time.Duration
```
In `Load()` set `VerifyHold: verifyHoldFromEnv(),`. Add the helper:
```go
const defaultVerifyHoldSeconds = 60

// verifyHoldFromEnv parses VERIFY_HOLD_SECONDS. Unset, non-numeric, or
// negative values fall back to the default; 0 disables the hold.
func verifyHoldFromEnv() time.Duration {
	raw := getEnv("VERIFY_HOLD_SECONDS", "")
	if raw == "" {
		return defaultVerifyHoldSeconds * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultVerifyHoldSeconds * time.Second
	}
	return time.Duration(n) * time.Second
}
```
Check `getEnv`: if it returns the fallback only when the variable is unset, an empty-but-set variable yields "" and the code above still handles it.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/config/ -v` and expect PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): VERIFY_HOLD_SECONDS"
```

---

### Task 4: Auth handler: record signup forensics, hold fast verifies

**Files:**
- Modify: `internal/handlers/auth.go` (struct, NewAuthHandler, Signup near line 158, VerifyEmail near line 443)
- Modify: `cmd/server/main.go:154`
- Test: `internal/handlers/auth_test.go`

- [ ] **Step 1: Write the failing tests** (append to `auth_test.go`; add `"time"` to imports)

```go
func TestShouldHoldVerification(t *testing.T) {
	created := time.Date(2026, 9, 7, 15, 25, 23, 0, time.UTC)
	tests := []struct {
		desc      string
		elapsed   time.Duration
		threshold time.Duration
		want      bool
	}{
		{"bot-speed click is held", 12 * time.Second, 60 * time.Second, true},
		{"just under threshold is held", 59 * time.Second, 60 * time.Second, true},
		{"exactly threshold is not held", 60 * time.Second, 60 * time.Second, false},
		{"human-speed click is not held", 30 * time.Minute, 60 * time.Second, false},
		{"threshold 0 disables hold", 1 * time.Second, 0, false},
		{"clock skew (negative elapsed) is held", -2 * time.Second, 60 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := shouldHoldVerification(created, created.Add(tt.elapsed), tt.threshold)
			if got != tt.want {
				t.Errorf("shouldHoldVerification(elapsed=%v, threshold=%v) = %v, want %v", tt.elapsed, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestTruncateUserAgent(t *testing.T) {
	short := "Mozilla/5.0"
	if got := truncateUserAgent(short); got != short {
		t.Errorf("short UA changed: %q", got)
	}
	long := strings.Repeat("x", 600)
	if got := truncateUserAgent(long); len(got) != maxUserAgentLen {
		t.Errorf("long UA len = %d, want %d", len(got), maxUserAgentLen)
	}
	if got := truncateUserAgent(""); got != "" {
		t.Errorf("empty UA = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/handlers/ -run 'TestShouldHoldVerification|TestTruncateUserAgent' -v`
Expected: FAIL, undefined `shouldHoldVerification`, `truncateUserAgent`, `maxUserAgentLen`.

- [ ] **Step 3: Implement helpers and wire them in**

Add to `auth.go` (near `generateSecureToken`):
```go
// maxUserAgentLen bounds what we store from the User-Agent header.
const maxUserAgentLen = 512

func truncateUserAgent(ua string) string {
	if len(ua) > maxUserAgentLen {
		return ua[:maxUserAgentLen]
	}
	return ua
}

// shouldHoldVerification decides whether an email-verification click is
// fast enough to look automated. Anything under the threshold, including a
// negative elapsed time from clock skew, is held. threshold <= 0 disables.
func shouldHoldVerification(createdAt, now time.Time, threshold time.Duration) bool {
	if threshold <= 0 {
		return false
	}
	return now.Sub(createdAt) < threshold
}
```

Struct + constructor: add `verifyHold time.Duration` to `AuthHandler`; add a trailing `verifyHold time.Duration` parameter to `NewAuthHandler` and assign it.

Signup (building the `user` literal near line 158): add
```go
		SignupIP:        ip,
		SignupUserAgent: truncateUserAgent(r.Header.Get("User-Agent")),
```
`ip` is already computed above the Turnstile check.

VerifyEmail: replace the two lines after the expiry check with
```go
	ip := ClientIP(r)
	h.pg.MarkEmailVerificationUsed(ctx, v.ID, ip)

	user, err := h.pg.GetUserByID(ctx, v.UserID)
	if err != nil || user == nil {
		http.Error(w, "invalid verification token", http.StatusBadRequest)
		return
	}
	if shouldHoldVerification(user.CreatedAt, time.Now(), h.verifyHold) {
		// Bot-speed click. Park the account and return the normal success
		// body so the operator doesn't learn what tripped it (same idea as
		// the signup honeypot). Admins release from the users page.
		log.Printf("[antispam] fast verify held: user=%s elapsed=%s ip=%s", user.ID, time.Since(user.CreatedAt).Round(time.Second), ip)
		h.pg.HoldEmailVerification(ctx, user.ID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "email verified"})
		return
	}
	h.pg.SetEmailVerified(ctx, v.UserID)
```
Keep the existing final `writeJSON(... "email verified")` for the normal path.

`cmd/server/main.go:154`: append `, cfg.VerifyHold` to the `NewAuthHandler` call.

- [ ] **Step 4: Build and test**

Run: `go build ./... && go test ./internal/handlers/ ./internal/config/`
Expected: both packages ok, no FAIL.

- [ ] **Step 5: Commit**

```bash
git add internal/handlers/auth.go internal/handlers/auth_test.go cmd/server/main.go
git commit -m "feat(auth): record signup IP/UA and hold bot-speed email verifications"
```

---

### Task 5: Admin view with forensic fields

**Files:**
- Create: `internal/handlers/admin_user_view.go`
- Modify: `internal/handlers/admin.go:403-485` (ListUsers, GetUser, UpdateUser)
- Test: `internal/handlers/admin_user_view_test.go`

- [ ] **Step 1: Write the failing test**

```go
package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jukebox/backend/internal/models"
)

func TestAdminUserViewExposesForensics(t *testing.T) {
	created := time.Date(2026, 9, 7, 15, 25, 23, 0, time.UTC)
	verified := created.Add(12*time.Second + 600*time.Millisecond)
	held := created.Add(13 * time.Second)
	u := models.User{
		ID: "u1", Email: "a@b.c", CreatedAt: created,
		SignupIP: "203.0.113.9", SignupUserAgent: "curl/8.0",
		VerifiedAt: &verified, VerifyHeldAt: &held,
	}
	view := newAdminUserView(u)
	if view.SignupIP != "203.0.113.9" || view.SignupUserAgent != "curl/8.0" {
		t.Errorf("forensics not copied: %+v", view)
	}
	if view.SecondsToVerify == nil || *view.SecondsToVerify != 12 {
		t.Errorf("SecondsToVerify = %v, want 12 (rounded down)", view.SecondsToVerify)
	}
	b, _ := json.Marshal(view)
	for _, key := range []string{`"signupIp"`, `"signupUserAgent"`, `"verifiedAt"`, `"verifyHeldAt"`, `"secondsToVerify":12`, `"email"`, `"createdAt"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("admin JSON missing %s: %s", key, b)
		}
	}
}

func TestAdminUserViewNullsWhenNeverVerified(t *testing.T) {
	view := newAdminUserView(models.User{ID: "u2", CreatedAt: time.Now()})
	if view.SecondsToVerify != nil {
		t.Errorf("SecondsToVerify = %v, want nil", *view.SecondsToVerify)
	}
	b, _ := json.Marshal(view)
	if !strings.Contains(string(b), `"secondsToVerify":null`) || !strings.Contains(string(b), `"verifiedAt":null`) {
		t.Errorf("want explicit nulls, got %s", b)
	}
}

// The plain User model backs /api/auth/me and profiles; forensics must not leak.
func TestUserModelHidesForensics(t *testing.T) {
	now := time.Now()
	b, _ := json.Marshal(models.User{ID: "u", SignupIP: "203.0.113.9", SignupUserAgent: "curl", VerifiedAt: &now, VerifyHeldAt: &now})
	for _, leak := range []string{"203.0.113.9", "curl", "verifiedAt", "verifyHeldAt", "signupIp"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("models.User JSON leaks %q: %s", leak, b)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/handlers/ -run 'TestAdminUserView|TestUserModelHides' -v`
Expected: FAIL, undefined `newAdminUserView`.

- [ ] **Step 3: Implement `internal/handlers/admin_user_view.go`**

```go
package handlers

import (
	"time"

	"github.com/jukebox/backend/internal/models"
)

// adminUserView is what /api/admin/users returns: every public User field
// plus the signup forensics that models.User deliberately hides from
// non-admin responses.
type adminUserView struct {
	models.User
	SignupIP        string     `json:"signupIp"`
	SignupUserAgent string     `json:"signupUserAgent"`
	VerifiedAt      *time.Time `json:"verifiedAt"`
	VerifyHeldAt    *time.Time `json:"verifyHeldAt"`
	// SecondsToVerify is verified_at - created_at, rounded down. nil when
	// the account never clicked a verification link.
	SecondsToVerify *int64 `json:"secondsToVerify"`
}

func newAdminUserView(u models.User) adminUserView {
	v := adminUserView{
		User:            u,
		SignupIP:        u.SignupIP,
		SignupUserAgent: u.SignupUserAgent,
		VerifiedAt:      u.VerifiedAt,
		VerifyHeldAt:    u.VerifyHeldAt,
	}
	if u.VerifiedAt != nil {
		secs := int64(u.VerifiedAt.Sub(u.CreatedAt) / time.Second)
		v.SecondsToVerify = &secs
	}
	return v
}

func newAdminUserViews(users []models.User) []adminUserView {
	out := make([]adminUserView, 0, len(users))
	for _, u := range users {
		out = append(out, newAdminUserView(u))
	}
	return out
}
```

Embedding `models.User` whose forensic fields are `json:"-"` and redeclaring them on the outer struct works: the outer fields shadow the embedded ones.

In `admin.go`:
- `ListUsers`: `writeJSON(w, http.StatusOK, newAdminUserViews(users))`
- `GetUser`: `writeJSON(w, http.StatusOK, newAdminUserView(*user))`
- `UpdateUser`: replace `h.pg.AdminSetField(ctx, userID, "email_verified", *req.EmailVerified)` with `h.pg.AdminSetEmailVerified(ctx, userID, *req.EmailVerified)`; the final lines become:
```go
	user, err := h.pg.AdminGetUser(ctx, userID)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, newAdminUserView(*user))
```

- [ ] **Step 4: Build and test**

Run: `go build ./... && go test ./...`
Expected: every package ok, no FAIL.

- [ ] **Step 5: Commit**

```bash
git add internal/handlers/admin_user_view.go internal/handlers/admin_user_view_test.go internal/handlers/admin.go
git commit -m "feat(admin): expose signup forensics and seconds-to-verify"
```

---

### Task 6: Admin users page

**Files:**
- Modify: `Jukebox-Frontend/app/(app)/admin/users/page.tsx` (interface near line 18, list row near line 185, info grid near line 240, badges near line 252)

No unit test harness for this page; verification is type-check, lint, and a browser check.

- [ ] **Step 1: Extend the interface and add helpers**

After the `AdminUser` interface's `country: string` add:
```ts
  signupIp: string
  signupUserAgent: string
  verifiedAt: string | null
  verifyHeldAt: string | null
  secondsToVerify: number | null
```
Below the interface:
```ts
// Mirrors the backend VERIFY_HOLD_SECONDS default. Display hint only.
const FAST_VERIFY_SECONDS = 60

function formatVerifyTime(secs: number | null): string {
  if (secs === null) return "Never"
  if (secs < 60) return `${secs}s`
  if (secs < 3600) return `${Math.floor(secs / 60)}m ${secs % 60}s`
  return `${Math.floor(secs / 3600)}h ${Math.floor((secs % 3600) / 60)}m`
}

const fastBadgeStyle = { background: "oklch(0.62 0.28 30 / 0.12)", color: "var(--text-error)", border: "0.5px solid oklch(0.62 0.28 30 / 0.3)" }
const heldBadgeStyle = { background: "rgba(232,154,60,0.1)", color: "var(--brand-amber)", border: "0.5px solid rgba(232,154,60,0.25)" }
```

- [ ] **Step 2: List row**

Read lines 185-200 to find the secondary line under the display name (it shows the email). Replace that line with:
```tsx
                      <div className="flex items-center gap-1.5 font-sans text-[10px] text-muted-foreground">
                        <span className="truncate">{u.email}</span>
                        {u.secondsToVerify !== null && (
                          <span className="shrink-0">verified in {formatVerifyTime(u.secondsToVerify)}</span>
                        )}
                        {u.secondsToVerify !== null && u.secondsToVerify < FAST_VERIFY_SECONDS && (
                          <Badge className="h-4 px-1 text-[9px]" style={fastBadgeStyle}>Fast</Badge>
                        )}
                        {u.verifyHeldAt && <Badge className="h-4 px-1 text-[9px]" style={heldBadgeStyle}>Held</Badge>}
                      </div>
```

- [ ] **Step 3: Detail panel**

In the info grid, after the `Joined` InfoRow:
```tsx
                <InfoRow label="Verified in" value={formatVerifyTime(selectedUser.secondsToVerify)} />
                <InfoRow label="Signup IP" value={selectedUser.signupIp || "Unknown"} />
                <InfoRow label="User agent" value={selectedUser.signupUserAgent || "Unknown"} />
```
In the badges block, after the `Unverified` badge:
```tsx
                {selectedUser.secondsToVerify !== null && selectedUser.secondsToVerify < FAST_VERIFY_SECONDS && <Badge style={fastBadgeStyle}>Fast verify</Badge>}
                {selectedUser.verifyHeldAt && <Badge style={heldBadgeStyle}>Held for review</Badge>}
```

- [ ] **Step 4: Type-check and lint**

Run in `Jukebox-Frontend/`: `npx tsc --noEmit` then the lint script from `package.json`.
Expected: no errors.

- [ ] **Step 5: Commit**

```bash
git -C ../Jukebox-Frontend add "app/(app)/admin/users/page.tsx"
git -C ../Jukebox-Frontend commit -m "feat(admin): show verify timing, hold state, and signup forensics"
```

---

### Task 7: End-to-end check against a local stack

- [ ] **Step 1:** Backend: `go test ./...` all ok. With `TEST_DATABASE_URL` on 55432: `go test ./internal/store/ -run 'TestRunMigrations|TestMigration015' -v` PASS.
- [ ] **Step 2:** Start backend against the throwaway DB with `VERIFY_HOLD_SECONDS=60`, sign up via the frontend, click the verification link within a minute. Expected: success page shown; admin users page shows the account Unverified with Held and Fast badges, signup IP and UA populated.
- [ ] **Step 3:** In admin, click Verify Email. Expected: Held badge clears, Verified shows Yes.
- [ ] **Step 4:** Report results. User pushes to main and deploys. `VERIFY_HOLD_SECONDS` only needs setting on Render for a value other than 60.
