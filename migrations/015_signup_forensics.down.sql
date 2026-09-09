ALTER TABLE email_verifications DROP COLUMN IF EXISTS used_ip;
ALTER TABLE users DROP COLUMN IF EXISTS verify_held_at;
ALTER TABLE users DROP COLUMN IF EXISTS verified_at;
ALTER TABLE users DROP COLUMN IF EXISTS signup_user_agent;
ALTER TABLE users DROP COLUMN IF EXISTS signup_ip;
