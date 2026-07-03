package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jukebox/backend/internal/models"
)

// newBatchTestDB gives a throwaway migrated database (skips without
// TEST_DATABASE_URL, like the migration tests — run `make docker-up` first).
func newBatchTestDB(t *testing.T) *PGStore {
	t.Helper()
	s := newMigrationTestDB(t)
	if err := s.RunMigrations(context.Background(), testMigrationsDir); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return s
}

func batchTestRoom(t *testing.T, s *PGStore, id string) *models.Room {
	t.Helper()
	room := &models.Room{
		ID:            id,
		Slug:          id,
		Name:          id,
		Vibes:         []string{},
		RequestPolicy: models.RequestPolicyOpen,
		DJKeyHash:     "not-a-real-hash",
		CreatedAt:     time.Now(),
	}
	if err := s.CreateRoom(context.Background(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	return room
}

func batchTestTracks(n int) []*models.Track {
	tracks := make([]*models.Track, n)
	for i := range tracks {
		tracks[i] = &models.Track{
			ID:        fmt.Sprintf("track-%d", i),
			Title:     fmt.Sprintf("Title %d", i),
			Artist:    "Artist",
			Duration:  180,
			Source:    models.TrackSourceYouTube,
			SourceURL: fmt.Sprintf("https://youtu.be/%d", i),
			CreatedAt: time.Now(),
		}
	}
	return tracks
}

// TestBatchQueueInserts proves the single-statement queue append: positions
// continue after the existing MAX in input order, with no per-row SELECT MAX.
func TestBatchQueueInserts(t *testing.T) {
	s := newBatchTestDB(t)
	ctx := context.Background()
	room := batchTestRoom(t, s, "room-batch-queue")

	tracks := batchTestTracks(5)
	if err := s.UpsertTracks(ctx, tracks); err != nil {
		t.Fatalf("UpsertTracks: %v", err)
	}
	// Idempotent: re-upserting must not error or duplicate.
	if err := s.UpsertTracks(ctx, tracks); err != nil {
		t.Fatalf("UpsertTracks (second run): %v", err)
	}

	// Seed one entry through the singular path so the batch has a MAX to
	// continue from.
	first := &models.QueueEntry{
		ID: "qe-seed", RoomID: room.ID, Track: *tracks[0],
		SubmittedBy: "DJ", SessionID: "s1", Status: models.QueueApproved, CreatedAt: time.Now(),
	}
	if err := s.AddToQueue(ctx, first); err != nil {
		t.Fatalf("AddToQueue: %v", err)
	}

	entries := make([]*models.QueueEntry, 4)
	for i := range entries {
		entries[i] = &models.QueueEntry{
			ID: fmt.Sprintf("qe-%d", i), RoomID: room.ID, Track: *tracks[i+1],
			SubmittedBy: "DJ", SessionID: "s1", Status: models.QueueApproved, CreatedAt: time.Now(),
		}
	}
	if err := s.AddTracksToQueue(ctx, room.ID, entries); err != nil {
		t.Fatalf("AddTracksToQueue: %v", err)
	}

	queue, err := s.GetQueue(ctx, room.ID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if len(queue) != 5 {
		t.Fatalf("queue has %d entries, want 5", len(queue))
	}
	for i, e := range queue {
		if e.Position != i+1 {
			t.Errorf("queue[%d].Position = %d, want %d (dense, continuing after MAX)", i, e.Position, i+1)
		}
	}
	if queue[1].ID != "qe-0" || queue[4].ID != "qe-3" {
		t.Errorf("batch order not preserved: got %s..%s", queue[1].ID, queue[4].ID)
	}
}

// TestBatchPlaylistInserts proves the single-statement playlist append:
// positions start at 0, continue across batches, and re-adding an existing
// track is a no-op (unique playlist_id+track_id).
func TestBatchPlaylistInserts(t *testing.T) {
	s := newBatchTestDB(t)
	ctx := context.Background()

	if err := s.CreateUser(ctx, &models.User{
		ID: "user-1", Email: "dj@example.com", PasswordHash: "x", DisplayName: "DJ",
		FavoriteGenres: []string{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pl := &models.Playlist{ID: "pl-1", UserID: "user-1", Name: "Session", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.CreatePlaylist(ctx, pl); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	tracks := batchTestTracks(4)
	if err := s.UpsertTracks(ctx, tracks); err != nil {
		t.Fatalf("UpsertTracks: %v", err)
	}

	if err := s.AddTracksToPlaylist(ctx, pl.ID,
		[]string{"pt-0", "pt-1"}, []string{"track-0", "track-1"}); err != nil {
		t.Fatalf("AddTracksToPlaylist: %v", err)
	}
	// Second batch: one duplicate (skipped) + two new — positions continue.
	if err := s.AddTracksToPlaylist(ctx, pl.ID,
		[]string{"pt-dup", "pt-2", "pt-3"}, []string{"track-0", "track-2", "track-3"}); err != nil {
		t.Fatalf("AddTracksToPlaylist (second batch): %v", err)
	}

	got, err := s.GetPlaylistWithTracks(ctx, pl.ID)
	if err != nil {
		t.Fatalf("GetPlaylistWithTracks: %v", err)
	}
	if got.TrackCount != 4 {
		t.Fatalf("playlist has %d tracks, want 4 (duplicate skipped)", got.TrackCount)
	}
	wantOrder := []string{"track-0", "track-1", "track-2", "track-3"}
	for i, pt := range got.Tracks {
		if pt.TrackID != wantOrder[i] {
			t.Errorf("tracks[%d] = %s, want %s", i, pt.TrackID, wantOrder[i])
		}
	}
	if got.Tracks[0].Position != 0 {
		t.Errorf("first position = %d, want 0 (matches AddTrackToPlaylist)", got.Tracks[0].Position)
	}

	// Mismatched parallel slices must be rejected, not silently misaligned.
	if err := s.AddTracksToPlaylist(ctx, pl.ID, []string{"only-one"}, []string{"t-a", "t-b"}); err == nil {
		t.Error("mismatched ids/trackIDs lengths: got nil error, want error")
	}
}

// TestGetTracksByIDs proves the rooms-list batch fetch: one ANY($1) query
// returns every existing track keyed by ID, IDs with no row are simply
// absent (the handler degrades to no nowPlaying), and empty input returns
// an empty map without touching the database.
func TestGetTracksByIDs(t *testing.T) {
	s := newBatchTestDB(t)
	ctx := context.Background()

	tracks := batchTestTracks(3)
	if err := s.UpsertTracks(ctx, tracks); err != nil {
		t.Fatalf("UpsertTracks: %v", err)
	}

	got, err := s.GetTracksByIDs(ctx, []string{"track-0", "track-2", "track-missing"})
	if err != nil {
		t.Fatalf("GetTracksByIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tracks, want 2 (missing ID must be absent, not an error)", len(got))
	}
	for _, id := range []string{"track-0", "track-2"} {
		tr := got[id]
		if tr == nil {
			t.Fatalf("track %s missing from result", id)
		}
		if tr.ID != id || tr.Artist != "Artist" || tr.Duration != 180 {
			t.Errorf("track %s = %+v, want the upserted row", id, tr)
		}
	}
	if _, ok := got["track-missing"]; ok {
		t.Error("nonexistent ID present in result map")
	}

	empty, err := s.GetTracksByIDs(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty input: got (%v, %v), want empty map", empty, err)
	}
}

// TestGetApprovedQueueCounts proves the idle monitor's aggregate: one query,
// approved entries only, absent rooms read as zero.
func TestGetApprovedQueueCounts(t *testing.T) {
	s := newBatchTestDB(t)
	ctx := context.Background()
	roomA := batchTestRoom(t, s, "room-counts-a")
	roomB := batchTestRoom(t, s, "room-counts-b")

	tracks := batchTestTracks(3)
	if err := s.UpsertTracks(ctx, tracks); err != nil {
		t.Fatalf("UpsertTracks: %v", err)
	}
	add := func(id, roomID string, track *models.Track, status models.QueueEntryStatus) {
		t.Helper()
		if err := s.AddToQueue(ctx, &models.QueueEntry{
			ID: id, RoomID: roomID, Track: *track,
			SubmittedBy: "DJ", SessionID: "s1", Status: status, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("AddToQueue(%s): %v", id, err)
		}
	}
	add("ce-1", roomA.ID, tracks[0], models.QueueApproved)
	add("ce-2", roomA.ID, tracks[1], models.QueueApproved)
	add("ce-3", roomA.ID, tracks[2], models.QueuePending) // not approved — excluded
	add("ce-4", roomB.ID, tracks[0], models.QueuePlayed)  // played — excluded

	counts, err := s.GetApprovedQueueCounts(ctx, []string{roomA.ID, roomB.ID, "room-missing"})
	if err != nil {
		t.Fatalf("GetApprovedQueueCounts: %v", err)
	}
	if counts[roomA.ID] != 2 {
		t.Errorf("roomA count = %d, want 2", counts[roomA.ID])
	}
	if counts[roomB.ID] != 0 {
		t.Errorf("roomB count = %d, want 0 (played entries excluded)", counts[roomB.ID])
	}
	if counts["room-missing"] != 0 {
		t.Errorf("missing room count = %d, want 0", counts["room-missing"])
	}

	empty, err := s.GetApprovedQueueCounts(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty input: got (%v, %v), want empty map", empty, err)
	}
}

// TestUpsertAutoplayTrackRefreshesMetadata proves stable-ID reuse: replaying
// the same (room, URL) updates the existing row in place — no new rows — and
// a duration learned from a client report survives a playlist that says 0.
func TestUpsertAutoplayTrackRefreshesMetadata(t *testing.T) {
	s := newBatchTestDB(t)
	ctx := context.Background()

	countTracks := func() int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM tracks`).Scan(&n); err != nil {
			t.Fatalf("count tracks: %v", err)
		}
		return n
	}

	tr := &models.Track{
		ID: "auto-deadbeefdeadbeef", Title: "First Title", Artist: "A", Duration: 0,
		Source: models.TrackSourceYouTube, SourceURL: "https://youtu.be/x", CreatedAt: time.Now(),
	}
	if err := s.UpsertAutoplayTrack(ctx, tr); err != nil {
		t.Fatalf("UpsertAutoplayTrack: %v", err)
	}
	if n := countTracks(); n != 1 {
		t.Fatalf("tracks rows = %d, want 1", n)
	}

	// Client reports the real duration (playlist still says 0).
	if err := s.UpdateTrackDuration(ctx, tr.ID, 240); err != nil {
		t.Fatalf("UpdateTrackDuration: %v", err)
	}

	// Next loop of the playlist: retitled, duration still unknown upstream.
	tr2 := *tr
	tr2.Title = "Corrected Title"
	tr2.InfoSnippet = "now with liner notes"
	if err := s.UpsertAutoplayTrack(ctx, &tr2); err != nil {
		t.Fatalf("UpsertAutoplayTrack (replay): %v", err)
	}
	if n := countTracks(); n != 1 {
		t.Fatalf("tracks rows after replay = %d, want 1 (stable ID must dedupe)", n)
	}

	got, err := s.GetTrack(ctx, tr.ID)
	if err != nil || got == nil {
		t.Fatalf("read back track: (%v, %v)", got, err)
	}
	if got.Title != "Corrected Title" {
		t.Errorf("title = %q, want refreshed %q", got.Title, "Corrected Title")
	}
	if got.Duration != 240 {
		t.Errorf("duration = %d, want 240 (learned duration must survive a 0 upsert)", got.Duration)
	}
}
