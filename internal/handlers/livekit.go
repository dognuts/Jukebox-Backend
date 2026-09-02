package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jukebox/backend/internal/config"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/store"
)

type LiveKitHandler struct {
	cfg *config.Config
	pg  *store.PGStore
}

func NewLiveKitHandler(cfg *config.Config, pg *store.PGStore) *LiveKitHandler {
	return &LiveKitHandler{cfg: cfg, pg: pg}
}

// LiveKit JWT claims — follows LiveKit's access token spec
type liveKitGrant struct {
	RoomJoin     bool   `json:"roomJoin,omitempty"`
	Room         string `json:"room,omitempty"`
	CanPublish   *bool  `json:"canPublish,omitempty"`
	CanSubscribe *bool  `json:"canSubscribe,omitempty"`
}

type liveKitClaims struct {
	jwt.RegisteredClaims
	Video *liveKitGrant `json:"video,omitempty"`
	Name  string        `json:"name,omitempty"`
}

func boolPtr(b bool) *bool { return &b }

// POST /api/livekit/token
// Body: { "roomSlug": "...", "isDJ": true/false }
// Returns: { "token": "...", "url": "wss://..." }
func (h *LiveKitHandler) GetToken(w http.ResponseWriter, r *http.Request) {
	if h.cfg.LiveKitAPIKey == "" || h.cfg.LiveKitAPISecret == "" {
		http.Error(w, "LiveKit not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		RoomSlug string `json:"roomSlug"`
		IsDJ     bool   `json:"isDJ"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoomSlug == "" {
		http.Error(w, "invalid request — roomSlug required", http.StatusBadRequest)
		return
	}

	// Determine identity from auth (logged in user) or session (anonymous)
	var identity string
	var displayName string

	// Check for authenticated user first
	if user := middleware.GetUser(r.Context()); user != nil {
		identity = "user:" + user.ID
		displayName = user.DisplayName
	} else if session := middleware.GetSession(r.Context()); session != nil {
		identity = "session:" + session.ID
		displayName = session.DisplayName
	} else {
		http.Error(w, "no identity", http.StatusUnauthorized)
		return
	}

	if displayName == "" {
		displayName = "Listener"
	}

	// LiveKit room name = "jukebox:" + slug
	livekitRoom := "jukebox:" + req.RoomSlug

	// Build grants — DJ can publish audio, listeners can only subscribe.
	// Publish rights are granted ONLY on proof of the room's DJ key —
	// never on the client's say-so, which would let any visitor broadcast
	// audio into any room's voice channel.
	grant := &liveKitGrant{
		RoomJoin:     true,
		Room:         livekitRoom,
		CanSubscribe: boolPtr(true),
		CanPublish:   boolPtr(false),
	}

	if req.IsDJ {
		room, err := h.pg.GetRoomBySlug(r.Context(), req.RoomSlug)
		if err != nil || room == nil {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		if !middleware.VerifyDJKey(middleware.ExtractDJKey(r), room.DJKeyHash) {
			http.Error(w, "DJ key required to publish", http.StatusForbidden)
			return
		}
		grant.CanPublish = boolPtr(true)
	}

	now := time.Now()
	claims := liveKitClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    h.cfg.LiveKitAPIKey,
			Subject:   identity,
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Hour)),
			ID:        identity + ":" + req.RoomSlug,
		},
		Video: grant,
		Name:  displayName,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(h.cfg.LiveKitAPISecret))
	if err != nil {
		log.Printf("[livekit] token signing error: %v", err)
		http.Error(w, "failed to generate token", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"token": tokenString,
		"url":   h.cfg.LiveKitURL,
	})
}
