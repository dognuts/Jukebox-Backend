package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

type QueueHandler struct {
	pg    *store.PGStore
	redis *store.RedisStore
	hubs  *ws.HubManager
}

func NewQueueHandler(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager) *QueueHandler {
	return &QueueHandler{pg: pg, redis: redis, hubs: hubs}
}

// GET /api/rooms/{slug}/queue
func (h *QueueHandler) GetQueue(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()

	room, err := h.pg.GetRoomBySlug(ctx, slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	queue, err := h.pg.GetQueue(ctx, room.ID)
	if err != nil {
		log.Printf("get queue: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if queue == nil {
		queue = []models.QueueEntry{}
	}

	writeJSON(w, http.StatusOK, queue)
}

// POST /api/rooms/{slug}/queue
func (h *QueueHandler) SubmitTrack(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()
	session := middleware.GetSession(ctx)
	if session == nil {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}

	room, err := h.pg.GetRoomBySlug(ctx, slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// Check if DJ (can always submit)
	djKey := middleware.ExtractDJKey(r)
	isDJ := middleware.VerifyDJKey(djKey, room.DJKeyHash)

	if room.RequestPolicy == models.RequestPolicyClosed && !isDJ {
		http.Error(w, "requests are closed", http.StatusForbidden)
		return
	}

	var req models.SubmitTrackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Create track
	track := &models.Track{
		ID:        uuid.New().String(),
		Title:     req.Title,
		Artist:    req.Artist,
		Duration:  req.Duration,
		Source:    models.TrackSource(req.Source),
		SourceURL: req.SourceURL,
		CreatedAt: time.Now(),
	}
	if err := h.pg.UpsertTrack(ctx, track); err != nil {
		log.Printf("upsert track: %v", err)
		http.Error(w, "failed to save track", http.StatusInternalServerError)
		return
	}

	status := models.QueueApproved
	if room.RequestPolicy == models.RequestPolicyApproval && !isDJ {
		status = models.QueuePending
	}

	entry := &models.QueueEntry{
		ID:          uuid.New().String(),
		RoomID:      room.ID,
		Track:       *track,
		SubmittedBy: session.DisplayName,
		SessionID:   session.ID,
		Status:      status,
		CreatedAt:   time.Now(),
	}

	if err := h.pg.AddToQueue(ctx, entry); err != nil {
		log.Printf("add to queue: %v", err)
		http.Error(w, "failed to add to queue", http.StatusInternalServerError)
		return
	}

	// Notify connected clients via WebSocket hub
	if hub := h.hubs.Get(room.ID); hub != nil {
		if status == models.QueueApproved {
			queue, _ := h.pg.GetQueue(ctx, room.ID)
			data, _ := json.Marshal(ws.WSMessage{Event: ws.EventQueueUpdate, Payload: queue})
			hub.Broadcast <- data
		}
	}

	writeJSON(w, http.StatusCreated, entry)
}

// GET /api/rooms/{slug}/requests  (DJ only)
func (h *QueueHandler) GetPendingRequests(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()
	djKey := middleware.ExtractDJKey(r)

	room, err := h.pg.GetRoomBySlug(ctx, slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	if !middleware.VerifyDJKey(djKey, room.DJKeyHash) {
		http.Error(w, "DJ key required", http.StatusForbidden)
		return
	}

	pending, err := h.pg.GetPendingRequests(ctx, room.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if pending == nil {
		pending = []models.QueueEntry{}
	}

	writeJSON(w, http.StatusOK, pending)
}
