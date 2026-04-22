package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubServer mounts mock handlers for the search.list and videos.list endpoints.
// Pass nil for an endpoint that should not be called in the test.
func stubServer(t *testing.T, search http.HandlerFunc, videos http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if search != nil {
		mux.HandleFunc("/search", search)
	}
	if videos != nil {
		mux.HandleFunc("/videos", videos)
	}
	return httptest.NewServer(mux)
}

func jsonResponse(t *testing.T, w http.ResponseWriter, status int, body interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func TestSearchTrack_HappyPath_ReturnsPrimaryAndAlternatives(t *testing.T) {
	srv := stubServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(t, w, 200, map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"id": map[string]string{"videoId": "id1"},
						"snippet": map[string]interface{}{
							"title":        "Incredible Bongo Band - Apache",
							"channelTitle": "Dusty Fingers",
							"thumbnails": map[string]interface{}{
								"medium": map[string]string{"url": "https://img/id1.jpg"},
							},
						},
					},
					{
						"id": map[string]string{"videoId": "id2"},
						"snippet": map[string]interface{}{
							"title":        "Apache (Live 1973)",
							"channelTitle": "Rare Funk",
							"thumbnails": map[string]interface{}{
								"medium": map[string]string{"url": "https://img/id2.jpg"},
							},
						},
					},
				},
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(t, w, 200, map[string]interface{}{
				"items": []map[string]interface{}{
					{"id": "id1", "contentDetails": map[string]string{"duration": "PT4M44S"}},
					{"id": "id2", "contentDetails": map[string]string{"duration": "PT6M30S"}},
				},
			})
		},
	)
	defer srv.Close()

	c := NewClient("fake-key")
	c.baseURL = srv.URL
	res, err := c.SearchTrack(context.Background(), "apache incredible bongo band")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Primary.Title != "Apache" {
		t.Errorf("Primary.Title = %q, want 'Apache' (split from 'Artist - Title')", res.Primary.Title)
	}
	if res.Primary.Artist != "Incredible Bongo Band" {
		t.Errorf("Primary.Artist = %q, want 'Incredible Bongo Band'", res.Primary.Artist)
	}
	if res.Primary.Duration != 284 {
		t.Errorf("Primary.Duration = %d, want 284 (4m44s)", res.Primary.Duration)
	}
	if res.Primary.Source != "youtube" {
		t.Errorf("Primary.Source = %q, want 'youtube'", res.Primary.Source)
	}
	if res.Primary.SourceURL != "https://www.youtube.com/watch?v=id1" {
		t.Errorf("Primary.SourceURL = %q, want canonical YouTube watch URL", res.Primary.SourceURL)
	}
	if res.Primary.Thumbnail != "https://img/id1.jpg" {
		t.Errorf("Primary.Thumbnail = %q, want medium thumbnail URL", res.Primary.Thumbnail)
	}
	if res.Primary.Channel != "Dusty Fingers" {
		t.Errorf("Primary.Channel = %q, want 'Dusty Fingers'", res.Primary.Channel)
	}

	if len(res.Alternatives) != 1 {
		t.Fatalf("len(Alternatives) = %d, want 1 (one extra item beyond primary)", len(res.Alternatives))
	}
	if res.Alternatives[0].SourceURL != "https://www.youtube.com/watch?v=id2" {
		t.Errorf("Alternatives[0].SourceURL = %q, want id2 URL", res.Alternatives[0].SourceURL)
	}
	if res.Alternatives[0].Duration != 390 {
		t.Errorf("Alternatives[0].Duration = %d, want 390 (6m30s)", res.Alternatives[0].Duration)
	}
}

func TestSearchTrack_TitleWithoutSeparator_UsesChannelAsArtist(t *testing.T) {
	srv := stubServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(t, w, 200, map[string]interface{}{
				"items": []map[string]interface{}{
					{
						"id": map[string]string{"videoId": "id1"},
						"snippet": map[string]interface{}{
							"title":        "Donuts full album",
							"channelTitle": "J Dilla",
							"thumbnails": map[string]interface{}{
								"medium": map[string]string{"url": "https://img/id1.jpg"},
							},
						},
					},
				},
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(t, w, 200, map[string]interface{}{
				"items": []map[string]interface{}{
					{"id": "id1", "contentDetails": map[string]string{"duration": "PT45M"}},
				},
			})
		},
	)
	defer srv.Close()

	c := NewClient("fake-key")
	c.baseURL = srv.URL
	res, err := c.SearchTrack(context.Background(), "j dilla donuts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Primary.Title != "Donuts full album" {
		t.Errorf("Primary.Title = %q, want full snippet title when no separator", res.Primary.Title)
	}
	if res.Primary.Artist != "J Dilla" {
		t.Errorf("Primary.Artist = %q, want channel title as fallback artist", res.Primary.Artist)
	}
	if res.Primary.Duration != 45*60 {
		t.Errorf("Primary.Duration = %d, want 2700 (45m)", res.Primary.Duration)
	}
}

func TestSearchTrack_NoResults_ReturnsErrNoResults(t *testing.T) {
	srv := stubServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(t, w, 200, map[string]interface{}{
				"items": []map[string]interface{}{},
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("videos endpoint should not be called when search is empty")
		},
	)
	defer srv.Close()

	c := NewClient("fake-key")
	c.baseURL = srv.URL
	_, err := c.SearchTrack(context.Background(), "bg)#$ no results here")
	if !errors.Is(err, ErrNoResults) {
		t.Errorf("got err=%v, want ErrNoResults", err)
	}
}

func TestSearchTrack_QuotaExhausted_ReturnsErrQuotaExhausted(t *testing.T) {
	srv := stubServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			// Real YouTube quota error body shape
			jsonResponse(t, w, 403, map[string]interface{}{
				"error": map[string]interface{}{
					"code":    403,
					"message": "The request cannot be completed because you have exceeded your quota.",
					"errors": []map[string]string{
						{"reason": "quotaExceeded", "domain": "youtube.quota"},
					},
				},
			})
		},
		nil,
	)
	defer srv.Close()

	c := NewClient("fake-key")
	c.baseURL = srv.URL
	_, err := c.SearchTrack(context.Background(), "any query")
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Errorf("got err=%v, want ErrQuotaExhausted", err)
	}
}

func TestSearchTrack_UpstreamServerError_ReturnsWrappedError(t *testing.T) {
	srv := stubServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "upstream boom", 500)
		},
		nil,
	)
	defer srv.Close()

	c := NewClient("fake-key")
	c.baseURL = srv.URL
	_, err := c.SearchTrack(context.Background(), "query")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNoResults) || errors.Is(err, ErrQuotaExhausted) {
		t.Errorf("err should be upstream-generic, got sentinel: %v", err)
	}
	// Error message should mention status 500 so operators can debug
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should include status 500, got: %v", err)
	}
}

func TestSearchTrack_EmptyQuery_ReturnsError(t *testing.T) {
	c := NewClient("fake-key")
	_, err := c.SearchTrack(context.Background(), "   ")
	if err == nil {
		t.Error("expected error on empty query, got nil")
	}
}

func TestParseISO8601Duration(t *testing.T) {
	tests := []struct {
		in   string
		want int
		desc string
	}{
		{"PT4M44S", 284, "minutes and seconds"},
		{"PT45M", 45 * 60, "minutes only"},
		{"PT30S", 30, "seconds only"},
		{"PT1H2M3S", 3723, "hours minutes seconds"},
		{"PT2H", 7200, "hours only"},
		{"P0D", 0, "zero duration"},
		{"", 0, "empty string"},
		{"garbage", 0, "unparseable returns 0"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := parseISO8601Duration(tt.in)
			if got != tt.want {
				t.Errorf("parseISO8601Duration(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
