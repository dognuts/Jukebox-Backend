-- Make stage_name unique (case-insensitive) and non-empty for registered users
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_stage_name_unique ON users (LOWER(stage_name)) WHERE stage_name != '';
