-- Stage name for users
ALTER TABLE users ADD COLUMN IF NOT EXISTS stage_name TEXT NOT NULL DEFAULT '';

-- DJ display name persisted on room
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS dj_display_name TEXT NOT NULL DEFAULT '';
