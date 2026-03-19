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

type PlaylistHandler struct {
	pg *store.PGStore
}

func NewPlaylistHandler(pg *store.PGStore) *PlaylistHandler {
	return &PlaylistHandler{pg: pg}
}

// GET /api/playlists — list user's playlists
func (h *PlaylistHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	playlists, err := h.pg.GetPlaylistsByUser(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "failed to list playlists", http.StatusInternalServerError)
		return
	}
	if playlists == nil {
		playlists = []models.Playlist{}
	}
	writeJSON(w, http.StatusOK, playlists)
}

// POST /api/playlists — create a playlist
func (h *PlaylistHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	pl := &models.Playlist{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Name:      body.Name,
		IsLiked:   false,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := h.pg.CreatePlaylist(r.Context(), pl); err != nil {
		http.Error(w, "failed to create playlist", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, pl)
}

// GET /api/playlists/{id} — get playlist with tracks
func (h *PlaylistHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	pl, err := h.pg.GetPlaylistWithTracks(r.Context(), id)
	if err != nil || pl == nil {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}

	if pl.UserID != user.ID {
		http.Error(w, "not your playlist", http.StatusForbidden)
		return
	}

	writeJSON(w, http.StatusOK, pl)
}

// PATCH /api/playlists/{id} — rename playlist
func (h *PlaylistHandler) Update(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	pl, _ := h.pg.GetPlaylistByID(r.Context(), id)
	if pl == nil || pl.UserID != user.ID {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}

	if pl.IsLiked {
		http.Error(w, "cannot rename liked tracks playlist", http.StatusBadRequest)
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	h.pg.UpdatePlaylistName(r.Context(), id, body.Name)
	pl.Name = body.Name
	writeJSON(w, http.StatusOK, pl)
}

// DELETE /api/playlists/{id} — delete playlist
func (h *PlaylistHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	pl, _ := h.pg.GetPlaylistByID(r.Context(), id)
	if pl == nil || pl.UserID != user.ID {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}

	if pl.IsLiked {
		http.Error(w, "cannot delete liked tracks playlist", http.StatusBadRequest)
		return
	}

	h.pg.DeletePlaylist(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/playlists/{id}/tracks — add track to playlist
func (h *PlaylistHandler) AddTrack(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	playlistID := chi.URLParam(r, "id")
	pl, _ := h.pg.GetPlaylistByID(r.Context(), playlistID)
	if pl == nil || pl.UserID != user.ID {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}

	var body struct {
		TrackID string `json:"trackId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TrackID == "" {
		http.Error(w, "trackId is required", http.StatusBadRequest)
		return
	}

	pt := &models.PlaylistTrack{
		ID:      uuid.New().String(),
		TrackID: body.TrackID,
	}

	if err := h.pg.AddTrackToPlaylist(r.Context(), pt, playlistID); err != nil {
		http.Error(w, "failed to add track", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"status": "added"})
}

// DELETE /api/playlists/{id}/tracks/{trackId} — remove track from playlist
func (h *PlaylistHandler) RemoveTrack(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	playlistID := chi.URLParam(r, "id")
	trackID := chi.URLParam(r, "trackId")

	pl, _ := h.pg.GetPlaylistByID(r.Context(), playlistID)
	if pl == nil || pl.UserID != user.ID {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}

	h.pg.RemoveTrackFromPlaylist(r.Context(), playlistID, trackID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}
