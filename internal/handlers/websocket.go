package handlers

import (
	"log"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

// Per-IP and per-session concurrent-connection caps. Each connection
// costs goroutines + buffers server-side and (with a fresh session)
// inflates the room's public listener count, so both need a ceiling.
const (
	maxWSConnsPerIP      = 20
	maxWSConnsPerSession = 4
)

type WSHandler struct {
	pg             *store.PGStore
	redis          *store.RedisStore
	hubs           *ws.HubManager
	jwtSecret      string
	allowedOrigins map[string]bool

	connMu    sync.Mutex
	connsByIP map[string]int
}

func NewWSHandler(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager, jwtSecret string, corsOrigins []string) *WSHandler {
	origins := make(map[string]bool, len(corsOrigins))
	for _, o := range corsOrigins {
		origins[o] = true
	}
	return &WSHandler{pg: pg, redis: redis, hubs: hubs, jwtSecret: jwtSecret, allowedOrigins: origins, connsByIP: map[string]int{}}
}

// acquireIPSlot reserves a connection slot for the IP, returning false if
// the cap is reached. The caller must call the returned release exactly
// once when the connection ends.
func (h *WSHandler) acquireIPSlot(ip string) (release func(), ok bool) {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.connsByIP[ip] >= maxWSConnsPerIP {
		return nil, false
	}
	h.connsByIP[ip]++
	var once sync.Once
	return func() {
		once.Do(func() {
			h.connMu.Lock()
			defer h.connMu.Unlock()
			if h.connsByIP[ip] <= 1 {
				delete(h.connsByIP, ip)
			} else {
				h.connsByIP[ip]--
			}
		})
	}, true
}

// GET /ws/room/{slug}?ticket=...
//
// Authentication is by single-use ticket (POST /api/ws/ticket) so no
// reusable credential rides the URL into request logs. A cookie-resolved
// session is accepted as a fallback for listeners, but user identity and
// DJ status come only from a ticket.
func (h *WSHandler) HandleRoomWS(w http.ResponseWriter, r *http.Request) {
	// Validate WebSocket origin
	upgrader := websocket.Upgrader{
		ReadBufferSize:    1024,
		WriteBufferSize:   1024,
		EnableCompression: true, // permessage-deflate — reduces bandwidth ~60-70%
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // non-browser clients (curl, etc.)
			}
			// Allow in development
			if h.allowedOrigins["http://localhost:3000"] && (origin == "http://localhost:3000" || origin == "http://localhost:8080") {
				return true
			}
			return h.allowedOrigins[origin]
		},
	}
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()

	// Look up room
	room, err := h.pg.GetRoomBySlug(ctx, slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// Resolve identity: ticket first, cookie-session fallback.
	var (
		session *models.Session
		userID  string
		isDJ    bool
	)
	if ticketID := r.URL.Query().Get("ticket"); ticketID != "" {
		ticket, err := h.redis.RedeemWSTicket(ctx, ticketID)
		if err == nil && ticket != nil && ticket.RoomID == room.ID {
			session, _ = h.redis.GetSession(ctx, ticket.SessionID)
			userID = ticket.UserID
			isDJ = ticket.IsDJ
		}
	}
	if session == nil {
		// Fallback: session from cookie via middleware (no user, no DJ).
		session = middleware.GetSession(ctx)
	}
	if session == nil {
		http.Error(w, "no session — fetch a ws ticket first", http.StatusUnauthorized)
		return
	}

	// Per-IP connection cap.
	ip := ClientIP(r)
	release, ok := h.acquireIPSlot(ip)
	if !ok {
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}

	// Upgrade to WebSocket BEFORE creating the hub: a hub minted for a
	// handshake that never registers a client has no unregister path and
	// would idle forever.
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		release()
		log.Printf("ws upgrade: %v", err)
		return
	}

	hub := h.hubs.GetOrCreate(room.ID, room.Slug)

	// Per-session cap within this room. Best-effort (check-then-act), but
	// the per-IP cap above bounds how far a race can overshoot it.
	if hub.SessionClientCount(session.ID) >= maxWSConnsPerSession {
		release()
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "too many connections for this session"))
		conn.Close()
		return
	}
	conn.EnableWriteCompression(true)
	// Level 1, not 6: gorilla negotiates permessage-deflate without
	// context takeover, so every frame is compressed independently and
	// higher levels buy only a few percent on this app's mostly-small
	// JSON frames while costing several times the CPU. The larger frames
	// (queue updates, chat replay) still shrink well at level 1, and
	// broadcast frames are wrapped in a single PreparedMessage (see
	// ws.Hub fanout) so they are compressed once per broadcast rather
	// than once per recipient.
	conn.SetCompressionLevel(1)

	// Create client
	client := ws.NewClient(hub, conn, session)
	client.IsDJ = isDJ

	if userID != "" {
		if user, err := h.pg.GetUserByID(ctx, userID); err == nil && user != nil && !user.IsBanned {
			client.UserID = user.ID
			client.User = user
		}
	}

	// Register with hub. This can only fail if the hub shut down between
	// lookup and registration (room just ended) — drop the connection and
	// let the client's reconnect land on a fresh hub.
	if !hub.RegisterClient(client) {
		release()
		conn.Close()
		return
	}

	// Free the IP slot when the hub drops the client.
	go func() {
		<-client.Done()
		release()
	}()

	log.Printf("[ws] client connected to room %s (session=%s, isDJ=%v)", room.Slug, session.ID, client.IsDJ)

	// Start read/write pumps
	go client.WritePump()
	go client.ReadPump()
}
