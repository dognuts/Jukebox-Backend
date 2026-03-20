package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jukebox/backend/internal/config"
	"github.com/jukebox/backend/internal/middleware"
)

type LiveKitHandler struct {
	cfg *config.Config
}

func NewLiveKitHandler(cfg *config.Config) *LiveKitHandler {
	return &LiveKitHandler{cfg: cfg}
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

	// Build grants — DJ can publish audio, listeners can only subscribe
	grant := &liveKitGrant{
		RoomJoin:     true,
		Room:         livekitRoom,
		CanSubscribe: boolPtr(true),
	}

	if req.IsDJ {
		grant.CanPublish = boolPtr(true)
	} else {
		grant.CanPublish = boolPtr(false)
	}

	now := time.Now()
	claims := liveKitClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    h.cfg.LiveKitAPIKey,
			Subject:   identity,
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(6 * time.Hour)),
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
