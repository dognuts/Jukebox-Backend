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
