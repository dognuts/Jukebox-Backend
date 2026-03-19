-- ============================================================
-- Private Messages (DMs between users)
-- ============================================================
CREATE TABLE direct_messages (
    id           TEXT PRIMARY KEY,
    from_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    to_user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    message      TEXT NOT NULL,
    read_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_dm_from ON direct_messages (from_user_id, created_at DESC);
CREATE INDEX idx_dm_to ON direct_messages (to_user_id, created_at DESC);
CREATE INDEX idx_dm_conversation ON direct_messages (
    LEAST(from_user_id, to_user_id),
    GREATEST(from_user_id, to_user_id),
    created_at DESC
);

-- ============================================================
-- Playlists (saved by users, persisted server-side)
-- ============================================================
CREATE TABLE playlists (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    is_liked   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_playlists_user ON playlists (user_id, created_at DESC);

-- ============================================================
-- Playlist tracks (join table)
-- ============================================================
CREATE TABLE playlist_tracks (
    id          TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    track_id    TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    position    INT NOT NULL DEFAULT 0,
    added_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(playlist_id, track_id)
);

CREATE INDEX idx_playlist_tracks ON playlist_tracks (playlist_id, position);

-- ============================================================
-- Profile photos (URL reference — actual files on Cloudflare R2 or similar)
-- ============================================================
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_url TEXT NOT NULL DEFAULT '';
