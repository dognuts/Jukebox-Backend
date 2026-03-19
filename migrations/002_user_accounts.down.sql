ALTER TABLE rooms DROP COLUMN IF EXISTS owner_user_id;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS password_resets;
DROP TABLE IF EXISTS email_verifications;
DROP TABLE IF EXISTS users;
