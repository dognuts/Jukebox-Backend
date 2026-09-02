package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/antispam"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/moderation"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

type QueueHandler struct {
	pg      *store.PGStore
	redis   *store.RedisStore
	hubs    *ws.HubManager
	limiter *antispam.RateLimiter
}

func NewQueueHandler(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager, limiter *antispam.RateLimiter) *QueueHandler {
	return &QueueHandler{pg: pg, redis: redis, hubs: hubs, limiter: limiter}
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

	// Same validation, moderation, rate limit, and per-submitter cap as
	// the WS submit path — this endpoint had none of them, so it was the
	// easier flood/beacon vector of the two.
	if err := models.ValidateTrackSubmission(req.Title, req.Artist, req.Source, req.SourceURL, req.Duration, isDJ); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if moderation.ContainsProfanity(req.Title) || moderation.ContainsProfanity(req.Artist) {
		http.Error(w, "track title contains prohibited language", http.StatusBadRequest)
		return
	}
	if !isDJ {
		if h.limiter != nil {
			if allowed, _ := h.limiter.AllowQueueSubmit(ctx, session.ID, room.ID); !allowed {
				http.Error(w, "you're submitting too fast — please slow down", http.StatusTooManyRequests)
				return
			}
		}
		if n, err := h.pg.CountActiveQueueEntriesBySession(ctx, room.ID, session.ID); err == nil && n >= 10 {
			http.Error(w, "you already have 10 tracks waiting — let some play first", http.StatusTooManyRequests)
			return
		}
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

	// Notify connected clients via WebSocket hub. BroadcastJSON never
	// blocks — a raw send into hub.Broadcast could strand this handler
	// goroutine forever if the hub had already shut down.
	if hub := h.hubs.Get(room.ID); hub != nil {
		if status == models.QueueApproved {
			queue, _ := h.pg.GetQueue(ctx, room.ID)
			hub.BroadcastJSON(ws.WSMessage{Event: ws.EventQueueUpdate, Payload: queue})
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
