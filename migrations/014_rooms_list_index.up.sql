-- Rooms-list hot path: GET /api/rooms filters on ended_at and orders by
-- is_live DESC, last_active_at DESC NULLS LAST, created_at DESC.
-- This index matches that ordering exactly so Postgres can walk it instead
-- of sorting every request (the homepage hits this query constantly).
CREATE INDEX IF NOT EXISTS idx_rooms_list_order
    ON rooms (is_live DESC, last_active_at DESC NULLS LAST, created_at DESC);
