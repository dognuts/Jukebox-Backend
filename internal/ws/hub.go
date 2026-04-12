package ws

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
)

// Hub manages all WebSocket clients for a single room.
type Hub struct {
	RoomID     string
	RoomSlug   string
	Clients    map[*Client]bool
	Register   chan *Client
	Unregister chan *Client
	Inbound    chan *ClientMessage
	Broadcast  chan []byte
	mu         sync.RWMutex
	listenerListTimer *time.Timer

	pg    *store.PGStore
	redis *store.RedisStore

	// OnAutoplayEnd is called when a listener reports the autoplay track ended
	OnAutoplayEnd func(roomID string)
	// OnReportDuration is called when a client reports actual track duration
	OnReportDuration func(roomID string, trackID string, duration int)
	// OnShutdown is called when the hub stops (no clients, room offline)
	OnShutdown func(roomID string)
}

// NewHub creates a hub for the given room.
func NewHub(roomID, roomSlug string, pg *store.PGStore, redis *store.RedisStore) *Hub {
	return &Hub{
		RoomID:     roomID,
		RoomSlug:   roomSlug,
		Clients:    make(map[*Client]bool),
		Register:   make(chan *Client),
		Unregister: make(chan *Client),
		Inbound:    make(chan *ClientMessage, 256),
		Broadcast:  make(chan []byte, 256),
		pg:         pg,
		redis:      redis,
	}
}

// Run starts the hub's main event loop. Call in a goroutine.
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.Register:
			h.mu.Lock()
			h.Clients[client] = true
			h.mu.Unlock()

			// Update listener count
			ctx := context.Background()
			count, _ := h.redis.AddListener(ctx, h.RoomID, client.Session.ID)
			h.broadcastListenerCount(int(count))

			// Broadcast join activity
			h.broadcastJSON(WSMessage{Event: "listener_join", Payload: map[string]string{
				"username":    client.DisplayName(),
				"avatarColor": client.Session.AvatarColor,
			}})

			// Start listen event for authenticated users
			if client.UserID != "" {
				evtID := client.Session.ID + ":" + h.RoomID
				h.pg.EndListenEventsByUser(ctx, client.UserID, h.RoomID) // close any stale events
				h.pg.StartListenEvent(ctx, &models.ListenEvent{
					ID:        evtID,
					UserID:    client.UserID,
					RoomID:    h.RoomID,
					StartedAt: time.Now(),
				})
			}

			// Send current playback state to new client
			h.sendInitialState(client)

		case client := <-h.Unregister:
			h.mu.Lock()
			if _, ok := h.Clients[client]; ok {
				delete(h.Clients, client)
				close(client.Send)
			}
			clientCount := len(h.Clients)
			h.mu.Unlock()

			ctx := context.Background()
			count, _ := h.redis.RemoveListener(ctx, h.RoomID, client.Session.ID)
			h.broadcastListenerCount(int(count))

			// Broadcast leave activity
			h.broadcastJSON(WSMessage{Event: "listener_leave", Payload: map[string]string{
				"username":    client.DisplayName(),
				"avatarColor": client.Session.AvatarColor,
			}})

			// End listen event for authenticated users
			if client.UserID != "" {
				evtID := client.Session.ID + ":" + h.RoomID
				h.pg.EndListenEvent(ctx, evtID, 0)
			}

			// If the DJ disconnects from a room that was NEVER live, auto-delete it.
			// This prevents ghost rooms from piling up when DJs create rooms
			// but leave before going live.
			if client.IsDJ {
				wasEverLive, err := h.pg.WasRoomEverLive(ctx, h.RoomID)
				if err == nil && !wasEverLive {
					log.Printf("[ws] DJ left room %s before going live — auto-deleting", h.RoomSlug)

					// Notify any remaining listeners
					h.broadcastJSON(WSMessage{
						Event:   "room_ended",
						Payload: map[string]string{"reason": "The DJ left before going live"},
					})

					// Clean up
					h.pg.DeleteRoom(ctx, h.RoomID)
					h.redis.ClearPlaybackState(ctx, h.RoomID)
					h.redis.ClearListeners(ctx, h.RoomID)

					if h.OnShutdown != nil {
						h.OnShutdown(h.RoomID)
					}
					return // shut down this hub
				}
			}

			// If no clients remain and room is no longer live, clean up this hub
			if clientCount == 0 {
				room, _ := h.pg.GetRoomByID(ctx, h.RoomID)
				if room == nil || !room.IsLive {
					log.Printf("[ws] hub %s has no clients and room is offline, shutting down", h.RoomSlug)
					if h.OnShutdown != nil {
						h.OnShutdown(h.RoomID)
					}
					return // exits the Run() goroutine
				}
			}

		case msg := <-h.Inbound:
			h.handleInbound(msg)

		case message := <-h.Broadcast:
			// Collect stale clients under read lock, then remove under write lock
			var stale []*Client
			h.mu.RLock()
			for client := range h.Clients {
				select {
				case client.Send <- message:
				default:
					stale = append(stale, client)
				}
			}
			h.mu.RUnlock()

			// Remove stale clients under write lock
			if len(stale) > 0 {
				h.mu.Lock()
				for _, client := range stale {
					if _, ok := h.Clients[client]; ok {
						close(client.Send)
						delete(h.Clients, client)
					}
				}
				h.mu.Unlock()
			}
		}
	}
}

func (h *Hub) sendInitialState(client *Client) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ws] panic in sendInitialState: %v", r)
		}
	}()

	ctx := context.Background()

	// Playback state
	ps, _ := h.redis.GetPlaybackState(ctx, h.RoomID)
	if ps != nil {
		// Send current track info FIRST so the client can start loading the player
		// before the playback state tells it where to seek
		if ps.TrackID != "" {
			track, _ := h.pg.GetTrack(ctx, ps.TrackID)
			if track != nil {
				client.SendJSON(WSMessage{Event: EventTrackChanged, Payload: track})
			}
		}

		// Now send playback state — client already has the track loaded
		client.SendJSON(WSMessage{Event: EventPlaybackState, Payload: ps})
	}

	// Current queue
	queue, _ := h.pg.GetQueue(ctx, h.RoomID)
	client.SendJSON(WSMessage{Event: EventQueueUpdate, Payload: queue})

	// Recent chat
	chat, _ := h.pg.GetRecentChat(ctx, h.RoomID, 50)
	for _, msg := range chat {
		client.SendJSON(WSMessage{Event: EventChatMessage, Payload: msg})
	}

	// Room settings
	room, _ := h.pg.GetRoomByID(ctx, h.RoomID)
	if room != nil {
		client.SendJSON(WSMessage{Event: EventRoomSettings, Payload: map[string]interface{}{
			"requestPolicy": room.RequestPolicy,
		}})
	}

	// If DJ, send pending requests
	if client.IsDJ {
		pending, _ := h.pg.GetPendingRequests(ctx, h.RoomID)
		if len(pending) > 0 {
			client.SendJSON(WSMessage{Event: EventRequestUpdate, Payload: pending})
		}
	}

	// Send current listener list
	h.broadcastListenerList()

	// Send current neon tube state
	tube, _ := h.pg.GetNeonTube(ctx, h.RoomID)
	if tube != nil {
		client.SendJSON(WSMessage{Event: "tube_update", Payload: tube})
	}
}

func (h *Hub) handleInbound(cm *ClientMessage) {
	ctx := context.Background()
	client := cm.Client
	msg := cm.Message

	switch msg.Action {
	case ActionSendChat:
		var p ChatPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			client.sendError("invalid chat message")
			return
		}
		// Must have either a text message or a media URL
		if p.Message == "" && p.MediaURL == "" {
			client.sendError("invalid chat message")
			return
		}
		if len(p.Message) > 500 {
			client.sendError("message too long (max 500 chars)")
			return
		}

		// Validate media URL if present — only allow GIPHY domains
		var mediaURL, mediaType string
		if p.MediaURL != "" {
			parsed, err := url.Parse(p.MediaURL)
			if err != nil || (parsed.Scheme != "https") {
				client.sendError("media URL must be HTTPS")
				return
			}
			host := strings.ToLower(parsed.Hostname())
			allowed := false
			allowedHosts := []string{
				"media.giphy.com",
				"i.giphy.com",
				"media0.giphy.com",
				"media1.giphy.com",
				"media2.giphy.com",
				"media3.giphy.com",
				"media4.giphy.com",
			}
			for _, h := range allowedHosts {
				if host == h {
					allowed = true
					break
				}
			}
			if !allowed {
				client.sendError("media URL domain not allowed")
				return
			}
			mediaURL = p.MediaURL
			if p.MediaType == "gif" || p.MediaType == "image" {
				mediaType = p.MediaType
			} else {
				mediaType = "gif" // default to gif for GIPHY URLs
			}
		}

		// Rate limit: max 1 message per 500ms per client
		now := time.Now()
		if now.Sub(client.LastChat) < 500*time.Millisecond {
			client.sendError("slow down — you're sending messages too fast")
			return
		}
		client.LastChat = now

		chatMsg := &models.ChatMessage{
			ID:          uuid.New().String(),
			RoomID:      h.RoomID,
			SessionID:   client.Session.ID,
			Username:    client.DisplayName(),
			AvatarColor: client.Session.AvatarColor,
			Message:     p.Message,
			Type:        models.ChatTypeMessage,
			Timestamp:   time.Now(),
			MediaURL:    mediaURL,
			MediaType:   mediaType,
		}

		if err := h.pg.InsertChatMessage(ctx, chatMsg); err != nil {
			log.Printf("insert chat: %v", err)
		}
		h.broadcastJSON(WSMessage{Event: EventChatMessage, Payload: chatMsg})

	case ActionReaction:
		var p ReactionPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Emoji == "" {
			return // silently ignore invalid reactions
		}
		// Broadcast to all clients in the room (including sender)
		h.broadcastJSON(WSMessage{Event: EventReaction, Payload: map[string]string{
			"emoji":    p.Emoji,
			"username": client.DisplayName(),
		}})

	case ActionSubmitTrack:
		var p SubmitTrackPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			client.sendError("invalid track submission")
			return
		}

		// Get room to check policy
		room, _ := h.pg.GetRoomByID(ctx, h.RoomID)
		if room == nil {
			client.sendError("room not found")
			return
		}
		if room.RequestPolicy == models.RequestPolicyClosed && !client.IsDJ {
			client.sendError("requests are closed for this room")
			return
		}

		// Create/upsert the track
		track := &models.Track{
			ID:        uuid.New().String(),
			Title:     p.Title,
			Artist:    p.Artist,
			Duration:  p.Duration,
			Source:    models.TrackSource(p.Source),
			SourceURL: p.SourceURL,
			CreatedAt: time.Now(),
		}
		if err := h.pg.UpsertTrack(ctx, track); err != nil {
			log.Printf("upsert track: %v", err)
			client.sendError("failed to save track")
			return
		}

		// Determine status based on policy
		status := models.QueueApproved
		if room.RequestPolicy == models.RequestPolicyApproval && !client.IsDJ {
			status = models.QueuePending
		}

		entry := &models.QueueEntry{
			ID:          uuid.New().String(),
			RoomID:      h.RoomID,
			Track:       *track,
			SubmittedBy: client.DisplayName(),
			SessionID:   client.Session.ID,
			Status:      status,
			CreatedAt:   time.Now(),
		}
		if err := h.pg.AddToQueue(ctx, entry); err != nil {
			log.Printf("add to queue: %v", err)
			client.sendError("failed to add to queue")
			return
		}

		if status == models.QueueApproved {
			// Broadcast updated queue to everyone
			queue, _ := h.pg.GetQueue(ctx, h.RoomID)
			h.broadcastJSON(WSMessage{Event: EventQueueUpdate, Payload: queue})
		} else {
			// Notify DJ of pending request
			h.notifyDJs(WSMessage{Event: EventRequestUpdate, Payload: entry})
			// Confirm to submitter
			client.SendJSON(WSMessage{Event: EventAnnouncement, Payload: map[string]string{
				"message": "Your request has been submitted for approval.",
			}})
		}

	case ActionDJSkip:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		h.skipToNext(ctx)

	case ActionDJPause:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		ps, _ := h.redis.GetPlaybackState(ctx, h.RoomID)
		if ps != nil && ps.IsPlaying {
			elapsed := int((time.Now().UnixMilli() - ps.StartedAtUnix) / 1000)
			ps.IsPlaying = false
			ps.PausePosition = elapsed
			h.redis.SetPlaybackState(ctx, ps)
			h.broadcastJSON(WSMessage{Event: EventPlaybackState, Payload: ps})
		}

	case ActionDJResume:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		ps, _ := h.redis.GetPlaybackState(ctx, h.RoomID)
		if ps != nil && !ps.IsPlaying {
			// Reset startedAt to account for the paused duration
			ps.StartedAtUnix = time.Now().UnixMilli() - int64(ps.PausePosition*1000)
			ps.IsPlaying = true
			h.redis.SetPlaybackState(ctx, ps)
			h.broadcastJSON(WSMessage{Event: EventPlaybackState, Payload: ps})
		}

	case ActionDJApprove:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		var p ApproveRejectPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			client.sendError("invalid payload")
			return
		}
		if err := h.pg.UpdateQueueEntryStatus(ctx, p.EntryID, models.QueueApproved); err != nil {
			log.Printf("approve entry: %v", err)
			return
		}
		queue, _ := h.pg.GetQueue(ctx, h.RoomID)
		h.broadcastJSON(WSMessage{Event: EventQueueUpdate, Payload: queue})

	case ActionDJReject:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		var p ApproveRejectPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			client.sendError("invalid payload")
			return
		}
		h.pg.UpdateQueueEntryStatus(ctx, p.EntryID, models.QueueRejected)

	case ActionDJSetPolicy:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		var p SetPolicyPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			client.sendError("invalid payload")
			return
		}
		policy := models.RequestPolicy(p.Policy)
		if policy != models.RequestPolicyOpen && policy != models.RequestPolicyApproval && policy != models.RequestPolicyClosed {
			client.sendError("invalid policy")
			return
		}
		h.pg.UpdateRoomPolicy(ctx, h.RoomID, policy)
		h.broadcastJSON(WSMessage{Event: EventRoomSettings, Payload: map[string]interface{}{
			"requestPolicy": policy,
		}})

	case ActionDJAnnounce:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		var p AnnouncePayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Message == "" {
			client.sendError("invalid announcement")
			return
		}
		chatMsg := &models.ChatMessage{
			ID:          uuid.New().String(),
			RoomID:      h.RoomID,
			SessionID:   client.Session.ID,
			Username:    client.DisplayName(),
			AvatarColor: client.Session.AvatarColor,
			Message:     p.Message,
			Type:        models.ChatTypeAnnouncement,
			Timestamp:   time.Now(),
		}
		h.pg.InsertChatMessage(ctx, chatMsg)
		h.broadcastJSON(WSMessage{Event: EventChatMessage, Payload: chatMsg})

	case ActionDJGoLive:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		// Pop first track from queue and start playing
		entry, err := h.pg.PopNextTrack(ctx, h.RoomID)
		if err != nil || entry == nil {
			client.sendError("no tracks in queue")
			return
		}

		// Set now playing
		h.pg.SetNowPlaying(ctx, h.RoomID, entry.Track.ID)

		// Mark room as live
		h.pg.SetRoomLive(ctx, h.RoomID, true)

		// Set playback state
		ps := &models.PlaybackState{
			RoomID:        h.RoomID,
			TrackID:       entry.Track.ID,
			StartedAtUnix: time.Now().UnixMilli(),
			IsPlaying:     true,
		}
		h.redis.SetPlaybackState(ctx, ps)

		// Broadcast to all clients
		h.broadcastJSON(WSMessage{Event: EventTrackChanged, Payload: entry.Track})
		h.broadcastJSON(WSMessage{Event: EventPlaybackState, Payload: ps})

		queue, _ := h.pg.GetQueue(ctx, h.RoomID)
		h.broadcastJSON(WSMessage{Event: EventQueueUpdate, Payload: queue})

		// Announce — use the room's stored DJ name
		room, _ := h.pg.GetRoomByID(ctx, h.RoomID)
		djName := client.DisplayName()
		if room != nil && room.DJDisplayName != "" {
			djName = room.DJDisplayName
		}
		goLiveMsg := &models.ChatMessage{
			ID:          uuid.New().String(),
			RoomID:      h.RoomID,
			SessionID:   client.Session.ID,
			Username:    "System",
			AvatarColor: "oklch(0.82 0.18 80)",
			Message:     djName + " is now live!",
			Type:        models.ChatTypeAnnouncement,
			Timestamp:   time.Now(),
		}
		h.pg.InsertChatMessage(ctx, goLiveMsg)
		h.broadcastJSON(WSMessage{Event: EventChatMessage, Payload: goLiveMsg})

	case ActionDJEndRoom:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}

		// End the room
		h.pg.EndRoom(ctx, h.RoomID)
		h.pg.ClearNowPlaying(ctx, h.RoomID)
		h.redis.ClearPlaybackState(ctx, h.RoomID)
		h.redis.ClearListeners(ctx, h.RoomID)

		// Announce in chat — use room's stored DJ name
		endRoom, _ := h.pg.GetRoomByID(ctx, h.RoomID)
		endDjName := client.DisplayName()
		if endRoom != nil && endRoom.DJDisplayName != "" {
			endDjName = endRoom.DJDisplayName
		}
		endMsg := &models.ChatMessage{
			ID:          uuid.New().String(),
			RoomID:      h.RoomID,
			SessionID:   client.Session.ID,
			Username:    "System",
			AvatarColor: "oklch(0.82 0.18 80)",
			Message:     endDjName + " has ended the session. Thanks for listening!",
			Type:        models.ChatTypeAnnouncement,
			Timestamp:   time.Now(),
		}
		h.pg.InsertChatMessage(ctx, endMsg)
		h.broadcastJSON(WSMessage{Event: EventChatMessage, Payload: endMsg})

		// Broadcast room ended
		h.broadcastJSON(WSMessage{
			Event:   "room_ended",
			Payload: map[string]string{"reason": "DJ ended the session"},
		})

	case ActionDJMic:
		if !client.IsDJ {
			client.sendError("DJ key required")
			return
		}
		var micPayload struct {
			Active     bool `json:"active"`
			PauseMusic bool `json:"pauseMusic"`
		}
		if err := json.Unmarshal(msg.Payload, &micPayload); err != nil {
			client.sendError("invalid mic payload")
			return
		}
		h.broadcastJSON(WSMessage{
			Event: EventDJMicState,
			Payload: map[string]interface{}{
				"active":     micPayload.Active,
				"pauseMusic": micPayload.PauseMusic,
				"djName":     client.DisplayName(),
			},
		})

	case ActionReportDuration:
		var durPayload struct {
			TrackID  string `json:"trackId"`
			Duration int    `json:"duration"`
		}
		if err := json.Unmarshal(msg.Payload, &durPayload); err == nil && durPayload.Duration > 0 {
			if h.OnReportDuration != nil {
				go h.OnReportDuration(h.RoomID, durPayload.TrackID, durPayload.Duration)
			}
		}

	case ActionAutoplayEnd:
		// Any listener can report that the autoplay track ended.
		// The advanceTrack debounce prevents double-advances, but we avoid
		// spawning a goroutine for every listener's report in a burst.
		if h.OnAutoplayEnd != nil {
			go h.OnAutoplayEnd(h.RoomID)
		}

	default:
		client.sendError("unknown action: " + msg.Action)
	}
}

// skipToNext pops the next track from the queue and updates playback state.
func (h *Hub) skipToNext(ctx context.Context) {
	entry, err := h.pg.PopNextTrack(ctx, h.RoomID)
	if err != nil {
		log.Printf("skip next: %v", err)
		return
	}

	if entry == nil {
		// Queue empty - clear playback
		h.redis.ClearPlaybackState(ctx, h.RoomID)
		h.pg.ClearNowPlaying(ctx, h.RoomID)
		h.broadcastJSON(WSMessage{Event: EventTrackChanged, Payload: nil})
		return
	}

	// Update now playing
	h.pg.SetNowPlaying(ctx, h.RoomID, entry.Track.ID)

	// Update playback state in Redis
	ps := &models.PlaybackState{
		RoomID:        h.RoomID,
		TrackID:       entry.Track.ID,
		StartedAtUnix: time.Now().UnixMilli(),
		IsPlaying:     true,
		PausePosition: 0,
	}
	h.redis.SetPlaybackState(ctx, ps)

	// Broadcast track change and updated queue
	h.broadcastJSON(WSMessage{Event: EventTrackChanged, Payload: entry.Track})
	h.broadcastJSON(WSMessage{Event: EventPlaybackState, Payload: ps})

	queue, _ := h.pg.GetQueue(ctx, h.RoomID)
	h.broadcastJSON(WSMessage{Event: EventQueueUpdate, Payload: queue})
}

func (h *Hub) broadcastJSON(msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.Broadcast <- data
}

// BroadcastJSON is the exported version for use by HTTP handlers.
func (h *Hub) BroadcastJSON(msg WSMessage) {
	h.broadcastJSON(msg)
}

func (h *Hub) broadcastListenerCount(count int) {
	h.broadcastJSON(WSMessage{
		Event:   EventListenerCount,
		Payload: map[string]int{"count": count},
	})
	// Throttle listener list broadcasts — schedule one 500ms from now,
	// cancelling any pending one. This collapses burst join/leave events
	// into a single list broadcast.
	h.mu.Lock()
	if h.listenerListTimer != nil {
		h.listenerListTimer.Stop()
	}
	h.listenerListTimer = time.AfterFunc(500*time.Millisecond, func() {
		h.broadcastListenerList()
	})
	h.mu.Unlock()
}

func (h *Hub) broadcastListenerList() {
	h.mu.RLock()
	type listenerInfo struct {
		Username    string `json:"username"`
		AvatarColor string `json:"avatarColor"`
		IsDJ        bool   `json:"isDJ"`
		UserID      string `json:"userId,omitempty"`
	}
	var listeners []listenerInfo
	seen := map[string]bool{}
	for client := range h.Clients {
		name := client.DisplayName()
		if seen[name] {
			continue
		}
		seen[name] = true
		listeners = append(listeners, listenerInfo{
			Username:    name,
			AvatarColor: client.Session.AvatarColor,
			IsDJ:        client.IsDJ,
			UserID:      client.UserID,
		})
	}
	h.mu.RUnlock()
	h.broadcastJSON(WSMessage{Event: EventListenerList, Payload: listeners})
}

func (h *Hub) notifyDJs(msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for client := range h.Clients {
		if client.IsDJ {
			select {
			case client.Send <- data:
			default:
			}
		}
	}
}

// ==================== Hub Manager ====================

// HubManager keeps track of all active room hubs.
type HubManager struct {
	hubs  map[string]*Hub // roomID -> Hub
	mu    sync.RWMutex
	pg    *store.PGStore
	redis *store.RedisStore

	// OnAutoplayEnd is set by the SyncService to handle autoplay track endings
	OnAutoplayEnd func(roomID string)
	// OnReportDuration is set by the SyncService to handle duration reports
	OnReportDuration func(roomID string, trackID string, duration int)
}

func NewHubManager(pg *store.PGStore, redis *store.RedisStore) *HubManager {
	return &HubManager{
		hubs:  make(map[string]*Hub),
		pg:    pg,
		redis: redis,
	}
}

// GetOrCreate returns the hub for a room, creating it if it doesn't exist.
func (m *HubManager) GetOrCreate(roomID, roomSlug string) *Hub {
	m.mu.Lock()
	defer m.mu.Unlock()

	if hub, ok := m.hubs[roomID]; ok {
		return hub
	}

	hub := NewHub(roomID, roomSlug, m.pg, m.redis)
	hub.OnAutoplayEnd = m.OnAutoplayEnd
	hub.OnReportDuration = m.OnReportDuration
	hub.OnShutdown = func(roomID string) {
		m.Remove(roomID)
	}
	m.hubs[roomID] = hub
	go hub.Run()
	return hub
}

// Get returns the hub for a room if it exists.
func (m *HubManager) Get(roomID string) *Hub {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hubs[roomID]
}

// Remove stops tracking a room hub (called when room goes offline with no listeners).
func (m *HubManager) Remove(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.hubs, roomID)
}

// SetDJKey validates the DJ key for a client joining a room.
func SetDJKey(client *Client, djKey string, djKeyHash string) {
	if djKey != "" && middleware.VerifyDJKey(djKey, djKeyHash) {
		client.IsDJ = true
	}
}
