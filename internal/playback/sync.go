package playback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sync"
	"time"

	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

// SyncService monitors playback state and auto-advances tracks when they finish.
type SyncService struct {
	pg          *store.PGStore
	redis       *store.RedisStore
	hubs        *ws.HubManager
	timers      map[string]*time.Timer // roomID -> timer
	lastAdvance map[string]time.Time   // roomID -> last advance time (debounce)
	mu          sync.Mutex
}

func NewSyncService(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager) *SyncService {
	s := &SyncService{
		pg:          pg,
		redis:       redis,
		hubs:        hubs,
		timers:      make(map[string]*time.Timer),
		lastAdvance: make(map[string]time.Time),
	}
	// Wire up the autoplay end callback so listeners can trigger track
	// advance — via the verified path, never advanceTrack directly: the
	// report comes from an untrusted anonymous client.
	hubs.OnAutoplayEnd = func(roomID string) {
		s.handleClientEndReport(roomID)
	}
	// Wire up duration reporting so autoplay timers use real durations
	hubs.OnReportDuration = func(roomID string, trackID string, duration int) {
		s.handleDurationReport(roomID, trackID, duration)
	}
	return s
}

// handleClientEndReport processes an autoplay_track_ended frame from a
// listener. Untrusted input: any anonymous socket can send it, so before
// the old code advanced on every report (10s debounce only), one client
// could skip every track in any room. Verify against server-side playback
// state before advancing.
func (s *SyncService) handleClientEndReport(roomID string) {
	ctx := context.Background()

	// Only autoplay rooms take listener end-reports at all — DJ rooms
	// advance via the DJ-gated dj_skip action.
	room, _ := s.pg.GetRoomByID(ctx, roomID)
	if room == nil || !room.IsAutoplay {
		return
	}

	ps, _ := s.redis.GetPlaybackState(ctx, roomID)
	if ps == nil || !ps.IsPlaying || ps.TrackID == "" {
		return
	}

	elapsed := time.Since(time.UnixMilli(ps.StartedAtUnix))

	track, _ := s.pg.GetTrack(ctx, ps.TrackID)
	if track != nil && track.Duration > 0 {
		// Known duration: accept the report only near the real end (small
		// slack for player-side buffering/timing skew).
		if elapsed < time.Duration(track.Duration)*time.Second-5*time.Second {
			return
		}
	} else {
		// Unknown duration (fresh autoplay track before any duration
		// report lands): the report is the only end signal we have, but
		// require a minimum play time so it can't be used to strobe-skip.
		if elapsed < 60*time.Second {
			return
		}
	}

	s.advanceTrack(roomID)
}

// ScheduleAdvance sets a timer to advance to the next track when the current one ends.
func (s *SyncService) ScheduleAdvance(roomID string, track *models.Track, startedAtUnix int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel existing timer for this room
	if t, ok := s.timers[roomID]; ok {
		t.Stop()
		delete(s.timers, roomID)
	}

	if track == nil {
		return
	}

	// If duration is 0 (unknown, e.g. YouTube embeds), use a fallback for autoplay rooms.
	// For DJ rooms, the client will fire onTrackEnd.
	if track.Duration <= 0 {
		// Check if this is an autoplay room — use 10 minute fallback
		room, _ := s.pg.GetRoomByID(context.Background(), roomID)
		if room != nil && room.IsAutoplay {
			track.Duration = 600 // 10 minute fallback
			log.Printf("[playback] room %s: autoplay track has no duration, using 600s fallback", roomID)
		} else {
			log.Printf("[playback] room %s: track has unknown duration, skipping auto-advance", roomID)
			return
		}
	}

	// Calculate when the track ends
	elapsed := time.Since(time.UnixMilli(startedAtUnix))
	remaining := time.Duration(track.Duration)*time.Second - elapsed
	if remaining <= 0 {
		go s.advanceTrack(roomID)
		return
	}

	remaining += 500 * time.Millisecond

	timer := time.AfterFunc(remaining, func() {
		s.advanceTrack(roomID)
	})
	s.timers[roomID] = timer

	log.Printf("[playback] scheduled advance for room %s in %v", roomID, remaining)
}

// CancelAdvance stops any pending auto-advance for a room.
func (s *SyncService) CancelAdvance(roomID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.timers[roomID]; ok {
		t.Stop()
		delete(s.timers, roomID)
	}
}

func (s *SyncService) advanceTrack(roomID string) {
	// Debounce: ignore if we advanced this room within the last 10 seconds
	s.mu.Lock()
	if last, ok := s.lastAdvance[roomID]; ok && time.Since(last) < 10*time.Second {
		log.Printf("[playback] room %s: debounce blocked advance (last advance %v ago)", roomID, time.Since(last))
		s.mu.Unlock()
		return
	}
	s.lastAdvance[roomID] = time.Now()
	// Cancel any pending timer for this room since we're advancing now
	if t, ok := s.timers[roomID]; ok {
		t.Stop()
		delete(s.timers, roomID)
	}
	s.mu.Unlock()

	ctx := context.Background()

	hub := s.hubs.Get(roomID)
	if hub == nil {
		return
	}

	entry, err := s.pg.PopNextTrack(ctx, roomID)
	if err != nil {
		log.Printf("[playback] pop next track: %v", err)
		return
	}

	// If queue is empty, check if this is an autoplay room
	if entry == nil {
		room, _ := s.pg.GetRoomByID(ctx, roomID)
		if room != nil && room.IsAutoplay {
			s.advanceAutoplay(ctx, roomID, hub)
			return
		}

		s.redis.ClearPlaybackState(ctx, roomID)
		s.pg.ClearNowPlaying(ctx, roomID)
		hub.BroadcastJSON(ws.WSMessage{Event: ws.EventTrackChanged, Payload: nil})
		log.Printf("[playback] room %s queue empty", roomID)
		return
	}

	s.pg.SetNowPlaying(ctx, roomID, entry.Track.ID)

	ps := &models.PlaybackState{
		RoomID:        roomID,
		TrackID:       entry.Track.ID,
		StartedAtUnix: time.Now().UnixMilli(),
		IsPlaying:     true,
		PausePosition: 0,
	}
	s.redis.SetPlaybackState(ctx, ps)

	hub.BroadcastJSON(ws.WSMessage{Event: ws.EventTrackChanged, Payload: entry.Track})
	hub.BroadcastJSON(ws.WSMessage{Event: ws.EventPlaybackState, Payload: ps})

	queue, _ := s.pg.GetQueue(ctx, roomID)
	hub.BroadcastJSON(ws.WSMessage{Event: ws.EventQueueUpdate, Payload: queue})

	s.ScheduleAdvance(roomID, &entry.Track, ps.StartedAtUnix)

	log.Printf("[playback] room %s now playing: %s - %s", roomID, entry.Track.Artist, entry.Track.Title)
}

// autoplayTrackID derives the stable synthetic track ID for an autoplay
// room + source URL. Earlier versions embedded a timestamp
// ("auto-<room>-<idx>-<unixmilli>"), so every advance inserted a brand-new
// tracks row — ~360 rows/day for a 24/7 room — that nothing ever deleted.
// A deterministic ID makes the upsert actually dedupe: one row per
// (room, source URL) no matter how many times the playlist loops. The room
// is part of the key so per-room info snippets never bleed across rooms
// playing the same URL.
//
// Legacy timestamped auto-* rows from before this change are left in place —
// no automated destructive cleanup is shipped. They can be removed manually
// once nothing references them, e.g.:
//
//	DELETE FROM tracks t
//	WHERE t.id ~ '^auto-.*-[0-9]+-[0-9]+$'
//	  AND NOT EXISTS (SELECT 1 FROM now_playing np WHERE np.track_id = t.id)
//	  AND NOT EXISTS (SELECT 1 FROM queue_entries q WHERE q.track_id = t.id)
//	  AND NOT EXISTS (SELECT 1 FROM playlist_tracks pt WHERE pt.track_id = t.id)
func autoplayTrackID(roomID, sourceURL string) string {
	sum := sha256.Sum256([]byte(roomID + "\x00" + sourceURL))
	return "auto-" + hex.EncodeToString(sum[:8])
}

// advanceAutoplay pulls the next track from the room's live autoplay playlist.
func (s *SyncService) advanceAutoplay(ctx context.Context, roomID string, hub *ws.Hub) {
	autoTrack, idx, err := s.pg.GetNextAutoplayTrack(ctx, roomID)
	if err != nil || autoTrack == nil {
		log.Printf("[autoplay] room %s: no autoplay tracks available", roomID)
		s.redis.ClearPlaybackState(ctx, roomID)
		s.pg.ClearNowPlaying(ctx, roomID)
		hub.BroadcastJSON(ws.WSMessage{Event: ws.EventTrackChanged, Payload: nil})
		return
	}

	// Create a Track from the autoplay track
	track := &models.Track{
		ID:            autoplayTrackID(roomID, autoTrack.SourceURL),
		Title:         autoTrack.Title,
		Artist:        autoTrack.Artist,
		Duration:      autoTrack.Duration,
		Source:        models.TrackSource(autoTrack.Source),
		SourceURL:     autoTrack.SourceURL,
		AlbumGradient: autoTrack.AlbumGradient,
		InfoSnippet:   autoTrack.InfoSnippet,
		CreatedAt:     time.Now(),
	}

	// Find-or-refresh the synthetic tracks row so the GetNowPlaying JOIN
	// resolves. The stable ID means replays reuse the same row (with metadata
	// refreshed) instead of growing the tracks table on every advance. On
	// success the upsert also writes the row's effective duration back into
	// track.Duration: when the playlist says 0 but a listener already
	// reported the real length (UpdateTrackDuration), the broadcast below and
	// the advance timer use the learned value instead of the 600s fallback.
	if err := s.pg.UpsertAutoplayTrack(ctx, track); err != nil {
		log.Printf("[autoplay] room %s: failed to upsert track: %v", roomID, err)
	}

	s.pg.SetNowPlaying(ctx, roomID, track.ID)

	ps := &models.PlaybackState{
		RoomID:        roomID,
		TrackID:       track.ID,
		StartedAtUnix: time.Now().UnixMilli(),
		IsPlaying:     true,
		PausePosition: 0,
	}
	s.redis.SetPlaybackState(ctx, ps)

	hub.BroadcastJSON(ws.WSMessage{Event: ws.EventTrackChanged, Payload: track})
	hub.BroadcastJSON(ws.WSMessage{Event: ws.EventPlaybackState, Payload: ps})

	s.ScheduleAdvance(roomID, track, ps.StartedAtUnix)

	log.Printf("[autoplay] room %s now playing [%d]: %s - %s", roomID, idx, track.Artist, track.Title)
}

// handleDurationReport updates the track duration and reschedules the
// advance timer. Untrusted input from any listener, so it is constrained:
// sane bounds, only for the track CURRENTLY playing in that room (no
// cross-room/arbitrary-track poisoning), only in autoplay rooms (the only
// place the server needs a client-reported length), and learn-once — a
// track that already has a duration keeps it.
func (s *SyncService) handleDurationReport(roomID, trackID string, duration int) {
	// Floor of 60s: learn-once means the FIRST reporter wins, so a low
	// floor would let an attacker pre-poison each fresh track with a tiny
	// duration and strobe-skip the room within the rules. 60s matches the
	// unknown-duration minimum in handleClientEndReport, capping the
	// worst-case skip rate either way.
	if duration < 60 || duration > models.MaxTrackDurationSecs {
		return
	}
	ctx := context.Background()

	room, _ := s.pg.GetRoomByID(ctx, roomID)
	if room == nil || !room.IsAutoplay {
		return
	}

	// Only the currently playing track accepts reports.
	ps, _ := s.redis.GetPlaybackState(ctx, roomID)
	if ps == nil || ps.TrackID != trackID || !ps.IsPlaying {
		return
	}

	track, _ := s.pg.GetTrack(ctx, trackID)
	if track == nil || track.Duration > 0 {
		return // unknown track, or duration already learned
	}

	// Update the track in the DB. Only the report that actually won the
	// learn-once write reschedules the timer — a raced loser acting on
	// its own value would override the winner's schedule.
	won, err := s.pg.UpdateTrackDuration(ctx, trackID, duration)
	if err != nil {
		log.Printf("[playback] failed to update track duration: %v", err)
		return
	}
	if !won {
		return
	}

	// Reschedule the advance timer with the real duration
	track.Duration = duration
	s.ScheduleAdvance(roomID, track, ps.StartedAtUnix)
	log.Printf("[playback] room %s: updated track duration to %ds, rescheduled advance", roomID, duration)
}

func (s *SyncService) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.timers {
		t.Stop()
	}
	s.timers = make(map[string]*time.Timer)
}

// StartAutoplayRooms boots all autoplay rooms on server start.
func (s *SyncService) StartAutoplayRooms(ctx context.Context) {
	rooms, err := s.pg.GetAutoplayRooms(ctx)
	if err != nil {
		log.Printf("[autoplay] failed to load autoplay rooms: %v", err)
		return
	}

	for _, room := range rooms {
		// Ensure room is marked live
		s.pg.SetRoomAutoplay(ctx, room.ID, true)

		// Ensure hub exists
		hub := s.hubs.GetOrCreate(room.ID, room.Slug)
		if hub == nil {
			continue
		}

		// Check if already playing
		ps, _ := s.redis.GetPlaybackState(ctx, room.ID)
		if ps != nil && ps.IsPlaying && ps.TrackID != "" {
			// Already playing — just re-schedule advance
			track, _ := s.pg.GetTrack(ctx, ps.TrackID)
			if track != nil {
				s.ScheduleAdvance(room.ID, track, ps.StartedAtUnix)
				log.Printf("[autoplay] resumed room %s (%s), currently playing: %s", room.Slug, room.ID, track.Title)
				continue
			}
		}

		// Start fresh — advance to first track
		s.advanceAutoplay(ctx, room.ID, hub)
		log.Printf("[autoplay] started room %s (%s)", room.Slug, room.ID)
	}

	if len(rooms) > 0 {
		log.Printf("✓ Started %d autoplay room(s)", len(rooms))
	}
}
