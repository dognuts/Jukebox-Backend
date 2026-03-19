package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
)

type SessionHandler struct {
	redis *store.RedisStore
}

func NewSessionHandler(redis *store.RedisStore) *SessionHandler {
	return &SessionHandler{redis: redis}
}

// GET /api/session
// Returns the current anonymous session info.
func (h *SessionHandler) GetCurrent(w http.ResponseWriter, r *http.Request) {
	session := middleware.GetSession(r.Context())
	if session == nil {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// PATCH /api/session
// Update display name.
func (h *SessionHandler) Update(w http.ResponseWriter, r *http.Request) {
	session := middleware.GetSession(r.Context())
	if session == nil {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}

	var req models.UpdateDisplayNameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.DisplayName == "" || len(req.DisplayName) > 30 {
		http.Error(w, "display name must be 1-30 characters", http.StatusBadRequest)
		return
	}

	if err := h.redis.UpdateSessionName(r.Context(), session.ID, req.DisplayName); err != nil {
		http.Error(w, "failed to update", http.StatusInternalServerError)
		return
	}

	session.DisplayName = req.DisplayName
	writeJSON(w, http.StatusOK, session)
}
