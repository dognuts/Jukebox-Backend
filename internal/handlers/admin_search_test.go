package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jukebox/backend/internal/middleware"
	"github.com/jukebox/backend/internal/models"
	"github.com/jukebox/backend/internal/youtube"
)

// fakeSearcher lets us inject canned responses into the handler.
type fakeSearcher struct {
	result *youtube.SearchResult
	err    error
	lastQ  string
}

func (f *fakeSearcher) SearchTrack(ctx context.Context, q string) (*youtube.SearchResult, error) {
	f.lastQ = q
	return f.result, f.err
}

func newSearchHandler(searcher youtubeSearcher) *AdminHandler {
	return &AdminHandler{yt: searcher}
}

func requestWithUser(user *models.User, query string) *http.Request {
	r := httptest.NewRequest("GET", "/api/admin/search-track?q="+query, nil)
	if user != nil {
		r = r.WithContext(context.WithValue(r.Context(), middleware.UserKey, user))
	}
	return r
}

func TestSearchTrack_RequiresAdmin(t *testing.T) {
	h := newSearchHandler(&fakeSearcher{})
	w := httptest.NewRecorder()

	// No user at all
	h.SearchTrack(w, requestWithUser(nil, "apache"))
	if w.Code != http.StatusForbidden {
		t.Errorf("no user: status = %d, want 403", w.Code)
	}

	// Logged-in but non-admin
	w = httptest.NewRecorder()
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: false}, "apache"))
	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin: status = %d, want 403", w.Code)
	}
}

func TestSearchTrack_EmptyQueryReturns400(t *testing.T) {
	h := newSearchHandler(&fakeSearcher{})
	w := httptest.NewRecorder()
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: true}, ""))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSearchTrack_TooLongQueryReturns400(t *testing.T) {
	h := newSearchHandler(&fakeSearcher{})
	w := httptest.NewRecorder()
	longQ := ""
	for i := 0; i < 250; i++ {
		longQ += "a"
	}
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: true}, longQ))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for query >200 chars", w.Code)
	}
}

func TestSearchTrack_NotConfiguredReturns503(t *testing.T) {
	h := newSearchHandler(nil) // no searcher configured
	w := httptest.NewRecorder()
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: true}, "apache"))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when searcher not configured", w.Code)
	}
}

// TestSearchTrack_TypedNilYTClient_ViaNewAdminHandler_Returns503 exercises
// the real production construction path: main.go passes a nil *youtube.Client
// to NewAdminHandler when YOUTUBE_DATA_API_KEY is unset. This previously
// panicked (500) because a typed-nil pointer stored in an interface field is
// not == nil — the handler's nil check missed it and dereferenced the pointer.
func TestSearchTrack_TypedNilYTClient_ViaNewAdminHandler_Returns503(t *testing.T) {
	var nilClient *youtube.Client // typed nil, simulates unconfigured env
	h := NewAdminHandler(nil, nil, nil, nil, nilClient)
	w := httptest.NewRecorder()
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: true}, "apache"))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (typed-nil-in-interface regression guard)", w.Code)
	}
}

func TestSearchTrack_NoResultsReturns204(t *testing.T) {
	h := newSearchHandler(&fakeSearcher{err: youtube.ErrNoResults})
	w := httptest.NewRecorder()
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: true}, "x$$$@@"))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestSearchTrack_QuotaExhaustedReturns429(t *testing.T) {
	h := newSearchHandler(&fakeSearcher{err: youtube.ErrQuotaExhausted})
	w := httptest.NewRecorder()
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: true}, "apache"))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 on quota exhausted", w.Code)
	}
}

func TestSearchTrack_UpstreamErrorReturns502(t *testing.T) {
	h := newSearchHandler(&fakeSearcher{err: errors.New("network timeout")})
	w := httptest.NewRecorder()
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: true}, "apache"))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on generic upstream error", w.Code)
	}
}

func TestSearchTrack_HappyPathReturns200WithResult(t *testing.T) {
	result := &youtube.SearchResult{
		Primary: youtube.Candidate{
			Title:     "Apache",
			Artist:    "Incredible Bongo Band",
			Duration:  284,
			Source:    "youtube",
			SourceURL: "https://www.youtube.com/watch?v=id1",
			Thumbnail: "https://img/id1.jpg",
			Channel:   "Dusty Fingers",
		},
		Alternatives: []youtube.Candidate{
			{SourceURL: "https://www.youtube.com/watch?v=id2"},
		},
	}
	fake := &fakeSearcher{result: result}
	h := newSearchHandler(fake)
	w := httptest.NewRecorder()
	h.SearchTrack(w, requestWithUser(&models.User{IsAdmin: true}, "apache+incredible+bongo+band"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Verify the query got to the searcher (URL-decoded)
	if fake.lastQ != "apache incredible bongo band" {
		t.Errorf("searcher received %q, want URL-decoded 'apache incredible bongo band'", fake.lastQ)
	}

	var got youtube.SearchResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v\nbody: %s", err, w.Body.String())
	}
	if got.Primary.SourceURL != result.Primary.SourceURL {
		t.Errorf("Primary.SourceURL = %q, want %q", got.Primary.SourceURL, result.Primary.SourceURL)
	}
	if len(got.Alternatives) != 1 {
		t.Errorf("len(Alternatives) = %d, want 1", len(got.Alternatives))
	}
}
