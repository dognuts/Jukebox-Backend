package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/store"
)

type MessageHandler struct {
	pg *store.PGStore
}

func NewMessageHandler(pg *store.PGStore) *MessageHandler {
	return &MessageHandler{pg: pg}
}

// GET /api/messages — list all conversations
func (h *MessageHandler) ListConversations(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	convos, err := h.pg.GetConversationList(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "failed to list conversations", http.StatusInternalServerError)
		return
	}
	if convos == nil {
		convos = []models.ConversationSummary{}
	}
	writeJSON(w, http.StatusOK, convos)
}

// GET /api/messages/{userId} — get messages with a specific user
func (h *MessageHandler) GetConversation(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	otherUserID := chi.URLParam(r, "userId")
	if otherUserID == "" {
		http.Error(w, "userId is required", http.StatusBadRequest)
		return
	}

	msgs, err := h.pg.GetConversation(r.Context(), user.ID, otherUserID, 50)
	if err != nil {
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}
	if msgs == nil {
		msgs = []models.DirectMessage{}
	}

	// Mark as read
	h.pg.MarkConversationRead(r.Context(), user.ID, otherUserID)

	writeJSON(w, http.StatusOK, msgs)
}

// POST /api/messages/{userId} — send a message
func (h *MessageHandler) SendMessage(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	toUserID := chi.URLParam(r, "userId")
	if toUserID == "" {
		http.Error(w, "userId is required", http.StatusBadRequest)
		return
	}

	if toUserID == user.ID {
		http.Error(w, "cannot message yourself", http.StatusBadRequest)
		return
	}

	// Verify recipient exists
	recipient, _ := h.pg.GetUserByID(r.Context(), toUserID)
	if recipient == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	if len(body.Message) > 2000 {
		http.Error(w, "message too long (max 2000 chars)", http.StatusBadRequest)
		return
	}

	msg := &models.DirectMessage{
		ID:         uuid.New().String(),
		FromUserID: user.ID,
		ToUserID:   toUserID,
		Message:    body.Message,
		CreatedAt:  time.Now(),
	}

	if err := h.pg.SendDirectMessage(r.Context(), msg); err != nil {
		http.Error(w, "failed to send message", http.StatusInternalServerError)
		return
	}

	msg.FromDisplayName = user.DisplayName
	msg.FromAvatarColor = user.AvatarColor

	writeJSON(w, http.StatusCreated, msg)
}

// POST /api/messages/{userId}/read — mark conversation as read
func (h *MessageHandler) MarkRead(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	otherUserID := chi.URLParam(r, "userId")
	h.pg.MarkConversationRead(r.Context(), user.ID, otherUserID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
