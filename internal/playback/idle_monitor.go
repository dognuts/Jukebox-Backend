package playback

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

const (
	idleCheckInterval = 60 * time.Second // how often to check
	idleTimeout       = 15 * time.Minute  // how long before auto-close
)

// IdleMonitor checks for live rooms with no playback and no queued tracks.
// If a room stays idle for idleTimeout, it automatically ends the room.
type IdleMonitor struct {
	pg    *store.PGStore
	redis *store.RedisStore
	hubs  *ws.HubManager

	// roomID -> when we first noticed it was idle
	idleSince map[string]time.Time
	mu        sync.Mutex
	stopCh    chan struct{}
}

func NewIdleMonitor(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager) *IdleMonitor {
	return &IdleMonitor{
		pg:        pg,
		redis:     redis,
		hubs:      hubs,
		idleSince: make(map[string]time.Time),
		stopCh:    make(chan struct{}),
	}
}

// Start runs the idle check loop in a background goroutine.
func (m *IdleMonitor) Start() {
	go m.loop()
	log.Println("✓ Idle room monitor started (timeout: 15m)")
}

func (m *IdleMonitor) Stop() {
	close(m.stopCh)
}

func (m *IdleMonitor) loop() {
	ticker := time.NewTicker(idleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.check()
		case <-m.stopCh:
			return
		}
	}
}

func (m *IdleMonitor) check() {
	ctx := context.Background()

	// Get all live rooms
	rooms, err := m.pg.ListRooms(ctx, true, "")
	if err != nil {
		log.Printf("[idle-monitor] list rooms: %v", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	activeRoomIDs := make(map[string]bool)

	for _, room := range rooms {
		if !room.IsLive {
			continue
		}
		activeRoomIDs[room.ID] = true

		// Check if room has playback state (track playing)
		ps, _ := m.redis.GetPlaybackState(ctx, room.ID)
		hasPlayback := ps != nil && ps.TrackID != ""

		// Check if room has queued tracks
		queue, _ := m.pg.GetQueue(ctx, room.ID)
		hasQueue := len(queue) > 0

		if hasPlayback || hasQueue {
			// Room is active — remove from idle tracking
			delete(m.idleSince, room.ID)
			continue
		}

		// Room is idle (no playback, no queue)
		since, tracked := m.idleSince[room.ID]
		if !tracked {
			// First time noticing this room is idle
			m.idleSince[room.ID] = now
			log.Printf("[idle-monitor] room %s (%s) is idle, starting 5m timer", room.ID, room.Name)
			continue
		}

		// Check if idle long enough
		if now.Sub(since) >= idleTimeout {
			log.Printf("[idle-monitor] room %s (%s) idle for >5m, auto-closing", room.ID, room.Name)
			roomCopy := room
			m.closeRoom(ctx, &roomCopy)
			delete(m.idleSince, room.ID)
		}
	}

	// Clean up tracking for rooms that are no longer live
	for id := range m.idleSince {
		if !activeRoomIDs[id] {
			delete(m.idleSince, id)
		}
	}
}

func (m *IdleMonitor) closeRoom(ctx context.Context, room *models.Room) {
	m.pg.EndRoom(ctx, room.ID)
	m.pg.ClearNowPlaying(ctx, room.ID)
	m.redis.ClearPlaybackState(ctx, room.ID)
	m.redis.ClearListeners(ctx, room.ID)

	// Broadcast to connected clients
	if hub := m.hubs.Get(room.ID); hub != nil {
		hub.BroadcastJSON(ws.WSMessage{
			Event:   "room_ended",
			Payload: map[string]string{"reason": "Room auto-closed after 15 minutes of inactivity"},
		})
	}
}
