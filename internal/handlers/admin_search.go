package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jukebox/backend/internal/youtube"
)

// youtubeSearcher is the interface AdminHandler needs to do bulk track search.
// Defining it here (instead of exporting from the youtube package) keeps the
// dependency narrow and makes the handler easy to test with a fake.
type youtubeSearcher interface {
	SearchTrack(ctx context.Context, query string) (*youtube.SearchResult, error)
}

const maxSearchQueryLen = 200

// SearchTrack handles GET /api/admin/search-track?q=<query>.
// Returns the YouTube Data API top match plus up to 4 alternatives.
func (h *AdminHandler) SearchTrack(w http.ResponseWriter, r *http.Request) {
	if h.requireAdmin(w, r) == nil {
		return
	}
	if h.yt == nil {
		http.Error(w, "bulk search not configured (set YOUTUBE_DATA_API_KEY)", http.StatusServiceUnavailable)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "query required", http.StatusBadRequest)
		return
	}
	if len(q) > maxSearchQueryLen {
		http.Error(w, "query too long", http.StatusBadRequest)
		return
	}

	result, err := h.yt.SearchTrack(r.Context(), q)
	if err != nil {
		switch {
		case errors.Is(err, youtube.ErrNoResults):
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, youtube.ErrQuotaExhausted):
			http.Error(w, "youtube quota exhausted", http.StatusTooManyRequests)
		default:
			http.Error(w, "upstream youtube error", http.StatusBadGateway)
		}
		return
	}

	writeJSON(w, http.StatusOK, result)
}
