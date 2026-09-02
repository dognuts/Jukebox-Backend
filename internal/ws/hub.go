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
	"github.com/gorilla/websocket"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/moderation"
	"github.com/jukebox/backend/internal/store"
)

// dataStore is the slice of *store.PGStore the hub uses, declared as an
// interface so tests can run the hub against an in-memory fake.
type dataStore interface {
	GetRoomByID(ctx context.Context, id string) (*models.Room, error)
	WasRoomEverLive(ctx context.Context, roomID string) (bool, error)
	DeleteRoom(ctx context.Context, roomID string) error
	SetRoomLive(ctx context.Context, roomID string, live bool) error
	EndRoom(ctx context.Context, roomID string) error
	UpdateRoomPolicy(ctx context.Context, roomID string, policy models.RequestPolicy) error

	GetTrack(ctx context.Context, id string) (*models.Track, error)
	UpsertTrack(ctx context.Context, t *models.Track) error

	GetQueue(ctx context.Context, roomID string) ([]models.QueueEntry, error)
	GetPendingRequests(ctx context.Context, roomID string) ([]models.QueueEntry, error)
	AddToQueue(ctx context.Context, entry *models.QueueEntry) error
	UpdateQueueEntryStatus(ctx context.Context, roomID, entryID string, status models.QueueEntryStatus) error
	CountActiveQueueEntriesBySession(ctx context.Context, roomID, sessionID string) (int, error)
	PopNextTrack(ctx context.Context, roomID string) (*models.QueueEntry, error)
	SetNowPlaying(ctx context.Context, roomID, trackID string) error
	ClearNowPlaying(ctx context.Context, roomID string) error

	InsertChatMessage(ctx context.Context, msg *models.ChatMessage) error
	GetRecentChat(ctx context.Context, roomID string, limit int) ([]models.ChatMessage, error)

	GetNeonTube(ctx context.Context, roomID string) (*models.NeonTube, error)

	StartListenEvent(ctx context.Context, evt *models.ListenEvent) error
	EndListenEvent(ctx context.Context, eventID string, tracksHeard int) error
	EndListenEventsByUser(ctx context.Context, userID, roomID string) error
}

// presenceStore is the slice of *store.RedisStore the hub uses.
type presenceStore interface {
	AddListener(ctx context.Context, roomID, sessionID string) (int64, error)
	RemoveListener(ctx context.Context, roomID, sessionID string) (int64, error)
	GetPlaybackState(ctx context.Context, roomID string) (*models.PlaybackState, error)
	SetPlaybackState(ctx context.Context, state *models.PlaybackState) error
	ClearPlaybackState(ctx context.Context, roomID string) error
	ClearListeners(ctx context.Context, roomID string) error
}

// Hub manages all WebSocket clients for a single room.
//
// Concurrency model: the Run loop owns only fast, in-memory work —
// mutating the client set and fanning broadcasts out to per-client
// buffers. Every blocking Postgres/Redis call runs on a side goroutine
// (onRegister / onUnregister per event, plus the inboundLoop and
// persistLoop workers), so one slow query can never freeze chat,
// reactions, joins, or broadcasts for the whole room — and the loop can
// never block on its own Broadcast channel (the classic self-deadlock:
// sole consumer waiting to produce into the channel it drains).
type Hub struct {
	RoomID            string
	RoomSlug          string
	Clients           map[*Client]bool
	Register          chan *Client
	Unregister        chan *Client
	Inbound           chan *ClientMessage
	Broadcast         chan []byte
	mu                sync.RWMutex
	listenerListTimer *time.Timer

	// quit is closed exactly once (via stop) when the hub shuts down.
	// Every producer selects on it so nothing can block forever on a
	// dead hub's channels.
	quit     chan struct{}
	stopOnce sync.Once

	// stopping and pendingRegs (both guarded by mu) make the idle-shutdown
	// decision atomic with registration. pendingRegs counts clients
	// accepted by RegisterClient but not yet added to Clients by the Run
	// loop; stopping is set in the same critical section as the "no
	// clients, none pending" check, and RegisterClient refuses once it is
	// set — so a fresh client can never register into the window between
	// that check and close(quit) and get torn down by a hub it just joined.
	// The Run loop re-checks stopping when it receives a registration, so
	// even a client already in flight when a stop path (DJ left, room
	// ended) commits is refused rather than added to a dying hub.
	stopping    bool
	pendingRegs int

	// countMu serializes each presence mutation (AddListener /
	// RemoveListener) with the enqueue of its listener_count broadcast.
	// The per-client onRegister/onUnregister goroutines run concurrently;
	// without this lock two joins could read counts 1 and 2 from Redis but
	// enqueue "2" before "1", leaving every client displaying the stale
	// count until the next churn. Broadcast fanout is FIFO, so making
	// (mutate, enqueue) atomic makes counts arrive in order.
	countMu sync.Mutex

	// micState mirrors the last dj_mic_state broadcast (guarded by mu) so
	// every joining client receives the current state during
	// sendInitialState instead of waiting for the next toggle. micOwner is
	// the connection that turned the mic on: only its departure clears the
	// state (a DJ's second tab closing must not kill a live voice session).
	micActive     bool
	micPauseMusic bool
	micDJName     string
	micOwner      *Client

	// persistCh feeds persistLoop, which inserts chat messages into
	// Postgres in FIFO order after they have been broadcast.
	persistCh chan *models.ChatMessage

	pg    dataStore
	redis presenceStore

	// OnAutoplayEnd is called when a listener reports the autoplay track ended
	OnAutoplayEnd func(roomID string)
	// OnReportDuration is called when a client reports actual track duration
	OnReportDuration func(roomID string, trackID string, duration int)
	// OnShutdown is called when the hub stops (no clients, room offline)
	OnShutdown func(roomID string)
}

// NewHub creates a hub for the given room.
func NewHub(roomID, roomSlug string, pg dataStore, redis presenceStore) *Hub {
	return &Hub{
		RoomID:     roomID,
		RoomSlug:   roomSlug,
		Clients:    make(map[*Client]bool),
		Register:   make(chan *Client),
		Unregister: make(chan *Client),
		Inbound:    make(chan *ClientMessage, 256),
		Broadcast:  make(chan []byte, 256),
		quit:       make(chan struct{}),
		persistCh:  make(chan *models.ChatMessage, 256),
		pg:         pg,
		redis:      redis,
	}
}

// Run starts the hub's main event loop. Call in a goroutine.
func (h *Hub) Run() {
	// Blocking I/O lives on these workers, never on this loop.
	go h.inboundLoop()
	go h.persistLoop()

	for {
		select {
		case client := <-h.Register:
			h.mu.Lock()
			if h.stopping {
				// The hub committed to stopping (DJ left, room ended)
				// after this client passed RegisterClient's check but
				// before the loop received it. Refuse deterministically:
				// never add it to Clients, release its ordering barrier
				// (nothing will run onRegister, so onUnregister must not
				// wait on it), and close it so its pumps exit and the
				// client reconnects onto a fresh hub.
				h.pendingRegs--
				h.mu.Unlock()
				close(client.registered)
				client.close()
				continue
			}
			h.Clients[client] = true
			h.pendingRegs--
			h.mu.Unlock()
			// All join-time I/O (listener accounting, listen events,
			// initial state replay) runs off the loop so a slow store
			// can't stall the room. The client is broadcast-visible from
			// this point, before its snapshot is replayed — see the
			// interleaving note on sendInitialState.
			go h.onRegister(client)

		case client := <-h.Unregister:
			h.mu.Lock()
			if _, ok := h.Clients[client]; ok {
				delete(h.Clients, client)
				client.close()
			}
			h.mu.Unlock()
			go h.onUnregister(client)

		case message := <-h.Broadcast:
			h.fanout(message)

		case <-h.quit:
			// Flush queued broadcasts (e.g. the final room_ended) so
			// they reach client buffers before the loop exits.
			for {
				select {
				case message := <-h.Broadcast:
					h.fanout(message)
				default:
					return
				}
			}
		}
	}
}

// inboundLoop serializes handling of client actions for the room on a
// goroutine separate from Run, so handler I/O (Postgres/Redis) never
// blocks broadcast fanout, joins, or leaves. Single consumer = inbound
// actions are still processed in arrival order.
func (h *Hub) inboundLoop() {
	for {
		select {
		case cm := <-h.Inbound:
			h.handleInbound(cm)
		case <-h.quit:
			return
		}
	}
}

// persistLoop inserts broadcast-first chat messages into Postgres in
// FIFO order, off every latency-sensitive path.
//
// Durability tradeoff: a chat message is shown to the room before it is
// stored, so if the insert fails (or the process dies first) the message
// is missing from the history replay. That's accepted — the alternative
// was capping the whole room's chat throughput at one DB insert per
// message and freezing chat whenever Postgres stalls.
func (h *Hub) persistLoop() {
	for {
		select {
		case msg := <-h.persistCh:
			h.insertChat(msg)
		case <-h.quit:
			// Drain anything already queued, then exit.
			for {
				select {
				case msg := <-h.persistCh:
					h.insertChat(msg)
				default:
					return
				}
			}
		}
	}
}

func (h *Hub) insertChat(msg *models.ChatMessage) {
	if err := h.pg.InsertChatMessage(context.Background(), msg); err != nil {
		log.Printf("insert chat: %v", err)
	}
}

// persistChat queues an already-broadcast chat message for insertion.
// If the persistence worker is persistCh-cap messages behind, the
// message is dropped from history (never from the live room) and logged.
func (h *Hub) persistChat(msg *models.ChatMessage) {
	select {
	case h.persistCh <- msg:
	default:
		log.Printf("[ws] room %s: chat persist queue full, message %s dropped from history", h.RoomSlug, msg.ID)
	}
}

// fanout delivers one marshaled broadcast to every client. The payload
// is wrapped in a single PreparedMessage so permessage-deflate
// compresses it once per broadcast rather than once per recipient.
// Sends never block: a client whose send buffer is full is evicted
// (disconnect policy — see sendBufferSize in client.go), so one slow
// reader can't stall the room.
func (h *Hub) fanout(message []byte) {
	out := outbound{data: message}
	if pm, err := websocket.NewPreparedMessage(websocket.TextMessage, message); err == nil {
		out.prepared = pm
	}

	// Collect stale clients under read lock, then remove under write lock.
	var stale []*Client
	h.mu.RLock()
	for client := range h.Clients {
		if !client.send(out) {
			stale = append(stale, client)
		}
	}
	h.mu.RUnlock()

	if len(stale) > 0 {
		h.mu.Lock()
		for _, client := range stale {
			if _, ok := h.Clients[client]; ok {
				client.close()
				delete(h.Clients, client)
			}
		}
		h.mu.Unlock()
	}
}

// onRegister runs all join-time I/O for a newly registered client off
// the hub loop, including the initial state replay.
func (h *Hub) onRegister(client *Client) {
	// Release this client's onUnregister once all join-time I/O is done
	// (or has panicked) — see Client.registered for the ordering contract.
	defer close(client.registered)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ws] panic in onRegister: %v", r)
		}
	}()

	ctx := context.Background()

	// Update listener count (mutation and broadcast are atomic — see countMu).
	h.updateListenerCount(func() (int64, error) {
		return h.redis.AddListener(ctx, h.RoomID, client.Session.ID)
	})

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
}

// onUnregister runs all leave-time I/O off the hub loop and decides
// whether the hub should shut down.
func (h *Hub) onUnregister(client *Client) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ws] panic in onUnregister: %v", r)
		}
	}()

	// Per-client ordering barrier: on a fast connect-then-disconnect this
	// goroutine could otherwise overtake onRegister, running RemoveListener
	// before AddListener (stranding a ghost session in the listeners set)
	// and EndListenEvent before StartListenEvent (a never-ended row).
	// onRegister always closes registered — even on panic — so this cannot
	// wait forever, and it waits off the hub loop, never blocking the room.
	<-client.registered

	ctx := context.Background()
	h.updateListenerCount(func() (int64, error) {
		return h.redis.RemoveListener(ctx, h.RoomID, client.Session.ID)
	})

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

	// The mic's owning connection is gone: clear mic state and tell
	// listeners, so their voice UI doesn't wait for a toggle that will
	// never come and a future joiner doesn't get a stale mic replay.
	// Scoped to the owner — a DJ's other tabs/devices leaving must not
	// kill a voice session that is still live.
	if client.IsDJ {
		h.mu.Lock()
		ownsMic := h.micOwner == client
		wasMicActive := h.micActive && ownsMic
		if ownsMic {
			h.micActive = false
			h.micPauseMusic = false
			h.micOwner = nil
		}
		h.mu.Unlock()
		if wasMicActive {
			h.broadcastJSON(WSMessage{
				Event: EventDJMicState,
				Payload: map[string]interface{}{
					"active":     false,
					"pauseMusic": false,
					"djName":     client.DisplayName(),
				},
			})
		}
	}

	// If the DJ disconnects from a room that was NEVER live, auto-delete it.
	// This prevents ghost rooms from piling up when DJs create rooms
	// but leave before going live.
	if client.IsDJ {
		wasEverLive, err := h.pg.WasRoomEverLive(ctx, h.RoomID)
		if err == nil && !wasEverLive {
			log.Printf("[ws] DJ left room %s before going live — auto-deleting", h.RoomSlug)

			// Notify any remaining listeners (flushed by Run before it exits)
			h.broadcastJSON(WSMessage{
				Event:   "room_ended",
				Payload: map[string]string{"reason": "The DJ left before going live"},
			})

			// Clean up
			h.pg.DeleteRoom(ctx, h.RoomID)
			h.redis.ClearPlaybackState(ctx, h.RoomID)
			h.redis.ClearListeners(ctx, h.RoomID)

			h.stop()
			return
		}
	}

	// If no clients remain and room is no longer live, clean up this hub
	if h.clientCount() == 0 {
		room, _ := h.pg.GetRoomByID(ctx, h.RoomID)
		if room == nil || !room.IsLive {
			// Re-check after the DB round-trip, atomically with
			// registration: counting pendingRegs and setting stopping in
			// one critical section means no client can be in flight when
			// we decide, and RegisterClient (which checks stopping under
			// this same lock) refuses everyone after — closing the TOCTOU
			// window between this check and close(quit).
			h.mu.Lock()
			idle := len(h.Clients) == 0 && h.pendingRegs == 0
			if idle {
				h.stopping = true
			}
			h.mu.Unlock()
			if idle {
				log.Printf("[ws] hub %s has no clients and room is offline, shutting down", h.RoomSlug)
				h.stop()
			}
		}
	}
}

func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.Clients)
}

// stop shuts the hub down exactly once: closing quit unblocks the Run
// loop (which flushes queued broadcasts first) and both workers, and
// OnShutdown removes the hub from its manager.
func (h *Hub) stop() {
	h.stopOnce.Do(func() {
		// Mark stopping first so RegisterClient refuses new clients even
		// on stop paths (DJ left, room ended) that didn't already set it.
		// A registration already in flight past RegisterClient's check is
		// covered too: the Run loop re-checks stopping on receive and
		// rejects it (see the Register case in Run).
		h.mu.Lock()
		h.stopping = true
		h.mu.Unlock()
		close(h.quit)
		if h.OnShutdown != nil {
			h.OnShutdown(h.RoomID)
		}
	})
}

// RegisterClient hands a new client to the hub loop. It returns false if
// the hub has shut down or committed to shutting down (a race with the
// room ending); the caller should drop the connection and let the client
// reconnect onto a fresh hub.
//
// If a stop path commits while the registration is already in flight, this
// can still return true — the Run loop then refuses the client (stopping
// re-check) and closes it, so its pumps exit immediately and the caller-side
// outcome is identical: dropped connection, clean reconnect.
func (h *Hub) RegisterClient(c *Client) bool {
	// Claim a pending-registration slot under mu: the idle-shutdown check
	// in onUnregister counts pendingRegs and sets stopping under the same
	// lock, so either this client is visible to that check (no shutdown)
	// or stopping is already set here (refuse; client reconnects fresh).
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return false
	}
	h.pendingRegs++
	h.mu.Unlock()

	select {
	case h.Register <- c:
		return true
	case <-h.quit:
		h.mu.Lock()
		h.pendingRegs--
		h.mu.Unlock()
		return false
	}
}

// unregister hands a client back to the hub loop; if the hub has already
// shut down it just marks the client closed so its WritePump exits.
func (h *Hub) unregister(c *Client) {
	select {
	case h.Unregister <- c:
	case <-h.quit:
		c.close()
	}
}

// sendInitialState replays the room snapshot to one newly joined client.
//
// Interleaving note: the client is added to the broadcast fanout (Clients
// set) by the Run loop before this snapshot is assembled off-loop, so a
// live broadcast can land before or between snapshot frames. Two visible
// consequences, both transient and self-correcting: (1) a snapshot
// queue_update read here may briefly overwrite a newer live queue_update
// the client already received — corrected by the next queue change; and
// (2) a chat message can arrive twice, once as the live broadcast and
// once in the GetRecentChat replay, if its async persist lands before the
// read — chat frames carry a stable message id, so clients should treat
// delivery as at-least-once and dedupe on id. The alternative — holding
// the client out of the fanout until the replay finishes — would silently
// drop every broadcast made during the replay, which is strictly worse.
func (h *Hub) sendInitialState(client *Client) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ws] panic in sendInitialState: %v", r)
		}
	}()

	// WS CONTRACT (frozen): initial_state carries serverTime, the unix
	// epoch in milliseconds at send time. Clients compute
	// clockOffset = serverTime - Date.now() on receipt and add it when
	// deriving playback position; clients tolerate the field being absent.
	client.sendValue(struct {
		Event      string `json:"event"`
		ServerTime int64  `json:"serverTime"`
	}{Event: EventInitialState, ServerTime: time.Now().UnixMilli()})

	ctx := context.Background()

	// Fan out all reads in parallel — previously this path did ~7
	// sequential round-trips to Redis and Postgres. With them fanned
	// out, the join latency is bounded by the slowest single call
	// instead of their sum.
	var (
		wg sync.WaitGroup

		ps      *models.PlaybackState
		track   *models.Track
		queue   []models.QueueEntry
		chat    []models.ChatMessage
		room    *models.Room
		pending []models.QueueEntry
		tube    *models.NeonTube
	)

	wg.Add(5)
	go func() {
		defer wg.Done()
		ps, _ = h.redis.GetPlaybackState(ctx, h.RoomID)
		if ps != nil && ps.TrackID != "" {
			track, _ = h.pg.GetTrack(ctx, ps.TrackID)
		}
	}()
	go func() {
		defer wg.Done()
		queue, _ = h.pg.GetQueue(ctx, h.RoomID)
	}()
	go func() {
		defer wg.Done()
		chat, _ = h.pg.GetRecentChat(ctx, h.RoomID, 50)
	}()
	go func() {
		defer wg.Done()
		room, _ = h.pg.GetRoomByID(ctx, h.RoomID)
	}()
	go func() {
		defer wg.Done()
		tube, _ = h.pg.GetNeonTube(ctx, h.RoomID)
	}()

	if client.IsDJ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pending, _ = h.pg.GetPendingRequests(ctx, h.RoomID)
		}()
	}

	wg.Wait()

	// Dispatch sends in the original order so the client receives them
	// in the expected sequence (track info before playback state, etc).
	if ps != nil {
		if track != nil {
			client.SendJSON(WSMessage{Event: EventTrackChanged, Payload: track})
		}
		client.SendJSON(WSMessage{Event: EventPlaybackState, Payload: ps})
	}

	client.SendJSON(WSMessage{Event: EventQueueUpdate, Payload: queue})

	for _, msg := range chat {
		client.SendJSON(WSMessage{Event: EventChatMessage, Payload: msg})
	}

	if room != nil {
		client.SendJSON(WSMessage{Event: EventRoomSettings, Payload: map[string]interface{}{
			"requestPolicy": room.RequestPolicy,
		}})
	}

	// Replay the current mic state UNCONDITIONALLY — same event and payload
	// shape as the ActionDJMic broadcast, so clients handle a replay and a
	// live toggle identically. Active: a late joiner connects to voice
	// immediately instead of waiting for the next toggle. Inactive: a
	// RECONNECTING listener who missed the mic-off broadcast gets unstuck
	// (their music would otherwise stay force-paused forever).
	h.mu.RLock()
	micActive, micPause, micDJ := h.micActive, h.micPauseMusic, h.micDJName
	h.mu.RUnlock()
	client.SendJSON(WSMessage{Event: EventDJMicState, Payload: map[string]interface{}{
		"active":     micActive,
		"pauseMusic": micPause,
		"djName":     micDJ,
	}})

	if client.IsDJ && len(pending) > 0 {
		client.SendJSON(WSMessage{Event: EventRequestUpdate, Payload: pending})
	}

	h.broadcastListenerList()

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
		// Chat requires a verified account. Anonymous listeners and
		// unverified signups can view and listen only. DJs are exempt:
		// holding the room's DJ key already proves ownership.
		if !client.IsDJ {
			if client.User == nil {
				client.sendError("create an account and verify your email to join the chat")
				return
			}
			if !client.User.EmailVerified {
				client.sendError("verify your email address to join the chat")
				return
			}
		}
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
		// Server-side moderation — the client-side filter is trivially
		// bypassed by anyone speaking the WS protocol directly.
		if moderation.ContainsProfanity(p.Message) {
			client.sendError("message contains prohibited language")
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

		// Broadcast first, persist async (FIFO via persistLoop) so one
		// slow insert can't cap the room's chat throughput. See
		// persistLoop for the durability tradeoff.
		h.broadcastJSON(WSMessage{Event: EventChatMessage, Payload: chatMsg})
		h.persistChat(chatMsg)

	case ActionReaction:
		var p ReactionPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Emoji == "" {
			return // silently ignore invalid reactions
		}
		// Cap the payload and rate — each reaction fans out to every
		// listener, so an unlimited ~4KB "emoji" is a 1-to-N broadcast
		// amplifier that evicts real chat/playback events from the buffer.
		if len([]rune(p.Emoji)) > 16 {
			return
		}
		now := time.Now()
		if now.Sub(client.LastReaction) < 300*time.Millisecond {
			return // silently drop; reactions are fire-and-forget
		}
		client.LastReaction = now
		// Broadcast to all clients in the room (including sender)
		h.broadcastJSON(WSMessage{Event: EventReaction, Payload: map[string]string{
			"emoji":    p.Emoji,
			"username": client.DisplayName(),
		}})

	case ActionSubmitTrack:
		var p SubmitTrackPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			client.rejectSubmit("invalid track submission")
			return
		}

		// Field caps + source/host allowlist (see models.ValidateTrackSubmission).
		if err := models.ValidateTrackSubmission(p.Title, p.Artist, p.Source, p.SourceURL, p.Duration, client.IsDJ); err != nil {
			client.rejectSubmit(err.Error())
			return
		}
		if moderation.ContainsProfanity(p.Title) || moderation.ContainsProfanity(p.Artist) {
			client.rejectSubmit("track title contains prohibited language")
			return
		}

		// Rate limit + per-submitter queue cap for non-DJs: submissions
		// insert DB rows and rebroadcast the whole queue to every client,
		// so an unthrottled loop is an O(n²) flood. The cooldown stamp
		// lands only after the checks pass — a rejection (cap reached,
		// requests closed) must not also charge the 5s cooldown.
		if !client.IsDJ {
			if time.Since(client.LastSubmit) < 5*time.Second {
				client.rejectSubmit("you're submitting too fast — wait a few seconds")
				return
			}
			if n, err := h.pg.CountActiveQueueEntriesBySession(ctx, h.RoomID, client.Session.ID); err == nil && n >= 10 {
				client.rejectSubmit("you already have 10 tracks waiting — let some play first")
				return
			}
			client.LastSubmit = time.Now()
		}

		// Get room to check policy
		room, _ := h.pg.GetRoomByID(ctx, h.RoomID)
		if room == nil {
			client.rejectSubmit("room not found")
			return
		}
		if room.RequestPolicy == models.RequestPolicyClosed && !client.IsDJ {
			client.rejectSubmit("requests are closed for this room")
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
			client.rejectSubmit("failed to save track")
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
			client.rejectSubmit("failed to add to queue")
			return
		}

		// WS CONTRACT (frozen): confirm the submission directly to the
		// submitting client before any broadcast echo.
		client.sendSubmitResult(true, "")

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
		if err := h.pg.UpdateQueueEntryStatus(ctx, h.RoomID, p.EntryID, models.QueueApproved); err != nil {
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
		h.pg.UpdateQueueEntryStatus(ctx, h.RoomID, p.EntryID, models.QueueRejected)

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
		// Broadcast first, persist async — same path as regular chat.
		h.broadcastJSON(WSMessage{Event: EventChatMessage, Payload: chatMsg})
		h.persistChat(chatMsg)

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
		h.mu.Lock()
		if !h.Clients[client] {
			// The sender disconnected while this action sat in the inbound
			// queue; onUnregister has already settled mic state — a dead
			// connection must not resurrect a mic nobody is holding.
			h.mu.Unlock()
			return
		}
		h.micActive = micPayload.Active
		h.micPauseMusic = micPayload.PauseMusic
		h.micDJName = client.DisplayName()
		if micPayload.Active {
			h.micOwner = client
		} else {
			h.micOwner = nil
		}
		h.mu.Unlock()
		h.broadcastJSON(WSMessage{
			Event: EventDJMicState,
			Payload: map[string]interface{}{
				"active":     micPayload.Active,
				"pauseMusic": micPayload.PauseMusic,
				"djName":     client.DisplayName(),
			},
		})

	case ActionReportDuration:
		// Per-client throttle: each report spawns a goroutine doing
		// Postgres + Redis lookups server-side, so a frame loop would be
		// cheap amplification even though the reports themselves are
		// verified downstream.
		if time.Since(client.LastPlaybackReport) < 2*time.Second {
			return
		}
		client.LastPlaybackReport = time.Now()
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
		// Any listener can report that the autoplay track ended; the
		// playback service verifies elapsed time server-side. Same
		// per-client throttle as report_duration.
		if time.Since(client.LastPlaybackReport) < 2*time.Second {
			return
		}
		client.LastPlaybackReport = time.Now()
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
	h.enqueueBroadcast(data)
}

// BroadcastJSON is the exported version for use by HTTP handlers and the
// playback services. The payload is marshaled exactly once and the send
// never blocks the caller.
func (h *Hub) BroadcastJSON(msg WSMessage) {
	h.broadcastJSON(msg)
}

// enqueueBroadcast hands a marshaled payload to the fanout loop without
// ever blocking. The Run loop is the sole consumer of Broadcast, so a
// blocking send from inside the hub's own goroutines could deadlock the
// room; and once the hub has shut down nothing drains the channel at
// all. Policy: if the buffer is full (only possible under a pathological
// burst, since fanout does no I/O) the message is dropped and logged.
func (h *Hub) enqueueBroadcast(data []byte) {
	select {
	case h.Broadcast <- data:
	case <-h.quit:
		// Hub already shut down; drop silently.
	default:
		log.Printf("[ws] room %s: broadcast buffer full, dropping message", h.RoomSlug)
	}
}

// updateListenerCount runs one presence mutation (AddListener or
// RemoveListener) and enqueues its listener_count broadcast as an atomic
// pair under countMu, so counts reach clients in mutation order and a stale
// count can never overwrite a newer one.
func (h *Hub) updateListenerCount(mutate func() (int64, error)) {
	h.countMu.Lock()
	defer h.countMu.Unlock()
	count, _ := mutate()
	h.broadcastListenerCount(int(count))
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
	// Deliberately no user IDs here: the list goes to every client in the
	// room (anonymous ones included), and broadcasting account IDs handed
	// spammers a ready-made target list for DMs.
	type listenerInfo struct {
		Username    string `json:"username"`
		AvatarColor string `json:"avatarColor"`
		IsDJ        bool   `json:"isDJ"`
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
		})
	}
	h.mu.RUnlock()
	h.broadcastJSON(WSMessage{Event: EventListenerList, Payload: listeners})
}

// SessionClientCount reports how many current clients belong to the given
// session — the basis of the per-session connection cap.
func (h *Hub) SessionClientCount(sessionID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for client := range h.Clients {
		if client.Session != nil && client.Session.ID == sessionID {
			n++
		}
	}
	return n
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
			client.send(outbound{data: data})
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
