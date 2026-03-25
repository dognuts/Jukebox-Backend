package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/playback"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

type AdminHandler struct {
	pg       *store.PGStore
	redis    *store.RedisStore
	hubs     *ws.HubManager
	playback *playback.SyncService
}

func NewAdminHandler(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager, pb *playback.SyncService) *AdminHandler {
	return &AdminHandler{pg: pg, redis: redis, hubs: hubs, playback: pb}
}

// requireAdmin checks that the requesting user is an admin.
func (h *AdminHandler) requireAdmin(r *http.Request) *models.User {
	user := middleware.GetUser(r.Context())
	if user == nil || !user.IsAdmin {
		return nil
	}
	return user
}

// GET /api/admin/rooms — list all rooms with full details
func (h *AdminHandler) ListRooms(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	rooms, err := h.pg.ListAllRooms(ctx)
	if err != nil {
		log.Printf("admin list rooms: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Enrich with listener counts
	for i := range rooms {
		count, _ := h.redis.GetListenerCount(ctx, rooms[i].ID)
		rooms[i].ListenerCount = int(count)
	}

	writeJSON(w, http.StatusOK, rooms)
}

// POST /api/admin/rooms — create an official room
func (h *AdminHandler) CreateOfficialRoom(w http.ResponseWriter, r *http.Request) {
	user := h.requireAdmin(r)
	if user == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}

	var req struct {
		Name          string  `json:"name"`
		Description   string  `json:"description"`
		Genre         string  `json:"genre"`
		Vibes         []string `json:"vibes"`
		CoverArt      string  `json:"coverArt"`
		CoverGradient string  `json:"coverGradient"`
		RequestPolicy string  `json:"requestPolicy"`
		ScheduledStart string `json:"scheduledStart,omitempty"`
		ExpiresAt     string  `json:"expiresAt,omitempty"` // empty = eternal
		IsFeatured    bool    `json:"isFeatured"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	djKey, djKeyHash, err := middleware.GenerateDJKey()
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}

	session := middleware.GetSession(r.Context())
	slug := toSlug(req.Name) + "-" + time.Now().Format("0102")

	vibes := req.Vibes
	if vibes == nil {
		vibes = []string{}
	}

	var scheduledStart *time.Time
	if req.ScheduledStart != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduledStart)
		if err == nil {
			scheduledStart = &t
		}
	}

	var expiresAt *time.Time
	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err == nil {
			expiresAt = &t
		}
	}

	room := &models.Room{
		ID:             uuid.New().String(),
		Slug:           slug,
		Name:           req.Name,
		Description:    req.Description,
		Genre:          req.Genre,
		Vibes:          vibes,
		CoverGradient:  req.CoverGradient,
		CoverArtURL:    req.CoverArt,
		RequestPolicy:  models.RequestPolicy(req.RequestPolicy),
		IsLive:         false,
		IsOfficial:     true,
		IsFeatured:     req.IsFeatured,
		DJKeyHash:      djKeyHash,
		DJSessionID:    session.ID,
		CreatedAt:      time.Now(),
		ScheduledStart: scheduledStart,
		ExpiresAt:      expiresAt,
	}
	if room.RequestPolicy == "" {
		room.RequestPolicy = models.RequestPolicyOpen
	}

	if err := h.pg.CreateRoom(r.Context(), room); err != nil {
		log.Printf("admin create room: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if req.IsFeatured {
		h.pg.SetRoomFeatured(r.Context(), room.ID, true)
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"room":  room,
		"djKey": djKey,
	})
}

// POST /api/admin/rooms/{id}/shutdown — force-close any room
func (h *AdminHandler) ShutdownRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	ctx := r.Context()

	room, err := h.pg.GetRoomByID(ctx, roomID)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	h.pg.EndRoom(ctx, room.ID)
	h.pg.ClearNowPlaying(ctx, room.ID)
	h.redis.ClearPlaybackState(ctx, room.ID)
	h.redis.ClearListeners(ctx, room.ID)
	h.playback.CancelAdvance(room.ID)

	if hub := h.hubs.Get(room.ID); hub != nil {
		hub.BroadcastJSON(ws.WSMessage{
			Event:   "room_ended",
			Payload: map[string]string{"reason": "Room was shut down by an administrator"},
		})
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "shutdown"})
}

// DELETE /api/admin/rooms/{id} — permanently delete a room
func (h *AdminHandler) DeleteRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	ctx := r.Context()

	room, err := h.pg.GetRoomByID(ctx, roomID)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// If room is live, shut it down first
	if room.IsLive {
		h.pg.ClearNowPlaying(ctx, room.ID)
		h.redis.ClearPlaybackState(ctx, room.ID)
		h.redis.ClearListeners(ctx, room.ID)
		h.playback.CancelAdvance(room.ID)

		if hub := h.hubs.Get(room.ID); hub != nil {
			hub.BroadcastJSON(ws.WSMessage{
				Event:   "room_ended",
				Payload: map[string]string{"reason": "Room was deleted by an administrator"},
			})
		}
	}

	if err := h.pg.DeleteRoom(ctx, room.ID); err != nil {
		log.Printf("[admin] delete room %s: %v", room.ID, err)
		http.Error(w, "failed to delete room", http.StatusInternalServerError)
		return
	}

	log.Printf("[admin] deleted room %s (%s)", room.Name, room.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/admin/rooms/{id}/feature — set room as featured
func (h *AdminHandler) SetFeatured(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	var req struct {
		Featured bool `json:"featured"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if err := h.pg.SetRoomFeatured(r.Context(), roomID, req.Featured); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"featured": req.Featured})
}

// POST /api/admin/rooms/{id}/official — toggle official status
func (h *AdminHandler) SetOfficial(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	var req struct {
		Official bool `json:"official"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if err := h.pg.SetRoomOfficial(r.Context(), roomID, req.Official); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"official": req.Official})
}

// PATCH /api/admin/rooms/{id} — update room settings (expiry, schedule, etc)
func (h *AdminHandler) UpdateRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	var req struct {
		ExpiresAt *string `json:"expiresAt"` // null = eternal
	}
	json.NewDecoder(r.Body).Decode(&req)

	ctx := r.Context()
	if req.ExpiresAt != nil {
		if *req.ExpiresAt == "" {
			h.pg.SetRoomExpiry(ctx, roomID, nil)
		} else {
			t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
			if err == nil {
				h.pg.SetRoomExpiry(ctx, roomID, &t)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// GET /api/admin/featured — get the current featured room (or auto-pick by listener count)
func (h *AdminHandler) GetFeatured(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Check for manually featured room first
	featured, _ := h.pg.GetFeaturedRoom(ctx)
	if featured != nil {
		count, _ := h.redis.GetListenerCount(ctx, featured.ID)
		featured.ListenerCount = int(count)
		writeJSON(w, http.StatusOK, featured)
		return
	}

	// Auto-pick: live room with most listeners
	rooms, _ := h.pg.ListRooms(ctx, true, "")
	if len(rooms) == 0 {
		writeJSON(w, http.StatusOK, nil)
		return
	}

	best := &rooms[0]
	for i := range rooms {
		count, _ := h.redis.GetListenerCount(ctx, rooms[i].ID)
		rooms[i].ListenerCount = int(count)
		if rooms[i].ListenerCount > best.ListenerCount {
			best = &rooms[i]
		}
	}
	writeJSON(w, http.StatusOK, best)
}

// ==================== User Management ====================

// GET /api/admin/users — list all users with optional search
func (h *AdminHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	query := r.URL.Query().Get("q")
	users, err := h.pg.AdminListUsers(r.Context(), query)
	if err != nil {
		log.Printf("admin list users: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// GET /api/admin/users/{id} — get full user details
func (h *AdminHandler) GetUser(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	userID := chi.URLParam(r, "id")
	user, err := h.pg.AdminGetUser(r.Context(), userID)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// PATCH /api/admin/users/{id} — update user fields (ban, verify, set admin, reset neon, etc.)
func (h *AdminHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	userID := chi.URLParam(r, "id")

	var req struct {
		IsAdmin       *bool   `json:"isAdmin,omitempty"`
		IsBanned      *bool   `json:"isBanned,omitempty"`
		EmailVerified *bool   `json:"emailVerified,omitempty"`
		IsPlus        *bool   `json:"isPlus,omitempty"`
		NeonBalance   *int    `json:"neonBalance,omitempty"`
		DisplayName   *string `json:"displayName,omitempty"`
		StageName     *string `json:"stageName,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if req.IsAdmin != nil {
		h.pg.AdminSetField(ctx, userID, "is_admin", *req.IsAdmin)
	}
	if req.IsBanned != nil {
		h.pg.AdminSetField(ctx, userID, "is_banned", *req.IsBanned)
	}
	if req.EmailVerified != nil {
		h.pg.AdminSetField(ctx, userID, "email_verified", *req.EmailVerified)
	}
	if req.IsPlus != nil {
		h.pg.AdminSetField(ctx, userID, "is_plus", *req.IsPlus)
	}
	if req.NeonBalance != nil {
		h.pg.AdminSetField(ctx, userID, "neon_balance", *req.NeonBalance)
	}
	if req.DisplayName != nil {
		h.pg.AdminSetField(ctx, userID, "display_name", *req.DisplayName)
	}
	if req.StageName != nil {
		h.pg.AdminSetField(ctx, userID, "stage_name", *req.StageName)
	}

	user, _ := h.pg.AdminGetUser(ctx, userID)
	writeJSON(w, http.StatusOK, user)
}

// DELETE /api/admin/users/{id} — delete a user account
func (h *AdminHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	userID := chi.URLParam(r, "id")
	if err := h.pg.AdminDeleteUser(r.Context(), userID); err != nil {
		log.Printf("admin delete user: %v", err)
		http.Error(w, "failed to delete user", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ==================== Autoplay Room Management ====================

// POST /api/admin/autoplay/rooms — create a new autoplay room
func (h *AdminHandler) CreateAutoplayRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Genre       string `json:"genre"`
		CoverGradient string `json:"coverGradient"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	slug := toSlug(req.Name)
	roomID := uuid.New().String()

	room := &models.Room{
		ID:            roomID,
		Slug:          slug,
		Name:          req.Name,
		Description:   req.Description,
		Genre:         req.Genre,
		CoverGradient: req.CoverGradient,
		RequestPolicy: models.RequestPolicyClosed,
		IsLive:        false, // not live until playlist is activated
		IsOfficial:    true,
		IsAutoplay:    true,
		CreatedAt:     time.Now(),
	}

	if err := h.pg.CreateAutoplayRoom(ctx, room); err != nil {
		log.Printf("admin create autoplay room: %v", err)
		http.Error(w, "failed to create room", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, room)
}

// GET /api/admin/autoplay/rooms/{id}/playlists — get live and staged playlists
func (h *AdminHandler) GetAutoplayPlaylists(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	playlists, err := h.pg.GetAutoplayPlaylists(r.Context(), roomID)
	if err != nil {
		http.Error(w, "failed to load playlists", http.StatusInternalServerError)
		return
	}
	if playlists == nil {
		playlists = []models.AutoplayPlaylist{}
	}
	writeJSON(w, http.StatusOK, playlists)
}

// PUT /api/admin/autoplay/rooms/{id}/staged — save the staged (next) playlist
func (h *AdminHandler) SaveStagedPlaylist(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")

	var req struct {
		Name   string                  `json:"name"`
		Tracks []models.AutoplayTrack  `json:"tracks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	playlist := &models.AutoplayPlaylist{
		RoomID:    roomID,
		Status:    "staged",
		Name:      req.Name,
		Tracks:    req.Tracks,
		CreatedAt: time.Now(),
	}
	if err := h.pg.SaveAutoplayPlaylist(r.Context(), playlist); err != nil {
		log.Printf("save staged playlist: %v", err)
		http.Error(w, "failed to save", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, playlist)
}

// POST /api/admin/autoplay/rooms/{id}/activate — promote staged to live
func (h *AdminHandler) ActivatePlaylist(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	ctx := r.Context()

	playlist, err := h.pg.ActivateStagedPlaylist(ctx, roomID)
	if err != nil || playlist == nil {
		log.Printf("activate playlist: %v", err)
		http.Error(w, "failed to activate — is there a staged playlist?", http.StatusBadRequest)
		return
	}

	// Mark room as live and autoplay
	h.pg.SetRoomAutoplay(ctx, roomID, true)

	// Start playing the first track
	h.playback.StartAutoplayRooms(ctx)

	writeJSON(w, http.StatusOK, playlist)
}

// DELETE /api/admin/autoplay/rooms/{id}/staged — delete the staged playlist
func (h *AdminHandler) DeleteStagedPlaylist(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	h.pg.DeleteAutoplayPlaylist(r.Context(), roomID, "staged")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/admin/autoplay/rooms/{id}/stop — stop an autoplay room
func (h *AdminHandler) StopAutoplayRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	roomID := chi.URLParam(r, "id")
	ctx := r.Context()

	h.pg.SetRoomAutoplay(ctx, roomID, false)
	h.playback.CancelAdvance(roomID)
	h.redis.ClearPlaybackState(ctx, roomID)
	h.pg.ClearNowPlaying(ctx, roomID)

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// GET /api/admin/metrics — dashboard metrics
func (h *AdminHandler) GetMetrics(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(r) == nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	ctx := r.Context()

	daysParam := r.URL.Query().Get("days")
	days := 30
	if daysParam != "" {
		if d, err := strconv.Atoi(daysParam); err == nil && d > 0 && d <= 90 {
			days = d
		}
	}

	summary, _ := h.pg.GetMetricsSummary(ctx)
	signups, _ := h.pg.GetSignupsPerDay(ctx, days)
	roomsCreated, _ := h.pg.GetRoomsCreatedPerDay(ctx, days)
	activeRooms, _ := h.pg.GetActiveRoomsPerDay(ctx, days)
	topGenres, _ := h.pg.GetTopGenres(ctx, 10)
	listenHours, _ := h.pg.GetListenHoursPerDay(ctx, days)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"summary":      summary,
		"signups":      signups,
		"roomsCreated": roomsCreated,
		"activeRooms":  activeRooms,
		"topGenres":    topGenres,
		"listenHours":  listenHours,
	})
}
