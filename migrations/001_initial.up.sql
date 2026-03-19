-- Jukebox MVP schema
-- Anonymous sessions + DJ key auth

-- ============================================================
-- Rooms
-- ============================================================
CREATE TABLE rooms (
    id             TEXT PRIMARY KEY,
    slug           TEXT UNIQUE NOT NULL,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    genre          TEXT NOT NULL DEFAULT '',
    vibes          TEXT[] NOT NULL DEFAULT '{}',
    cover_gradient TEXT NOT NULL DEFAULT '',
    cover_art_url  TEXT NOT NULL DEFAULT '',
    request_policy TEXT NOT NULL DEFAULT 'open'
                       CHECK (request_policy IN ('open', 'approval', 'closed')),
    is_live        BOOLEAN NOT NULL DEFAULT FALSE,
    is_official    BOOLEAN NOT NULL DEFAULT FALSE,
    dj_key_hash    TEXT NOT NULL,  -- bcrypt hash of the room's DJ key
    dj_session_id  TEXT NOT NULL DEFAULT '', -- session that created / controls room
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    scheduled_start TIMESTAMPTZ,
    last_active_at  TIMESTAMPTZ
);

CREATE INDEX idx_rooms_slug ON rooms (slug);
CREATE INDEX idx_rooms_is_live ON rooms (is_live);
CREATE INDEX idx_rooms_genre ON rooms (genre);
CREATE INDEX idx_rooms_scheduled ON rooms (scheduled_start) WHERE scheduled_start IS NOT NULL;

-- ============================================================
-- Tracks  (canonical track records, de-duplicated by source URL)
-- ============================================================
CREATE TABLE tracks (
    id             TEXT PRIMARY KEY,
    title          TEXT NOT NULL,
    artist         TEXT NOT NULL,
    duration       INTEGER NOT NULL,  -- seconds
    source         TEXT NOT NULL CHECK (source IN ('youtube', 'soundcloud', 'mp3')),
    source_url     TEXT NOT NULL,
    album_gradient TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tracks_source_url ON tracks (source_url);

-- ============================================================
-- Queue entries  (per-room track queue)
-- ============================================================
CREATE TABLE queue_entries (
    id            TEXT PRIMARY KEY,
    room_id       TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    track_id      TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    submitted_by  TEXT NOT NULL DEFAULT 'Anonymous',  -- display name
    session_id    TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'approved'
                      CHECK (status IN ('pending', 'approved', 'rejected', 'played')),
    position      INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_queue_room_status ON queue_entries (room_id, status, position);

-- ============================================================
-- Chat messages
-- ============================================================
CREATE TABLE chat_messages (
    id           TEXT PRIMARY KEY,
    room_id      TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    session_id   TEXT NOT NULL DEFAULT '',
    username     TEXT NOT NULL,
    avatar_color TEXT NOT NULL DEFAULT '',
    message      TEXT NOT NULL,
    msg_type     TEXT NOT NULL DEFAULT 'message'
                     CHECK (msg_type IN ('message', 'request', 'announcement')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_chat_room_time ON chat_messages (room_id, created_at DESC);

-- ============================================================
-- Now-playing  (which track is currently playing per room)
-- ============================================================
CREATE TABLE now_playing (
    room_id    TEXT PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
    track_id   TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
