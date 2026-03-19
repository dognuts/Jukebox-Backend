-- Add ended_at to track when a room session ended
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS ended_at TIMESTAMPTZ;
