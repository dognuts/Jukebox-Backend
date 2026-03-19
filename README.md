# Jukebox Backend

Go backend for the Jukebox music streaming app — "Twitch for Radio." Provides real-time synchronized playback, room management, live chat, and DJ controls via REST API + WebSockets.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  Next.js Frontend (v0)                                  │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐              │
│  │ REST API │  │WebSocket │  │  Cookie   │              │
│  │ Calls    │  │Connection│  │ (Session) │              │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘              │
└───────┼──────────────┼─────────────┼────────────────────┘
        │              │             │
┌───────┼──────────────┼─────────────┼────────────────────┐
│  Go Server (Chi)     │             │                    │
│  ┌───────────────────┴─────────────┴──────────────────┐ │
│  │            Session Middleware                       │ │
│  │     (auto-creates anonymous sessions via cookie)   │ │
│  └────────────────────────────────────────────────────┘ │
│                                                         │
│  ┌─────────────┐  ┌─────────────┐  ┌────────────────┐  │
│  │ REST        │  │ WebSocket   │  │ Playback Sync  │  │
│  │ Handlers    │  │ Hub Manager │  │ Service         │  │
│  │ (rooms,     │  │ (per-room   │  │ (auto-advance  │  │
│  │  queue,     │  │  fan-out)   │  │  tracks when   │  │
│  │  session)   │  │             │  │  they end)     │  │
│  └──────┬──────┘  └──────┬──────┘  └───────┬────────┘  │
│         │                │                  │           │
│  ┌──────┴────────────────┴──────────────────┴────────┐  │
│  │                   Store Layer                     │  │
│  │  ┌──────────────┐        ┌──────────────────┐     │  │
│  │  │  PostgreSQL   │        │     Redis         │     │  │
│  │  │  - rooms      │        │  - sessions       │     │  │
│  │  │  - tracks     │        │  - playback state │     │  │
│  │  │  - queue      │        │  - listener sets  │     │  │
│  │  │  - chat       │        │  - pub/sub        │     │  │
│  │  │  - now_playing │        │                   │     │  │
│  │  └──────────────┘        └──────────────────┘     │  │
│  └───────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

## Auth Model (MVP)

No user accounts. Instead:
- **Anonymous sessions**: Every visitor gets a session cookie with a generated display name + avatar color. Sessions live in Redis with a 24h TTL.
- **DJ key**: When a room is created, the server returns a secret `djKey` (32-char hex string). The creator shares this key to grant DJ permissions. Pass it via `?djKey=...` query param or `X-DJ-Key` header.

## Quick Start

```bash
# 1. Start Postgres + Redis
docker compose up -d

# 2. Copy env file
cp .env.example .env

# 3. Run the server (auto-applies migrations)
make run
```

Server starts on `http://localhost:8080`.

## Project Structure

```
cmd/server/main.go          Entry point, wires everything together
internal/
  config/config.go          Environment config loader
  middleware/
    session.go              Anonymous session middleware (cookie-based)
    djauth.go               DJ key generation + verification
  models/models.go          All domain types + API request/response types
  store/
    postgres.go             PostgreSQL operations (rooms, tracks, queue, chat)
    redis.go                Redis operations (sessions, playback, listeners)
  handlers/
    rooms.go                Room CRUD, go-live, end-session
    queue.go                Track submissions, queue retrieval
    session.go              Get/update anonymous session
    websocket.go            WebSocket upgrade handler
  ws/
    messages.go             WebSocket event/action type constants + payloads
    client.go               Individual WS connection (read/write pumps)
    hub.go                  Per-room broadcast hub + inbound message handler
  playback/
    sync.go                 Auto-advance service (timers per room)
migrations/
  001_initial.up.sql        Database schema
  001_initial.down.sql      Teardown
```

## API Reference

### Session

| Method | Path           | Description                    |
|--------|----------------|--------------------------------|
| GET    | /api/session   | Get current anonymous session  |
| PATCH  | /api/session   | Update display name            |

### Rooms

| Method | Path                       | Auth     | Description              |
|--------|----------------------------|----------|--------------------------|
| GET    | /api/rooms                 | —        | List rooms (?live=true&genre=Jazz) |
| POST   | /api/rooms                 | —        | Create room (returns djKey) |
| GET    | /api/rooms/{slug}          | —        | Room detail + now playing, queue, chat |
| POST   | /api/rooms/{slug}/go-live  | DJ key   | Start streaming          |
| POST   | /api/rooms/{slug}/end      | DJ key   | End session              |

### Queue

| Method | Path                        | Auth     | Description               |
|--------|-----------------------------|----------|---------------------------|
| GET    | /api/rooms/{slug}/queue     | —        | Current approved queue     |
| POST   | /api/rooms/{slug}/queue     | —        | Submit a track             |
| GET    | /api/rooms/{slug}/requests  | DJ key   | Pending requests (DJ only) |

### WebSocket

Connect to `ws://localhost:8080/ws/room/{slug}?djKey=optional`

**Server → Client events:**
- `playback_state` — current track position + playing/paused
- `track_changed` — new track started
- `queue_update` — queue contents changed
- `chat_message` — new chat message
- `listener_count` — updated listener count
- `room_settings` — request policy changed
- `request_update` — (DJ only) new pending request
- `announcement` — DJ announcement
- `error` — error message

**Client → Server actions:**
```json
{"action": "send_chat", "payload": {"message": "Great track!"}}
{"action": "submit_track", "payload": {"title":"...", "artist":"...", "duration":180, "source":"youtube", "sourceUrl":"..."}}
{"action": "dj_skip", "payload": {}}
{"action": "dj_pause", "payload": {}}
{"action": "dj_resume", "payload": {}}
{"action": "dj_approve", "payload": {"entryId": "..."}}
{"action": "dj_reject", "payload": {"entryId": "..."}}
{"action": "dj_set_policy", "payload": {"policy": "open|approval|closed"}}
{"action": "dj_announce", "payload": {"message": "Welcome everyone!"}}
```

## Playback Sync

The server is the authority on what's playing and when. Each room's playback state is stored in Redis:

```json
{
  "roomId": "abc123",
  "trackId": "track456",
  "startedAt": 1708900000000,
  "isPlaying": true,
  "pausePosition": 0
}
```

Clients calculate their position: `currentPosition = (now - startedAt) / 1000` seconds. When a track's duration elapses, the `SyncService` automatically pops the next track from the queue and broadcasts the update.

## Frontend Integration

To connect your Next.js frontend:

1. **Replace mock data imports** with API calls to `/api/rooms`, `/api/rooms/{slug}`, etc.
2. **Add WebSocket connection** in `room/[slug]/page.tsx` — connect to `/ws/room/{slug}` on mount, handle incoming events to update local state.
3. **Update contexts** — `PlayerContext` should sync from WebSocket `playback_state` events instead of local state. `PlaylistContext` stays client-side for now.
4. **Session cookie** is set automatically by the middleware on first API call.
5. **DJ controls** — pass `djKey` via query param or header. The WebSocket hub validates it and grants elevated permissions.

## Next Steps

- [ ] Wire frontend API calls to replace mock data
- [ ] Add WebSocket client hook in Next.js
- [ ] Cover art file upload (S3 or local disk)
- [ ] Rate limiting on chat + track submissions
- [ ] JWT/OAuth user auth layer
- [ ] Persistent playlists + favorites (currently client-side)
- [ ] Redis pub/sub for multi-instance scaling
- [ ] Prometheus metrics
