package handlers

import (
	"context"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jukebox/backend/internal/models"
)

// djProfileStore is the slice of the store the public DJ profile endpoint
// needs — an interface so the handler can be tested without Postgres.
// *store.PGStore satisfies it.
type djProfileStore interface {
	GetUserByStageName(ctx context.Context, stageName string) (*models.User, error)
	GetLiveRoomByCreator(ctx context.Context, userID string) (*models.Room, error)
	GetDJStats(ctx context.Context, userID string) (models.DJStats, error)
	GetDJRecentShows(ctx context.Context, userID string, limit int) ([]models.DJRecentShow, error)
	GetDJGenre(ctx context.Context, userID string) (string, error)
}

type DJHandler struct {
	store djProfileStore
}

func NewDJHandler(store djProfileStore) *DJHandler {
	return &DJHandler{store: store}
}

// nullableString maps "" to a nil pointer so contract fields typed
// string|null marshal as JSON null instead of "".
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GET /api/djs/{username} — public DJ profile, resolved by stage name
// (case-insensitive). 404s with {"error":"not found"} for unknown names.
func (h *DJHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	ctx := r.Context()

	user, err := h.store.GetUserByStageName(ctx, username)
	if err != nil {
		log.Printf("dj profile %q: user lookup: %v", username, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	profile := models.DJProfile{
		Username:    user.StageName,
		DisplayName: user.DisplayName,
		Bio:         nullableString(user.Bio),
		AvatarURL:   nullableString(user.AvatarURL),
		RecentShows: []models.DJRecentShow{},
	}

	// Everything past the user row is best-effort enrichment: a failed read
	// logs and degrades to the zero value (false/null/0/[]) instead of
	// failing the whole profile.
	if liveRoom, err := h.store.GetLiveRoomByCreator(ctx, user.ID); err != nil {
		log.Printf("dj profile %q: live room: %v", username, err)
	} else if liveRoom != nil {
		profile.IsLive = true
		profile.CurrentRoomSlug = nullableString(liveRoom.Slug)
	}

	if genre, err := h.store.GetDJGenre(ctx, user.ID); err != nil {
		log.Printf("dj profile %q: genre: %v", username, err)
	} else {
		profile.Genre = nullableString(genre)
	}

	if stats, err := h.store.GetDJStats(ctx, user.ID); err != nil {
		log.Printf("dj profile %q: stats: %v", username, err)
	} else {
		profile.Stats = stats
	}

	if shows, err := h.store.GetDJRecentShows(ctx, user.ID, 10); err != nil {
		log.Printf("dj profile %q: recent shows: %v", username, err)
	} else if shows != nil {
		profile.RecentShows = shows
	}

	// Public, non-personalized payload — let browsers/CDNs reuse it briefly.
	// Same Set-Cookie guard as setPublicCache (see that function's comment);
	// longer TTL because profiles change slowly.
	if len(w.Header().Values("Set-Cookie")) > 0 {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	writeJSON(w, http.StatusOK, profile)
}
