package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jukebox/backend/internal/models"
)

// fakeDJStore lets us exercise the handler without Postgres.
type fakeDJStore struct {
	user          *models.User
	liveRoom      *models.Room
	stats         models.DJStats
	shows         []models.DJRecentShow
	genre         string
	lastStageName string
}

func (f *fakeDJStore) GetUserByStageName(ctx context.Context, stageName string) (*models.User, error) {
	f.lastStageName = stageName
	return f.user, nil
}

func (f *fakeDJStore) GetLiveRoomByCreator(ctx context.Context, userID string) (*models.Room, error) {
	return f.liveRoom, nil
}

func (f *fakeDJStore) GetDJStats(ctx context.Context, userID string) (models.DJStats, error) {
	return f.stats, nil
}

func (f *fakeDJStore) GetDJRecentShows(ctx context.Context, userID string, limit int) ([]models.DJRecentShow, error) {
	return f.shows, nil
}

func (f *fakeDJStore) GetDJGenre(ctx context.Context, userID string) (string, error) {
	return f.genre, nil
}

// serveDJProfile routes the request through chi so chi.URLParam resolves,
// exactly as in main.go.
func serveDJProfile(t *testing.T, store djProfileStore, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/djs/{username}", NewDJHandler(store).GetProfile)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// TestDJProfileFound pins the frozen API contract for a fully-populated
// profile: every key present, correct casing, live state and stats mapped on.
func TestDJProfileFound(t *testing.T) {
	showDate := time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC)
	store := &fakeDJStore{
		user: &models.User{
			ID:          "u1",
			StageName:   "Neon Rider",
			DisplayName: "Charlie",
			Bio:         "late night funk",
			AvatarURL:   "https://cdn.example.com/a.png",
		},
		liveRoom: &models.Room{ID: "r1", Slug: "friday-funk-ab12cd34", IsLive: true},
		genre:    "funk",
		stats:    models.DJStats{TotalShows: 12, TotalListeners: 340},
		shows: []models.DJRecentShow{
			{Date: showDate, RoomName: "Friday Funk", TrackCount: 24},
		},
	}

	w := serveDJProfile(t, store, "/api/djs/neon%20rider")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got, want := w.Header().Get("Cache-Control"), "public, max-age=60"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if store.lastStageName != "neon rider" {
		t.Errorf("looked up stage name %q, want %q (URL-decoded)", store.lastStageName, "neon rider")
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}

	// Scalar fields per the contract.
	for key, want := range map[string]interface{}{
		"username":        "Neon Rider",
		"displayName":     "Charlie",
		"bio":             "late night funk",
		"avatarUrl":       "https://cdn.example.com/a.png",
		"isLive":          true,
		"currentRoomSlug": "friday-funk-ab12cd34",
		"genre":           "funk",
	} {
		if got := body[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}

	stats, ok := body["stats"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats = %#v, want object", body["stats"])
	}
	if stats["totalShows"] != float64(12) || stats["totalListeners"] != float64(340) {
		t.Errorf("stats = %#v, want totalShows=12 totalListeners=340", stats)
	}

	shows, ok := body["recentShows"].([]interface{})
	if !ok || len(shows) != 1 {
		t.Fatalf("recentShows = %#v, want array of 1", body["recentShows"])
	}
	show := shows[0].(map[string]interface{})
	if show["roomName"] != "Friday Funk" || show["trackCount"] != float64(24) {
		t.Errorf("recentShows[0] = %#v, want Friday Funk / 24 tracks", show)
	}
	if _, err := time.Parse(time.RFC3339, show["date"].(string)); err != nil {
		t.Errorf("recentShows[0].date = %#v, want ISO8601 string", show["date"])
	}
}

// TestDJProfileOffAirDefaults: a DJ with no live room, no rooms with genre,
// no history must serve nulls, zeros and [] — never fabricated values.
func TestDJProfileOffAirDefaults(t *testing.T) {
	store := &fakeDJStore{
		user: &models.User{ID: "u1", StageName: "Quiet DJ", DisplayName: "Q"},
	}

	w := serveDJProfile(t, store, "/api/djs/Quiet%20DJ")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}

	if body["isLive"] != false {
		t.Errorf("isLive = %#v, want false", body["isLive"])
	}
	for _, key := range []string{"bio", "avatarUrl", "currentRoomSlug", "genre"} {
		if got, present := body[key]; !present || got != nil {
			t.Errorf("%s = %#v (present=%v), want explicit null", key, got, present)
		}
	}
	if shows, ok := body["recentShows"].([]interface{}); !ok || len(shows) != 0 {
		t.Errorf("recentShows = %#v, want []", body["recentShows"])
	}
	stats, ok := body["stats"].(map[string]interface{})
	if !ok || stats["totalShows"] != float64(0) || stats["totalListeners"] != float64(0) {
		t.Errorf("stats = %#v, want zeros", body["stats"])
	}
}

// TestDJProfileNotFound pins the 404 contract: {"error":"not found"}.
func TestDJProfileNotFound(t *testing.T) {
	w := serveDJProfile(t, &fakeDJStore{user: nil}, "/api/djs/nobody")
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body["error"] != "not found" {
		t.Errorf(`body = %#v, want {"error":"not found"}`, body)
	}
}
