package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/playback"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
	"github.com/jukebox/backend/internal/youtube"
)

type AdminHandler struct {
	pg       *store.PGStore
	redis    *store.RedisStore
	hubs     *ws.HubManager
	playback *playback.SyncService
	yt       youtubeSearcher // nil when YOUTUBE_DATA_API_KEY unset
}

// NewAdminHandler takes the concrete *youtube.Client (not the youtubeSearcher
// interface) specifically to avoid the "typed nil in interface" Go pitfall:
// if main.go passes a nil *youtube.Client into an interface-typed parameter,
// the resulting interface value is NOT == nil and the nil check in the
// handler would fail open, panicking at request time.
func NewAdminHandler(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager, pb *playback.SyncService, yt *youtube.Client) *AdminHandler {
	h := &AdminHandler{pg: pg, redis: redis, hubs: hubs, playback: pb}
	if yt != nil {
		h.yt = yt
	}
	return h
}

// requireAdmin checks that the requester is authenticated AND admin. It writes
// 401 (no user — typically an expired or missing JWT) or 403 (authenticated
// but not admin) directly and returns nil in those cases, so the caller just
// does `if h.requireAdmin(w, r) == nil { return }`.
//
// Distinguishing 401 from 403 matters for the frontend: 401 signals "refresh
// your token and retry," while 403 means "your account doesn't have access."
// Conflating them into 403 left expired-token users looking like non-admins
// until their next scheduled refresh window, which was the cause of the save
// failures seen after a Render restart on 2026-04-22.
func (h *AdminHandler) requireAdmin(w http.ResponseWriter, r *http.Request) *models.User {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return nil
	}
	if !user.IsAdmin {
		http.Error(w, "admin required", http.StatusForbidden)
		return nil
	}
	return user
}

// GET /api/admin/rooms — list all rooms with full details
func (h *AdminHandler) ListRooms(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
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
	user := h.requireAdmin(w, r)
	if user == nil {
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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

// PATCH /api/admin/rooms/{id} — update room settings (expiry, cover art, etc)
func (h *AdminHandler) UpdateRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
		return
	}
	roomID := chi.URLParam(r, "id")
	var req struct {
		ExpiresAt     *string `json:"expiresAt"` // null = eternal
		CoverArt      *string `json:"coverArt,omitempty"`
		CoverGradient *string `json:"coverGradient,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("admin update room %s: decode error: %v (content-length=%s)", roomID, err, r.Header.Get("Content-Length"))
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

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
	if req.CoverArt != nil {
		if err := h.pg.SetRoomCoverArt(ctx, roomID, *req.CoverArt); err != nil {
			log.Printf("admin update cover art: %v", err)
			http.Error(w, "failed to update cover art", http.StatusInternalServerError)
			return
		}
	}
	if req.CoverGradient != nil {
		if err := h.pg.SetRoomCoverGradient(ctx, roomID, *req.CoverGradient); err != nil {
			log.Printf("admin update cover gradient: %v", err)
			http.Error(w, "failed to update cover gradient", http.StatusInternalServerError)
			return
		}
	}

	room, err := h.pg.GetRoomByID(ctx, roomID)
	if err != nil || room == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}
	writeJSON(w, http.StatusOK, room)
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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

// PATCH /api/admin/autoplay/rooms/{id}/live/snippets — update info snippets
// on the LIVE playlist in place. Body: {"snippets": ["...", "...", ...]}.
// The snippets array is positional — index N maps to track N in the live
// playlist. Send "" to clear a snippet. Tracks past the array length are
// left alone. If the snippet for the currently playing track changed, the
// tracks row is patched and a track_info_updated WS event is broadcast so
// listeners see it immediately.
func (h *AdminHandler) UpdateLiveSnippets(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
		return
	}
	roomID := chi.URLParam(r, "id")
	ctx := r.Context()

	var req struct {
		Snippets []string `json:"snippets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	playlist, err := h.pg.GetAutoplayPlaylist(ctx, roomID, "live")
	if err != nil || playlist == nil {
		http.Error(w, "no live playlist", http.StatusNotFound)
		return
	}

	// Apply snippet edits in place. Only touch indices we actually received.
	for i := range playlist.Tracks {
		if i >= len(req.Snippets) {
			break
		}
		playlist.Tracks[i].InfoSnippet = req.Snippets[i]
	}

	if err := h.pg.SaveAutoplayPlaylist(ctx, playlist); err != nil {
		log.Printf("update live snippets: %v", err)
		http.Error(w, "failed to save", http.StatusInternalServerError)
		return
	}

	// If the currently playing track is one we just edited, patch the
	// tracks-table row and tell live listeners about it.
	nowPlaying, _ := h.pg.GetNowPlaying(ctx, roomID)
	if nowPlaying != nil {
		if idx, ok := parseAutoplayTrackIndex(nowPlaying.ID); ok && idx < len(playlist.Tracks) {
			newSnippet := playlist.Tracks[idx].InfoSnippet
			if newSnippet != nowPlaying.InfoSnippet {
				_ = h.pg.UpdateTrackInfoSnippet(ctx, nowPlaying.ID, newSnippet)
				if hub := h.hubs.Get(roomID); hub != nil {
					hub.BroadcastJSON(ws.WSMessage{
						Event: ws.EventTrackInfoUpdate,
						Payload: map[string]string{
							"id":          nowPlaying.ID,
							"infoSnippet": newSnippet,
						},
					})
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, playlist)
}

// PUT /api/admin/autoplay/rooms/{id}/live/tracks — replace the live playlist
// tracks in place. Body: {"tracks": [...AutoplayTrack]}.
//
// This is the in-place editing path used by the admin "Edit Live Tracks"
// panel: add a song, remove a song, or swap a track's source URL while the
// room is on-air. The currently-playing track keeps playing the audio it
// already loaded — we never yank a listener mid-track. The current_index
// is reconciled against the now-playing track's source URL so that
// auto-advance picks the right next track when the current one ends.
//
// Reconciliation rules:
//   - If the now-playing track's source URL matches a track in the new list,
//     current_index is set to (matchIdx + 1) % len so the next advance
//     picks the track that follows it.
//   - If no match (the playing track was removed or its URL replaced),
//     current_index is clamped/wrapped to a valid bound. The current play
//     finishes and the next advance picks whatever lives there now.
//   - If the new list is empty, we 400 — admins should use Stop instead.
func (h *AdminHandler) UpdateLiveTracks(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
		return
	}
	roomID := chi.URLParam(r, "id")
	ctx := r.Context()

	var req struct {
		Tracks []models.AutoplayTrack `json:"tracks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Tracks) == 0 {
		http.Error(w, "tracks cannot be empty — use the stop endpoint to take a room offline", http.StatusBadRequest)
		return
	}

	playlist, err := h.pg.GetAutoplayPlaylist(ctx, roomID, "live")
	if err != nil || playlist == nil {
		http.Error(w, "no live playlist", http.StatusNotFound)
		return
	}

	// Reconcile current_index against the now-playing track's source URL.
	// current_index stores the CURRENTLY-PLAYING index (not "next"), so we
	// want to point at the matched row — the next advance will move to the
	// following track. If the playing track was removed from the new list,
	// clamp to something in range so the next advance still picks a valid
	// track.
	nowPlaying, _ := h.pg.GetNowPlaying(ctx, roomID)
	newIndex := playlist.CurrentIndex
	if nowPlaying != nil && nowPlaying.SourceURL != "" {
		matchIdx := -1
		for i, t := range req.Tracks {
			if t.SourceURL == nowPlaying.SourceURL {
				matchIdx = i
				break
			}
		}
		if matchIdx >= 0 {
			newIndex = matchIdx
		}
	}
	// Clamp into [-1, len). -1 is the "nothing played yet" sentinel and is
	// left alone; otherwise wrap into [0, len).
	if newIndex >= len(req.Tracks) {
		newIndex = len(req.Tracks) - 1
	}
	if newIndex < -1 {
		newIndex = -1
	}

	playlist.Tracks = req.Tracks
	playlist.CurrentIndex = newIndex
	if err := h.pg.SaveAutoplayPlaylist(ctx, playlist); err != nil {
		log.Printf("update live tracks: %v", err)
		http.Error(w, "failed to save", http.StatusInternalServerError)
		return
	}

	// If the currently-playing track is still in the new list, propagate any
	// metadata edits (title/artist/snippet) to the live tracks row so the
	// listener UI updates without a track change.
	if nowPlaying != nil && nowPlaying.SourceURL != "" {
		for _, t := range req.Tracks {
			if t.SourceURL != nowPlaying.SourceURL {
				continue
			}
			if t.InfoSnippet != nowPlaying.InfoSnippet {
				_ = h.pg.UpdateTrackInfoSnippet(ctx, nowPlaying.ID, t.InfoSnippet)
				if hub := h.hubs.Get(roomID); hub != nil {
					hub.BroadcastJSON(ws.WSMessage{
						Event: ws.EventTrackInfoUpdate,
						Payload: map[string]string{
							"id":          nowPlaying.ID,
							"infoSnippet": t.InfoSnippet,
						},
					})
				}
			}
			break
		}
	}

	writeJSON(w, http.StatusOK, playlist)
}

// parseAutoplayTrackIndex extracts the playlist index encoded in autoplay
// track IDs of the form "auto-{roomIDPrefix}-{idx}-{timestamp}". Returns
// (0,false) for any other ID shape.
func parseAutoplayTrackIndex(trackID string) (int, bool) {
	if !strings.HasPrefix(trackID, "auto-") {
		return 0, false
	}
	parts := strings.Split(trackID, "-")
	if len(parts) < 4 {
		return 0, false
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil || idx < 0 {
		return 0, false
	}
	return idx, true
}

// POST /api/admin/autoplay/rooms/{id}/activate — promote staged to live
func (h *AdminHandler) ActivatePlaylist(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
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

	// Clear any stale state from the previous playlist so the fresh one
	// starts cleanly at track 0. Without this, StartAutoplayRooms sees the
	// old playback state in Redis and resumes mid-track instead of advancing
	// to the new playlist's first track — cause of the "started a few songs
	// down" issue reported 2026-04-22.
	h.playback.CancelAdvance(roomID)
	h.redis.ClearPlaybackState(ctx, roomID)
	h.pg.ClearNowPlaying(ctx, roomID)

	// Start playing the first track
	h.playback.StartAutoplayRooms(ctx)

	writeJSON(w, http.StatusOK, playlist)
}

// DELETE /api/admin/autoplay/rooms/{id}/staged — delete the staged playlist
func (h *AdminHandler) DeleteStagedPlaylist(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
		return
	}
	roomID := chi.URLParam(r, "id")
	h.pg.DeleteAutoplayPlaylist(r.Context(), roomID, "staged")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/admin/autoplay/rooms/{id}/start — relaunch a stopped autoplay
// room using its existing live playlist. No-op (with 200) if the room is
// already running.
func (h *AdminHandler) StartAutoplayRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
		return
	}
	roomID := chi.URLParam(r, "id")
	ctx := r.Context()

	playlist, err := h.pg.GetAutoplayPlaylist(ctx, roomID, "live")
	if err != nil {
		log.Printf("start autoplay room: load live playlist: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if playlist == nil || len(playlist.Tracks) == 0 {
		http.Error(w, "no live playlist with tracks — build a staged playlist and activate it first", http.StatusBadRequest)
		return
	}

	if err := h.pg.SetRoomAutoplay(ctx, roomID, true); err != nil {
		log.Printf("start autoplay room: set autoplay: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.playback.StartAutoplayRooms(ctx)

	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

// POST /api/admin/autoplay/rooms/{id}/stop — stop an autoplay room
func (h *AdminHandler) StopAutoplayRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
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
	if h.requireAdmin(w, r) == nil {
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
