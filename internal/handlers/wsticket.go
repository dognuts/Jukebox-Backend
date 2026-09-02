package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/jukebox/backend/internal/antispam"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/store"
)

// WSTicketHandler mints single-use WebSocket tickets. Browsers can't set
// custom headers on WebSocket handshakes, and putting the JWT / DJ key /
// session id in the ws URL wrote long-lived credentials into every proxy
// and app log line. Instead, the client POSTs here — where headers work —
// and connects with only the 30-second single-use ticket in the URL.
type WSTicketHandler struct {
	pg      *store.PGStore
	redis   *store.RedisStore
	limiter *antispam.RateLimiter
}

func NewWSTicketHandler(pg *store.PGStore, redis *store.RedisStore, limiter *antispam.RateLimiter) *WSTicketHandler {
	return &WSTicketHandler{pg: pg, redis: redis, limiter: limiter}
}

// POST /api/ws/ticket  { "roomSlug": "..." }
// Identity comes from the standard auth surfaces: session (cookie or
// X-Session-ID), user (Authorization bearer), DJ key (X-DJ-Key header).
func (h *WSTicketHandler) Create(w http.ResponseWriter, r *http.Request) {
	session := middleware.GetSession(r.Context())
	if session == nil {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}

	// Each mint costs a room lookup + a Redis write — cap it per IP.
	if h.limiter != nil {
		if allowed, _ := h.limiter.AllowWSTicket(r.Context(), ClientIP(r)); !allowed {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
	}

	var req struct {
		RoomSlug string `json:"roomSlug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoomSlug == "" {
		http.Error(w, "roomSlug required", http.StatusBadRequest)
		return
	}

	room, err := h.pg.GetRoomBySlug(r.Context(), req.RoomSlug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	ticket := &store.WSTicket{
		SessionID: session.ID,
		RoomID:    room.ID,
	}
	if user := middleware.GetUser(r.Context()); user != nil {
		ticket.UserID = user.ID
	}
	// DJ status is proven here, once, against the room's key hash — the
	// ws handshake later just trusts the ticket.
	if djKey := middleware.ExtractDJKey(r); djKey != "" {
		ticket.IsDJ = middleware.VerifyDJKey(djKey, room.DJKeyHash)
	}

	id, err := h.redis.CreateWSTicket(r.Context(), ticket)
	if err != nil {
		http.Error(w, "failed to create ticket", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"ticket": id})
}
