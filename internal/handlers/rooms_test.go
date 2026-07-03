package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jukebox/backend/internal/models"
)

func liveRoom(id string) models.Room {
	return models.Room{ID: id, Slug: id, Name: id, IsLive: true}
}

func offlineRoom(id string) models.Room {
	return models.Room{ID: id, Slug: id, Name: id, IsLive: false}
}

// TestNowPlayingTrackIDs covers the input side of the batched track fetch:
// only live rooms with a playing track contribute, and duplicates collapse
// so the batch query receives each ID once.
func TestNowPlayingTrackIDs(t *testing.T) {
	rooms := []models.Room{
		liveRoom("r1"),    // playing t1
		liveRoom("r2"),    // playing t1 too — must dedupe
		liveRoom("r3"),    // playback state with empty TrackID — skipped
		liveRoom("r4"),    // no playback state — skipped
		offlineRoom("r5"), // stale playback state on offline room — skipped
	}
	playbacks := map[string]*models.PlaybackState{
		"r1": {RoomID: "r1", TrackID: "t1"},
		"r2": {RoomID: "r2", TrackID: "t1"},
		"r3": {RoomID: "r3", TrackID: ""},
		"r5": {RoomID: "r5", TrackID: "t9"},
	}

	ids := nowPlayingTrackIDs(rooms, playbacks)
	if len(ids) != 1 || ids[0] != "t1" {
		t.Errorf("nowPlayingTrackIDs = %v, want [t1]", ids)
	}

	if got := nowPlayingTrackIDs(nil, nil); len(got) != 0 {
		t.Errorf("nowPlayingTrackIDs(nil, nil) = %v, want empty", got)
	}
}

// TestAssembleRoomsList covers the output side of the batched track fetch:
// tracks resolved by the single ANY($1) query are attached to the right
// rooms, missing tracks degrade to no nowPlaying, and listener counts map on.
func TestAssembleRoomsList(t *testing.T) {
	rooms := []models.Room{
		liveRoom("r1"),    // playing t1, 7 listeners
		liveRoom("r2"),    // playing t-missing (not returned by batch fetch)
		liveRoom("r3"),    // no playback state
		offlineRoom("r4"), // stale playback state must be ignored
	}
	counts := map[string]int64{"r1": 7, "r4": 3}
	playbacks := map[string]*models.PlaybackState{
		"r1": {RoomID: "r1", TrackID: "t1"},
		"r2": {RoomID: "r2", TrackID: "t-missing"},
		"r4": {RoomID: "r4", TrackID: "t1"},
	}
	tracks := map[string]*models.Track{
		"t1": {ID: "t1", Title: "Song One", Artist: "Artist"},
	}

	result := assembleRoomsList(rooms, counts, playbacks, tracks)
	if len(result) != 4 {
		t.Fatalf("got %d rooms, want 4", len(result))
	}

	if result[0].NowPlaying == nil || result[0].NowPlaying.ID != "t1" {
		t.Errorf("r1 nowPlaying = %+v, want track t1", result[0].NowPlaying)
	}
	if result[0].ListenerCount != 7 {
		t.Errorf("r1 listenerCount = %d, want 7", result[0].ListenerCount)
	}
	if result[1].NowPlaying != nil {
		t.Errorf("r2 nowPlaying = %+v, want nil (track missing from batch)", result[1].NowPlaying)
	}
	if result[2].NowPlaying != nil {
		t.Errorf("r3 nowPlaying = %+v, want nil (no playback state)", result[2].NowPlaying)
	}
	if result[3].NowPlaying != nil {
		t.Errorf("r4 (offline) nowPlaying = %+v, want nil", result[3].NowPlaying)
	}
	if result[3].ListenerCount != 3 {
		t.Errorf("r4 listenerCount = %d, want 3", result[3].ListenerCount)
	}

	// Rooms absent from the counts map read as 0 listeners.
	if result[1].ListenerCount != 0 || result[2].ListenerCount != 0 {
		t.Errorf("rooms without counts should have 0 listeners, got r2=%d r3=%d",
			result[1].ListenerCount, result[2].ListenerCount)
	}
}

// TestSessionPlaylistTrackIDs covers the input side of the batched
// save-session INSERT: now-playing leads, queue order is preserved, and
// duplicates/empty IDs never reach the multi-row statement twice.
func TestSessionPlaylistTrackIDs(t *testing.T) {
	entry := func(trackID string) models.QueueEntry {
		return models.QueueEntry{Track: models.Track{ID: trackID}}
	}

	tests := []struct {
		desc       string
		nowPlaying string
		entries    []models.QueueEntry
		want       []string
	}{
		{"now-playing first, then queue order", "np", []models.QueueEntry{entry("a"), entry("b")}, []string{"np", "a", "b"}},
		{"no now-playing", "", []models.QueueEntry{entry("a"), entry("b")}, []string{"a", "b"}},
		{"now-playing also queued dedupes", "a", []models.QueueEntry{entry("a"), entry("b")}, []string{"a", "b"}},
		{"duplicate queue entries collapse", "", []models.QueueEntry{entry("a"), entry("a"), entry("b")}, []string{"a", "b"}},
		{"empty track IDs skipped", "", []models.QueueEntry{entry(""), entry("a")}, []string{"a"}},
		{"empty session", "", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := sessionPlaylistTrackIDs(tt.nowPlaying, tt.entries)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestSetPublicCache: a plain response gets the shared-cache header, but if
// SessionMiddleware already minted a session cookie (first-time visitor), the
// response is personalized and must be no-store — a shared cache storing that
// Set-Cookie would hand one visitor's session to everyone else.
func TestSetPublicCache(t *testing.T) {
	t.Run("no cookie: publicly cacheable", func(t *testing.T) {
		w := httptest.NewRecorder()
		setPublicCache(w)
		if got, want := w.Header().Get("Cache-Control"), "public, max-age=5, stale-while-revalidate=30"; got != want {
			t.Errorf("Cache-Control = %q, want %q", got, want)
		}
	})

	t.Run("set-cookie present: no-store", func(t *testing.T) {
		w := httptest.NewRecorder()
		http.SetCookie(w, &http.Cookie{Name: "jukebox_session", Value: "s1", Path: "/"})
		setPublicCache(w)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})
}

func TestFeaturedRoomIndex(t *testing.T) {
	withCount := func(r models.Room, n int) RoomWithNowPlaying {
		r.ListenerCount = n
		return RoomWithNowPlaying{Room: r}
	}
	featured := liveRoom("feat")
	featured.IsFeatured = true
	offlineFeatured := offlineRoom("off-feat")
	offlineFeatured.IsFeatured = true

	tests := []struct {
		desc   string
		result []RoomWithNowPlaying
		want   int
	}{
		{"empty list", nil, -1},
		{"no live rooms", []RoomWithNowPlaying{withCount(offlineRoom("a"), 10)}, -1},
		{"explicit featured live room wins", []RoomWithNowPlaying{
			withCount(liveRoom("a"), 100),
			{Room: featured},
		}, 1},
		{"offline featured flag ignored, most listeners wins", []RoomWithNowPlaying{
			{Room: offlineFeatured},
			withCount(liveRoom("a"), 1),
			withCount(liveRoom("b"), 5),
		}, 2},
		{"live room with zero listeners still qualifies", []RoomWithNowPlaying{
			withCount(offlineRoom("a"), 50),
			withCount(liveRoom("b"), 0),
		}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := featuredRoomIndex(tt.result); got != tt.want {
				t.Errorf("featuredRoomIndex = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPayloadCacheHitWithinTTL(t *testing.T) {
	c := newPayloadCache(time.Minute)
	var builds int32
	build := func() ([]byte, error) {
		atomic.AddInt32(&builds, 1)
		return []byte("payload"), nil
	}

	for i := 0; i < 5; i++ {
		data, err := c.getOrBuild("k", build)
		if err != nil {
			t.Fatalf("getOrBuild: %v", err)
		}
		if string(data) != "payload" {
			t.Fatalf("got %q, want payload", data)
		}
	}
	if n := atomic.LoadInt32(&builds); n != 1 {
		t.Errorf("build ran %d times, want 1 (cache hit within TTL)", n)
	}
}

func TestPayloadCacheExpires(t *testing.T) {
	c := newPayloadCache(10 * time.Millisecond)
	var builds int32
	build := func() ([]byte, error) {
		atomic.AddInt32(&builds, 1)
		return []byte("payload"), nil
	}

	if _, err := c.getOrBuild("k", build); err != nil {
		t.Fatalf("getOrBuild: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := c.getOrBuild("k", build); err != nil {
		t.Fatalf("getOrBuild after expiry: %v", err)
	}
	if n := atomic.LoadInt32(&builds); n != 2 {
		t.Errorf("build ran %d times, want 2 (rebuild after TTL)", n)
	}
}

func TestPayloadCacheKeysAreIndependent(t *testing.T) {
	c := newPayloadCache(time.Minute)
	a, _ := c.getOrBuild("a", func() ([]byte, error) { return []byte("A"), nil })
	b, _ := c.getOrBuild("b", func() ([]byte, error) { return []byte("B"), nil })
	if string(a) != "A" || string(b) != "B" {
		t.Errorf("got a=%q b=%q, want A and B", a, b)
	}
}

// TestPayloadCacheErrorNotCached: a failed build must not poison the cache —
// the next caller retries and can succeed.
func TestPayloadCacheErrorNotCached(t *testing.T) {
	c := newPayloadCache(time.Minute)
	boom := errors.New("boom")
	if _, err := c.getOrBuild("k", func() ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("got err %v, want boom", err)
	}
	data, err := c.getOrBuild("k", func() ([]byte, error) { return []byte("ok"), nil })
	if err != nil || string(data) != "ok" {
		t.Errorf("retry after error: got (%q, %v), want (ok, nil)", data, err)
	}
}

// TestPayloadCacheSingleflight: concurrent misses on the same key share one
// build — the homepage stampede must not fan out to Postgres/Redis.
func TestPayloadCacheSingleflight(t *testing.T) {
	c := newPayloadCache(time.Minute)
	var builds int32
	release := make(chan struct{})
	build := func() ([]byte, error) {
		atomic.AddInt32(&builds, 1)
		<-release // hold the flight open so all goroutines pile onto it
		return []byte("shared"), nil
	}

	const n = 16
	var wg sync.WaitGroup
	started := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			data, err := c.getOrBuild("k", build)
			if err != nil || string(data) != "shared" {
				t.Errorf("got (%q, %v), want (shared, nil)", data, err)
			}
		}()
	}
	for i := 0; i < n; i++ {
		<-started
	}
	// All goroutines have at least reached getOrBuild; let the build finish.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&builds); got != 1 {
		t.Errorf("build ran %d times for %d concurrent callers, want 1", got, n)
	}
}
