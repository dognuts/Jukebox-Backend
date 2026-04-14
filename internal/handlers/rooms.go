package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/playback"
	"github.com/jukebox/backend/internal/store"
	"github.com/jukebox/backend/internal/ws"
)

type RoomHandler struct {
	pg       *store.PGStore
	redis    *store.RedisStore
	hubs     *ws.HubManager
	playback *playback.SyncService
}

func NewRoomHandler(pg *store.PGStore, redis *store.RedisStore, hubs *ws.HubManager, pb *playback.SyncService) *RoomHandler {
	return &RoomHandler{pg: pg, redis: redis, hubs: hubs, playback: pb}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func toSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "room"
	}
	return s
}

// POST /api/rooms
func (h *RoomHandler) Create(w http.ResponseWriter, r *http.Request) {
	session := middleware.GetSession(r.Context())
	if session == nil {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}

	// Require a logged-in user account to create a jukebox
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "you must be logged in to create a jukebox", http.StatusUnauthorized)
		return
	}

	var req models.CreateRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// Generate DJ key
	djKey, djKeyHash, err := middleware.GenerateDJKey()
	if err != nil {
		http.Error(w, "failed to generate DJ key", http.StatusInternalServerError)
		return
	}

	// Parse vibes (max 3), ensure non-nil for Postgres TEXT[]
	vibes := req.Vibes
	if vibes == nil {
		vibes = []string{}
	}
	if len(vibes) > 3 {
		vibes = vibes[:3]
	}

	// Generate slug with uniqueness suffix
	baseSlug := toSlug(req.Name)
	slug := baseSlug + "-" + uuid.New().String()[:8]

	var scheduledStart *time.Time
	if req.ScheduledStart != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduledStart)
		if err == nil {
			scheduledStart = &t
		}
	}

	// Determine DJ display name: prefer user's stage_name if logged in
	djName := session.DisplayName
	if user.StageName != "" {
		djName = user.StageName
	} else if user.DisplayName != "" {
		djName = user.DisplayName
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
		IsOfficial:     false,
		DJKeyHash:      djKeyHash,
		DJSessionID:    session.ID,
		DJDisplayName:  djName,
		DJAvatarColor:  session.AvatarColor,
		CreatorUserID:  user.ID,
		CreatedAt:      time.Now(),
		ScheduledStart: scheduledStart,
	}

	if room.RequestPolicy == "" {
		room.RequestPolicy = models.RequestPolicyOpen
	}

	if err := h.pg.CreateRoom(r.Context(), room); err != nil {
		log.Printf("create room: %v", err)
		http.Error(w, "failed to create room", http.StatusInternalServerError)
		return
	}

	// Pre-load queue from a user tracklist if provided
	if req.PlaylistID != "" && user != nil {
		log.Printf("[room] attempting to pre-load playlist %s for room %s", req.PlaylistID, room.Slug)
		pl, err := h.pg.GetPlaylistWithTracks(r.Context(), req.PlaylistID)
		if err != nil {
			log.Printf("[room] failed to fetch playlist %s: %v", req.PlaylistID, err)
		} else if pl == nil {
			log.Printf("[room] playlist %s not found", req.PlaylistID)
		} else if pl.UserID != user.ID {
			log.Printf("[room] playlist %s belongs to %s, not %s", req.PlaylistID, pl.UserID, user.ID)
		} else if len(pl.Tracks) == 0 {
			log.Printf("[room] playlist %s has no tracks", req.PlaylistID)
		} else {
			loaded := 0
			for i, pt := range pl.Tracks {
				// Ensure the track exists in the tracks table (it should, but be safe)
				track := &models.Track{
					ID:            pt.TrackID,
					Title:         pt.Title,
					Artist:        pt.Artist,
					Duration:      pt.Duration,
					Source:        models.TrackSource(pt.Source),
					SourceURL:     pt.SourceUrl,
					AlbumGradient: pt.AlbumGradient,
					CreatedAt:     time.Now(),
				}
				if err := h.pg.UpsertTrack(r.Context(), track); err != nil {
					log.Printf("[room] pre-load: failed to upsert track %d (%s): %v", i, pt.TrackID, err)
					continue
				}

				entry := &models.QueueEntry{
					ID:          uuid.New().String(),
					RoomID:      room.ID,
					Track:       *track,
					SubmittedBy: djName,
					SessionID:   session.ID,
					Status:      models.QueueApproved,
					CreatedAt:   time.Now(),
				}
				if err := h.pg.AddToQueue(r.Context(), entry); err != nil {
					log.Printf("[room] pre-load: failed to queue track %d (%s - %s): %v", i, pt.Artist, pt.Title, err)
					continue
				}
				loaded++
			}
			log.Printf("[room] pre-loaded %d/%d tracks from playlist '%s' into room %s", loaded, len(pl.Tracks), pl.Name, room.Slug)
		}
	}

	writeJSON(w, http.StatusCreated, models.CreateRoomResponse{
		Room:  *room,
		DJKey: djKey,
	})
}

// GET /api/rooms
func (h *RoomHandler) List(w http.ResponseWriter, r *http.Request) {
	liveOnly := r.URL.Query().Get("live") == "true"
	genre := r.URL.Query().Get("genre")

	rooms, err := h.pg.ListRooms(r.Context(), liveOnly, genre)
	if err != nil {
		log.Printf("list rooms: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Enrich with listener counts and now-playing track from Redis
	ctx := r.Context()
	type RoomWithNowPlaying struct {
		models.Room
		NowPlaying *models.Track         `json:"nowPlaying,omitempty"`
		RecentChat []models.ChatMessage  `json:"recentChat,omitempty"`
	}

	// Batch Redis reads: one pipeline for listener counts across all rooms,
	// one pipeline for playback state across live rooms. Reduces what used
	// to be O(2n) sequential round-trips to two.
	allIDs := make([]string, len(rooms))
	liveIDs := make([]string, 0, len(rooms))
	for i := range rooms {
		allIDs[i] = rooms[i].ID
		if rooms[i].IsLive {
			liveIDs = append(liveIDs, rooms[i].ID)
		}
	}
	counts := h.redis.GetListenerCounts(ctx, allIDs)
	playbacks := h.redis.GetPlaybackStates(ctx, liveIDs)

	result := make([]RoomWithNowPlaying, len(rooms))
	for i := range rooms {
		rooms[i].ListenerCount = int(counts[rooms[i].ID])
		result[i] = RoomWithNowPlaying{Room: rooms[i]}

		if rooms[i].IsLive {
			if ps := playbacks[rooms[i].ID]; ps != nil && ps.TrackID != "" {
				track, _ := h.pg.GetTrack(ctx, ps.TrackID)
				if track != nil {
					result[i].NowPlaying = track
				}
			}
		}
	}

	if result == nil {
		result = []RoomWithNowPlaying{}
	}

	// Attach a small chat preview to the featured room so the homepage
	// can render the featured card at final size in one pass. Mirrors
	// the frontend's featured-picking logic: IsFeatured flag wins,
	// otherwise the live room with the most listeners.
	featuredIdx := -1
	for i := range result {
		if result[i].IsLive && result[i].IsFeatured {
			featuredIdx = i
			break
		}
	}
	if featuredIdx < 0 {
		var best int
		for i := range result {
			if !result[i].IsLive {
				continue
			}
			if featuredIdx < 0 || result[i].ListenerCount > best {
				featuredIdx = i
				best = result[i].ListenerCount
			}
		}
	}
	if featuredIdx >= 0 {
		if chat, _ := h.pg.GetRecentChat(ctx, result[featuredIdx].ID, 3); chat != nil {
			result[featuredIdx].RecentChat = chat
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// GET /api/rooms/{slug}
func (h *RoomHandler) Get(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()

	room, err := h.pg.GetRoomBySlug(ctx, slug)
	if err != nil {
		log.Printf("get room: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// Enrich
	count, _ := h.redis.GetListenerCount(ctx, room.ID)
	room.ListenerCount = int(count)

	nowPlaying, _ := h.pg.GetNowPlaying(ctx, room.ID)
	queue, _ := h.pg.GetQueue(ctx, room.ID)
	chat, _ := h.pg.GetRecentChat(ctx, room.ID, 50)

	ps, _ := h.redis.GetPlaybackState(ctx, room.ID)
	if ps == nil {
		ps = &models.PlaybackState{RoomID: room.ID}
	}

	if queue == nil {
		queue = []models.QueueEntry{}
	}
	if chat == nil {
		chat = []models.ChatMessage{}
	}

	writeJSON(w, http.StatusOK, models.RoomDetailResponse{
		Room:          *room,
		NowPlaying:    nowPlaying,
		Queue:         queue,
		RecentChat:    chat,
		PlaybackState: *ps,
	})
}

// POST /api/rooms/{slug}/go-live
func (h *RoomHandler) GoLive(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()
	djKey := middleware.ExtractDJKey(r)

	room, err := h.pg.GetRoomBySlug(ctx, slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	if !middleware.VerifyDJKey(djKey, room.DJKeyHash) {
		http.Error(w, "invalid DJ key", http.StatusForbidden)
		return
	}

	var req models.GoLiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Create the initial track
	track := &models.Track{
		ID:        uuid.New().String(),
		Title:     req.TrackTitle,
		Artist:    req.TrackArtist,
		Duration:  req.TrackDuration,
		Source:    models.TrackSource(req.TrackSource),
		SourceURL: req.TrackSourceURL,
		CreatedAt: time.Now(),
	}
	h.pg.UpsertTrack(ctx, track)
	h.pg.SetNowPlaying(ctx, room.ID, track.ID)

	// Set playback state
	ps := &models.PlaybackState{
		RoomID:        room.ID,
		TrackID:       track.ID,
		StartedAtUnix: time.Now().UnixMilli(),
		IsPlaying:     true,
	}
	h.redis.SetPlaybackState(ctx, ps)

	// Mark room as live
	h.pg.SetRoomLive(ctx, room.ID, true)

	// Schedule auto-advance
	h.playback.ScheduleAdvance(room.ID, track, ps.StartedAtUnix)

	// Broadcast to WebSocket clients if hub exists
	if hub := h.hubs.Get(room.ID); hub != nil {
		hub.BroadcastJSON(ws.WSMessage{Event: ws.EventTrackChanged, Payload: track})
		hub.BroadcastJSON(ws.WSMessage{Event: ws.EventPlaybackState, Payload: ps})
		queue, _ := h.pg.GetQueue(ctx, room.ID)
		hub.BroadcastJSON(ws.WSMessage{Event: ws.EventQueueUpdate, Payload: queue})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "live",
		"playbackState": ps,
		"track":         track,
	})
}

// POST /api/rooms/{slug}/end
func (h *RoomHandler) EndSession(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	ctx := r.Context()
	djKey := middleware.ExtractDJKey(r)

	room, err := h.pg.GetRoomBySlug(ctx, slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	if !middleware.VerifyDJKey(djKey, room.DJKeyHash) {
		http.Error(w, "invalid DJ key", http.StatusForbidden)
		return
	}

	h.endRoom(ctx, room)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ended"})
}

// endRoom performs all cleanup for ending a room — used by both manual end and auto-close.
func (h *RoomHandler) endRoom(ctx context.Context, room *models.Room) {
	h.pg.EndRoom(ctx, room.ID)
	h.pg.ClearNowPlaying(ctx, room.ID)
	h.redis.ClearPlaybackState(ctx, room.ID)
	h.redis.ClearListeners(ctx, room.ID)
	h.playback.CancelAdvance(room.ID)

	// Broadcast room_ended to all connected clients
	if hub := h.hubs.Get(room.ID); hub != nil {
		hub.BroadcastJSON(ws.WSMessage{
			Event:   "room_ended",
			Payload: map[string]string{"reason": "DJ ended the session"},
		})
	}
}

// GET /api/rooms/{slug}/history — played tracks for a room
func (h *RoomHandler) GetHistory(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	room, err := h.pg.GetRoomBySlug(r.Context(), slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	played, err := h.pg.GetPlayedTracks(r.Context(), room.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if played == nil {
		played = []models.QueueEntry{}
	}
	writeJSON(w, http.StatusOK, played)
}

// POST /api/rooms/{slug}/save-session — saves all played+queued tracks as a playlist
func (h *RoomHandler) SaveSession(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}

	slug := chi.URLParam(r, "slug")
	room, err := h.pg.GetRoomBySlug(r.Context(), slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// Get all session tracks (played + approved queue)
	allTracks, err := h.pg.GetAllSessionTracks(r.Context(), room.ID)
	if err != nil || len(allTracks) == 0 {
		http.Error(w, "no tracks to save", http.StatusBadRequest)
		return
	}

	// Also include the currently playing track if we can find it
	ps, _ := h.redis.GetPlaybackState(r.Context(), room.ID)
	var nowPlayingTrackID string
	if ps != nil && ps.TrackID != "" {
		nowPlayingTrackID = ps.TrackID
	}

	// Create playlist named after the room + DJ
	playlistName := room.Name + " — " + room.DJDisplayName
	pl := &models.Playlist{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Name:      playlistName,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := h.pg.CreatePlaylist(r.Context(), pl); err != nil {
		http.Error(w, "failed to create playlist", http.StatusInternalServerError)
		return
	}

	// Add now-playing track first if it's not already in the list
	added := map[string]bool{}
	if nowPlayingTrackID != "" {
		h.pg.AddTrackToPlaylist(r.Context(), &models.PlaylistTrack{
			ID: uuid.New().String(), TrackID: nowPlayingTrackID,
		}, pl.ID)
		added[nowPlayingTrackID] = true
	}

	// Add all session tracks
	for _, entry := range allTracks {
		if added[entry.Track.ID] {
			continue
		}
		h.pg.AddTrackToPlaylist(r.Context(), &models.PlaylistTrack{
			ID: uuid.New().String(), TrackID: entry.Track.ID,
		}, pl.ID)
		added[entry.Track.ID] = true
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"playlist":   pl,
		"trackCount": len(added),
	})
}

// GET /api/rooms/{slug}/autoplay-tracks — public endpoint for autoplay playlist
func (h *RoomHandler) GetAutoplayTracks(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	room, err := h.pg.GetRoomBySlug(r.Context(), slug)
	if err != nil || room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	if !room.IsAutoplay {
		writeJSON(w, http.StatusOK, map[string]interface{}{"tracks": []interface{}{}, "currentIndex": 0})
		return
	}
	playlist, err := h.pg.GetAutoplayPlaylist(r.Context(), room.ID, "live")
	if err != nil || playlist == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"tracks": []interface{}{}, "currentIndex": 0})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tracks":       playlist.Tracks,
		"currentIndex": playlist.CurrentIndex,
		"name":         playlist.Name,
	})
}

// Helper
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
