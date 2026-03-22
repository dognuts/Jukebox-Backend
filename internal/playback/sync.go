package playback

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Wire up the autoplay end callback so listeners can trigger track advance
	hubs.OnAutoplayEnd = func(roomID string) {
		s.advanceTrack(roomID)
	}
	return s
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
	// Debounce: ignore if we advanced this room within the last 3 seconds
	s.mu.Lock()
	if last, ok := s.lastAdvance[roomID]; ok && time.Since(last) < 3*time.Second {
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
		hub.Broadcast <- marshalMsg(ws.WSMessage{Event: ws.EventTrackChanged, Payload: nil})
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

	hub.Broadcast <- marshalMsg(ws.WSMessage{Event: ws.EventTrackChanged, Payload: entry.Track})
	hub.Broadcast <- marshalMsg(ws.WSMessage{Event: ws.EventPlaybackState, Payload: ps})

	queue, _ := s.pg.GetQueue(ctx, roomID)
	hub.Broadcast <- marshalMsg(ws.WSMessage{Event: ws.EventQueueUpdate, Payload: queue})

	s.ScheduleAdvance(roomID, &entry.Track, ps.StartedAtUnix)

	log.Printf("[playback] room %s now playing: %s - %s", roomID, entry.Track.Artist, entry.Track.Title)
}

// advanceAutoplay pulls the next track from the room's live autoplay playlist.
func (s *SyncService) advanceAutoplay(ctx context.Context, roomID string, hub *ws.Hub) {
	autoTrack, idx, err := s.pg.GetNextAutoplayTrack(ctx, roomID)
	if err != nil || autoTrack == nil {
		log.Printf("[autoplay] room %s: no autoplay tracks available", roomID)
		s.redis.ClearPlaybackState(ctx, roomID)
		s.pg.ClearNowPlaying(ctx, roomID)
		hub.Broadcast <- marshalMsg(ws.WSMessage{Event: ws.EventTrackChanged, Payload: nil})
		return
	}

	// Create a Track from the autoplay track
	track := &models.Track{
		ID:            fmt.Sprintf("auto-%s-%d-%d", roomID[:8], idx, time.Now().UnixMilli()),
		Title:         autoTrack.Title,
		Artist:        autoTrack.Artist,
		Duration:      autoTrack.Duration,
		Source:        models.TrackSource(autoTrack.Source),
		SourceURL:     autoTrack.SourceURL,
		AlbumGradient: autoTrack.AlbumGradient,
		CreatedAt:     time.Now(),
	}

	// Insert into tracks table so GetNowPlaying JOIN works
	if err := s.pg.UpsertTrack(ctx, track); err != nil {
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

	hub.Broadcast <- marshalMsg(ws.WSMessage{Event: ws.EventTrackChanged, Payload: track})
	hub.Broadcast <- marshalMsg(ws.WSMessage{Event: ws.EventPlaybackState, Payload: ps})

	s.ScheduleAdvance(roomID, track, ps.StartedAtUnix)

	log.Printf("[autoplay] room %s now playing [%d]: %s - %s", roomID, idx, track.Artist, track.Title)
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

func marshalMsg(msg ws.WSMessage) []byte {
	data, _ := json.Marshal(msg)
	return data
}
