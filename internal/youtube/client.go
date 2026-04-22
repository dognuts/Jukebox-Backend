// Package youtube provides a minimal client for the YouTube Data API v3
// search.list + videos.list pair, wrapping the two calls into a single
// SearchTrack function that returns a primary match plus alternatives.
//
// Used by the admin bulk-track-search endpoint. See
// Jukebox-Frontend/docs/superpowers/specs/2026-04-22-admin-bulk-track-search-design.md
package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://www.googleapis.com/youtube/v3"

var (
	ErrNoResults      = errors.New("youtube: no results")
	ErrQuotaExhausted = errors.New("youtube: quota exhausted")
)

type Client struct {
	apiKey  string
	http    *http.Client
	baseURL string
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second},
		baseURL: defaultBaseURL,
	}
}

type Candidate struct {
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Duration  int    `json:"duration"`
	Source    string `json:"source"`
	SourceURL string `json:"sourceUrl"`
	Thumbnail string `json:"thumbnail"`
	Channel   string `json:"channel"`
}

type SearchResult struct {
	Primary      Candidate   `json:"primary"`
	Alternatives []Candidate `json:"alternatives"`
}

// Shape of the YouTube search.list response (fields we read).
type ytSearchResponse struct {
	Items []struct {
		ID      struct{ VideoID string `json:"videoId"` } `json:"id"`
		Snippet struct {
			Title        string `json:"title"`
			ChannelTitle string `json:"channelTitle"`
			Thumbnails   struct {
				Medium struct{ URL string `json:"url"` } `json:"medium"`
			} `json:"thumbnails"`
		} `json:"snippet"`
	} `json:"items"`
}

// Shape of the YouTube videos.list response (fields we read).
type ytVideosResponse struct {
	Items []struct {
		ID             string `json:"id"`
		ContentDetails struct {
			Duration string `json:"duration"`
		} `json:"contentDetails"`
	} `json:"items"`
}

type ytError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

// SearchTrack runs a YouTube search for query, fetches durations for each
// result, and returns a SearchResult with the top hit as Primary and up to
// 4 more as Alternatives.
func (c *Client) SearchTrack(ctx context.Context, query string) (*SearchResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("youtube: empty query")
	}

	searchResp, err := c.callSearch(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(searchResp.Items) == 0 {
		return nil, ErrNoResults
	}

	videoIDs := make([]string, 0, len(searchResp.Items))
	for _, it := range searchResp.Items {
		if it.ID.VideoID != "" {
			videoIDs = append(videoIDs, it.ID.VideoID)
		}
	}
	if len(videoIDs) == 0 {
		return nil, ErrNoResults
	}

	durations, err := c.callVideos(ctx, videoIDs)
	if err != nil {
		return nil, err
	}

	candidates := make([]Candidate, 0, len(searchResp.Items))
	for _, it := range searchResp.Items {
		if it.ID.VideoID == "" {
			continue
		}
		artist, title := splitTitleArtist(it.Snippet.Title, it.Snippet.ChannelTitle)
		candidates = append(candidates, Candidate{
			Title:     title,
			Artist:    artist,
			Duration:  durations[it.ID.VideoID],
			Source:    "youtube",
			SourceURL: "https://www.youtube.com/watch?v=" + it.ID.VideoID,
			Thumbnail: it.Snippet.Thumbnails.Medium.URL,
			Channel:   it.Snippet.ChannelTitle,
		})
	}

	return &SearchResult{
		Primary:      candidates[0],
		Alternatives: candidates[1:],
	}, nil
}

func (c *Client) callSearch(ctx context.Context, query string) (*ytSearchResponse, error) {
	u := c.baseURL + "/search?" + url.Values{
		"part":            {"snippet"},
		"type":            {"video"},
		"maxResults":      {"5"},
		"videoEmbeddable": {"true"},
		"q":               {query},
		"key":             {c.apiKey},
	}.Encode()

	body, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	var out ytSearchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("youtube: decode search: %w", err)
	}
	return &out, nil
}

func (c *Client) callVideos(ctx context.Context, ids []string) (map[string]int, error) {
	u := c.baseURL + "/videos?" + url.Values{
		"part": {"contentDetails"},
		"id":   {strings.Join(ids, ",")},
		"key":  {c.apiKey},
	}.Encode()

	body, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	var out ytVideosResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("youtube: decode videos: %w", err)
	}
	durations := make(map[string]int, len(out.Items))
	for _, it := range out.Items {
		durations[it.ID] = parseISO8601Duration(it.ContentDetails.Duration)
	}
	return durations, nil
}

// get issues a GET and classifies errors (quota exhausted vs generic upstream).
func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("youtube: request: %w", err)
	}
	defer resp.Body.Close()

	body := make([]byte, 0, 2048)
	buf := make([]byte, 2048)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}

	if resp.StatusCode == http.StatusForbidden {
		// Distinguish quota-exceeded from other 403s by inspecting reason codes
		var ye ytError
		if json.Unmarshal(body, &ye) == nil {
			for _, e := range ye.Error.Errors {
				if e.Reason == "quotaExceeded" || e.Reason == "rateLimitExceeded" {
					return nil, ErrQuotaExhausted
				}
			}
		}
		return nil, fmt.Errorf("youtube: status %d: %s", resp.StatusCode, string(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("youtube: status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// splitTitleArtist applies the "Artist - Title" convention when a separator
// is present in the video title; otherwise falls back to channelTitle as artist.
func splitTitleArtist(snippetTitle, channelTitle string) (artist, title string) {
	if i := strings.Index(snippetTitle, " - "); i >= 0 {
		return strings.TrimSpace(snippetTitle[:i]), strings.TrimSpace(snippetTitle[i+3:])
	}
	return channelTitle, snippetTitle
}

// Matches ISO 8601 duration strings like PT1H2M3S, PT45M, PT30S.
// The T is YouTube's convention; date components (years, months, days) are
// effectively always zero for videos so we ignore them.
var iso8601Duration = regexp.MustCompile(`PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?`)

func parseISO8601Duration(s string) int {
	m := iso8601Duration.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	parseOr0 := func(v string) int {
		if v == "" {
			return 0
		}
		n, _ := strconv.Atoi(v)
		return n
	}
	return parseOr0(m[1])*3600 + parseOr0(m[2])*60 + parseOr0(m[3])
}
