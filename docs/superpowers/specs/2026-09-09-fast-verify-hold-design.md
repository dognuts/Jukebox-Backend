# Fast-verification hold and signup forensics

Date: 2026-09-09

## Problem

Automated signups are verifying their email addresses 12 to 19 seconds after
account creation. Every human account in production took 17 minutes or more.
The gap is wide enough to act on, but the app records neither the time of
verification nor the network the signup came from, so admins cannot see it
without opening psql.

## Goals

1. Record signup IP and user agent per account.
2. Record verification time per account and expose seconds-to-verify in the
   admin users page.
3. Hold accounts that verify faster than a configurable threshold: keep them
   unverified and flagged, while returning the normal success response so
   bots do not learn what tripped them.

## Non-goals

- Blocking datacenter IP ranges at signup. Revisit once IPs are recorded.
- Automatic banning. Hold is reversible by an admin with one click.

## Data model

Migration `015_signup_forensics`:

```sql
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_ip         TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS verified_at       TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS verify_held_at    TIMESTAMPTZ;
ALTER TABLE email_verifications ADD COLUMN IF NOT EXISTS used_ip TEXT NOT NULL DEFAULT '';
```

Backfill `verified_at` from the newest email_verifications row per user
whose `used_at` is set, regardless of the current `email_verified` flag
(admins may have un-verified accounts by hand). The down migration drops
the five columns.

`verified_at` is stored on the user rather than computed from tokens
because a resend marks the previous token used at resend time, which would
otherwise produce false verification timestamps.

## Backend

### Config

`VERIFY_HOLD_SECONDS` (int, default 60). 0 disables the hold.

### Signup (`POST /api/auth/signup`)

After the existing anti-spam checks, save `ClientIP(r)` and the
`User-Agent` header (truncated to 512 bytes) on the new user row.

### Verify (`POST /api/auth/verify-email`)

After token validation:

1. Load the user. Compute `elapsed = now - user.created_at`.
2. Mark the token used and record the clicking IP.
3. If threshold > 0 and elapsed < threshold: set `verify_held_at = now`,
   leave `email_verified` false, log `[antispam] fast verify held`, and
   return `{"status":"email verified"}` with 200.
4. Otherwise: set `email_verified = true`, `verified_at = now`,
   `verify_held_at = NULL`.

Participation is already gated on `email_verified`, so a held account
cannot post, DJ, or create rooms.

### Admin

`AdminSetField` for `email_verified = true` also clears `verify_held_at`,
so the existing Verify Email button releases a hold. Setting it false
leaves `verified_at` untouched (it records the click, not the flag).

`GET /api/admin/users` and `GET /api/admin/users/{id}` return an
admin-only view: every existing `User` field plus

| field            | type            |
|------------------|-----------------|
| signupIp         | string          |
| signupUserAgent  | string          |
| verifiedAt       | RFC3339 or null |
| verifyHeldAt     | RFC3339 or null |
| secondsToVerify  | int or null     |

`secondsToVerify` is `verified_at - created_at` rounded down, null when
`verified_at` is null. The forensic fields live on `models.User` with
`json:"-"` and are copied into the admin view, so `/api/auth/me` and
profile endpoints never expose them.

## Frontend (admin users page)

- List rows: show "Verified in Ns" next to the join date. Under 60 seconds
  render a red "Fast" badge. Held accounts get a "Held" badge.
- Detail panel: InfoRows for Verified in, Signup IP, and User agent, plus
  the Held badge. The existing Verify Email button is the release action.
- The 60-second badge threshold is a frontend constant. It mirrors the
  backend default but is only a display hint.

## Testing

- Handler: verify under threshold returns 200 success, user stays
  unverified, `verify_held_at` set. Verify over threshold sets
  `email_verified` and `verified_at`. Threshold 0 never holds.
- Handler: signup stores IP and user agent; user agent truncated.
- Config: default 60, env override, invalid value falls back to default.
- Store/migration: backfill picks the newest token per user and includes
  un-verified users whose token was used.
- Admin view: forensic fields present in admin JSON, absent from `/me`.
