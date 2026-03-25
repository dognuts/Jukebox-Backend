package handlers

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

type WSHandler struct {
	pg        *store.PGStore
	redis     *store.RedisStore
	hubs      *ws.HubManager
	jwtSecret string
	allowedOrigins map[string]bool
}

func NewWSHandler(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager, jwtSecret string, corsOrigins []string) *WSHandler {
	origins := make(map[string]bool, len(corsOrigins))
	for _, o := range corsOrigins {
		origins[o] = true
	}
	return &WSHandler{pg: pg, redis: redis, hubs: hubs, jwtSecret: jwtSecret, allowedOrigins: origins}
}

// GET /ws/room/{slug}?djKey=optional
func (h *WSHandler) HandleRoomWS(w http.ResponseWriter, r *http.Request) {
	// Validate WebSocket origin
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
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

	// Get session
	session := middleware.GetSession(ctx)
	if session == nil {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}

	// Look up room
	room, err := h.pg.GetRoomBySlug(ctx, slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// Upgrade to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}

	// Get or create hub for this room
	hub := h.hubs.GetOrCreate(room.ID, room.Slug)

	// Create client
	client := &ws.Client{
		Hub:     hub,
		Conn:    conn,
		Send:    make(chan []byte, 256),
		Session: session,
		IsDJ:    false,
	}

	// Set user if authenticated (from middleware or token query param)
	if user := middleware.GetUser(ctx); user != nil {
		client.UserID = user.ID
		client.User = user
	} else if tokenStr := r.URL.Query().Get("token"); tokenStr != "" {
		// JWT passed as query param for WebSocket connections
		claims, err := middleware.ValidateAccessToken(tokenStr, h.jwtSecret)
		if err == nil {
			user, err := h.pg.GetUserByID(ctx, claims.UserID)
			if err == nil && user != nil {
				client.UserID = user.ID
				client.User = user
			}
		}
	}

	// Check DJ key
	djKey := middleware.ExtractDJKey(r)
	ws.SetDJKey(client, djKey, room.DJKeyHash)

	// Register with hub
	hub.Register <- client

	log.Printf("[ws] client connected to room %s (session=%s, isDJ=%v)", room.Slug, session.ID, client.IsDJ)

	// Start read/write pumps
	go client.WritePump()
	go client.ReadPump()
}
