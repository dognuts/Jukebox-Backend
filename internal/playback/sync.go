package playback

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

// SyncService monitors playback state and auto-advances tracks when they finish.
type SyncService struct {
	pg     *store.PGStore
	redis  *store.RedisStore
	hubs   *ws.HubManager
	timers map[string]*time.Timer // roomID -> timer
	mu     sync.Mutex
}

func NewSyncService(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager) *SyncService {
	return &SyncService{
		pg:     pg,
		redis:  redis,
		hubs:   hubs,
		timers: make(map[string]*time.Timer),
	}
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

	// If duration is 0 (unknown, e.g. YouTube embeds), don't schedule auto-advance.
	// The client's audio player will fire onTrackEnd when it actually finishes.
	if track.Duration <= 0 {
		log.Printf("[playback] room %s: track has unknown duration, skipping auto-advance", roomID)
		return
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

	if entry == nil {
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

func (s *SyncService) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.timers {
		t.Stop()
	}
	s.timers = make(map[string]*time.Timer)
}

func marshalMsg(msg ws.WSMessage) []byte {
	data, _ := json.Marshal(msg)
	return data
}
