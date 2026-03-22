-- Autoplay rooms: 24/7 rooms that loop a curated playlist
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS is_autoplay BOOLEAN NOT NULL DEFAULT false;

-- Autoplay playlists table — each room can have a "live" playlist and a "staged" (next) playlist
CREATE TABLE IF NOT EXISTS autoplay_playlists (
    id TEXT PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'staged', -- 'live' or 'staged'
    name TEXT NOT NULL DEFAULT '',
    tracks JSONB NOT NULL DEFAULT '[]',  -- ordered array of {title, artist, duration, source, sourceUrl, albumGradient}
    current_index INT NOT NULL DEFAULT 0, -- current position in the playlist (for live only)
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    activated_at TIMESTAMPTZ,
    UNIQUE(room_id, status)
);

CREATE INDEX IF NOT EXISTS idx_autoplay_playlists_room ON autoplay_playlists(room_id);
