package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jukebox/backend/internal/models"
)

// nilIfEmpty returns nil for empty strings so nullable TEXT columns
// store NULL instead of an empty string.
func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

type PGStore struct {
	pool *dbPool
}

func NewPGStore(ctx context.Context, databaseURL string) (*PGStore, error) {
	cfg, err := newPoolConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.NewWithConfig: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &PGStore{pool: wrapPool(pool, envDuration("PG_QUERY_TIMEOUT", defaultQueryTimeout))}, nil
}

func (s *PGStore) Close() {
	s.pool.Close()
}

// ResetAllRoomsOffline marks all live rooms as ended and clears now_playing.
// Called on server startup to clean up stale state from a previous crash/restart.
func (s *PGStore) ResetAllRoomsOffline(ctx context.Context) error {
	now := time.Now()
	// Don't reset autoplay rooms — they'll be re-booted by StartAutoplayRooms
	_, err := s.pool.Exec(ctx,
		`UPDATE rooms SET is_live = false, ended_at = $1, last_active_at = $1 WHERE is_live = true AND is_autoplay = false`,
		now,
	)
	if err != nil {
		return fmt.Errorf("reset rooms: %w", err)
	}
	// Only clear now_playing for non-autoplay rooms
	_, err = s.pool.Exec(ctx, `DELETE FROM now_playing WHERE room_id NOT IN (SELECT id FROM rooms WHERE is_autoplay = true)`)
	if err != nil {
		return fmt.Errorf("clear now_playing: %w", err)
	}
	return nil
}

// legacyMigrationCutoff marks the transition to schema_migrations bookkeeping.
// Migrations sorting before this prefix (001–012) shipped before the
// bookkeeping table existed, so on older databases they may already be applied
// without being recorded. Only those files tolerate duplicate-object errors
// (see isAlreadyAppliedError) on re-run; every migration from 013 onward runs
// strictly and fails loudly.
// This set is closed — it can never grow, so do not raise the cutoff.
const legacyMigrationCutoff = "013"

// migrationLockKey is the well-known pg_advisory_lock key that serializes
// RunMigrations across server replicas sharing one database. The value is
// arbitrary but must never change: 0x6a756b65626f7801 is "jukebox\x01".
const migrationLockKey = int64(0x6a756b65626f7801)

// isAlreadyAppliedError reports whether err is Postgres telling us the
// migration's objects already exist. Matched by SQLSTATE code — never by
// message text, which is localized under non-English lc_messages:
//
//	42P07 duplicate_table, 42701 duplicate_column,
//	42710 duplicate_object, 23505 unique_violation
func isAlreadyAppliedError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P07", "42701", "42710", "23505":
		return true
	}
	return false
}

// RunMigrations executes every *.up.sql file in migrationsDir in lexicographic
// order, recording applied files in a schema_migrations table so each
// migration runs exactly once. The whole run holds a session-scoped advisory
// lock, so two replicas booting simultaneously apply each migration once
// instead of racing on the schema_migrations inserts.
//
// Databases migrated before schema_migrations existed are handled per file:
// each file executes inside one explicit transaction, so on an
// already-migrated database a pre-cutoff file fails atomically with a
// duplicate-object error, leaves the schema untouched, and is recorded as
// applied.
func (s *PGStore) RunMigrations(ctx context.Context, migrationsDir string) error {
	// The advisory lock is session-scoped, so it must be taken and released
	// on one dedicated connection — pool.Exec could run the lock and unlock
	// on different sessions.
	lockConn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer lockConn.Release()
	// The lock wait may legitimately outlast the pool-wide statement_timeout
	// (sized for individual queries) while another replica migrates; exempt
	// this session so a slow-but-healthy migration doesn't crash its peers.
	// Session-scoped, and the session is released/closed right after the run.
	if _, err := lockConn.Exec(ctx, `SET statement_timeout = 0`); err != nil {
		return fmt.Errorf("exempt migration lock from statement_timeout: %w", err)
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Unlock on a fresh context so a canceled ctx can't strand the lock.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := lockConn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			// Close the session so the lock dies with it rather than leaking
			// into a pooled connection that outlives this run.
			lockConn.Conn().Close(unlockCtx)
		}
	}()

	if _, err := lockConn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	files, err := listUpMigrations(migrationsDir)
	if err != nil {
		return err
	}

	applied := make(map[string]bool)
	rows, err := lockConn.Query(ctx, `SELECT filename FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			rows.Close()
			return fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[f] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}

	for _, f := range files {
		if applied[f] {
			continue
		}
		data, err := os.ReadFile(migrationsDir + "/" + f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}
		if err := s.applyMigration(ctx, lockConn, f, string(data)); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration executes one migration file and records it in
// schema_migrations, atomically when the SQL succeeds. Pre-cutoff files that
// fail with a duplicate-object SQLSTATE are treated as already applied on a
// database that predates bookkeeping and are recorded as such.
// It runs on the dedicated lock session (statement_timeout exempted), so a
// legitimately slow migration is not killed by the pool-wide query timeout.
func (s *PGStore) applyMigration(ctx context.Context, conn *pgxpool.Conn, name, sql string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, sql); err != nil {
		if !isAlreadyAppliedError(err) || name >= legacyMigrationCutoff {
			return fmt.Errorf("exec migration %s: %w", name, err)
		}
		// Legacy file already applied before bookkeeping existed. The failed
		// transaction is aborted (schema untouched), so record outside it.
		tx.Rollback(ctx)
		if _, err := conn.Exec(ctx,
			`INSERT INTO schema_migrations (filename) VALUES ($1) ON CONFLICT (filename) DO NOTHING`,
			name,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		return nil
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (filename) VALUES ($1)`, name,
	); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	return tx.Commit(ctx)
}

// listUpMigrations returns the *.up.sql files in dir in lexicographic order.
// Numeric prefixes are zero-padded, so this is application order.
func listUpMigrations(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

// ==================== Rooms ====================

// roomColumns is the canonical column list for room SELECT queries.
// Any new room column should be added here and in scanRoom().
const roomColumns = `id, slug, name, description, genre, vibes, cover_gradient,
	cover_art_url, request_policy, is_live, is_official, dj_key_hash,
	dj_session_id, created_at, scheduled_start, last_active_at,
	ended_at, expires_at, is_featured, dj_display_name, creator_user_id, is_autoplay`

// scanRoom scans a row into a models.Room. Column order must match roomColumns.
func scanRoom(scanner interface{ Scan(dest ...any) error }) (models.Room, error) {
	var r models.Room
	err := scanner.Scan(
		&r.ID, &r.Slug, &r.Name, &r.Description, &r.Genre, &r.Vibes,
		&r.CoverGradient, &r.CoverArtURL, &r.RequestPolicy, &r.IsLive,
		&r.IsOfficial, &r.DJKeyHash, &r.DJSessionID, &r.CreatedAt,
		&r.ScheduledStart, &r.LastActiveAt, &r.EndedAt, &r.ExpiresAt, &r.IsFeatured,
		&r.DJDisplayName, &r.CreatorUserID, &r.IsAutoplay,
	)
	return r, err
}

func (s *PGStore) CreateRoom(ctx context.Context, room *models.Room) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO rooms (id, slug, name, description, genre, vibes, cover_gradient,
			cover_art_url, request_policy, is_live, is_official, dj_key_hash,
			dj_session_id, created_at, scheduled_start, expires_at, is_featured, dj_display_name, creator_user_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		room.ID, room.Slug, room.Name, room.Description, room.Genre, room.Vibes,
		room.CoverGradient, room.CoverArtURL, room.RequestPolicy, room.IsLive,
		room.IsOfficial, room.DJKeyHash, room.DJSessionID, room.CreatedAt,
		room.ScheduledStart, room.ExpiresAt, room.IsFeatured, room.DJDisplayName, room.CreatorUserID,
	)
	return err
}

func (s *PGStore) GetRoomBySlug(ctx context.Context, slug string) (*models.Room, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+roomColumns+` FROM rooms WHERE slug = $1`, slug)
	r, err := scanRoom(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &r, err
}

func (s *PGStore) GetRoomByID(ctx context.Context, id string) (*models.Room, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+roomColumns+` FROM rooms WHERE id = $1`, id)
	r, err := scanRoom(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &r, err
}

func (s *PGStore) ListRooms(ctx context.Context, liveOnly bool, genre string) ([]models.Room, error) {
	query := `SELECT ` + roomColumns + ` FROM rooms WHERE (ended_at IS NULL OR ended_at > NOW() - INTERVAL '24 hours')`
	args := []interface{}{}
	idx := 1

	if liveOnly {
		query += fmt.Sprintf(" AND is_live = $%d", idx)
		args = append(args, true)
		idx++
	}
	if genre != "" {
		query += fmt.Sprintf(" AND genre = $%d", idx)
		args = append(args, genre)
		idx++
	}
	query += " ORDER BY is_live DESC, last_active_at DESC NULLS LAST, created_at DESC"

	return s.queryRooms(ctx, query, args...)
}

// ListAllRooms returns all rooms without any time filter (for admin).
func (s *PGStore) ListAllRooms(ctx context.Context) ([]models.Room, error) {
	return s.queryRooms(ctx, `SELECT `+roomColumns+` FROM rooms ORDER BY created_at DESC`)
}

// queryRooms is a shared helper for scanning multiple room rows.
func (s *PGStore) queryRooms(ctx context.Context, query string, args ...interface{}) ([]models.Room, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rooms []models.Room
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, r)
	}
	return rooms, nil
}

func (s *PGStore) SetRoomLive(ctx context.Context, roomID string, live bool) error {
	var lastActive *time.Time
	if !live {
		now := time.Now()
		lastActive = &now
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE rooms SET is_live = $2, last_active_at = $3 WHERE id = $1`,
		roomID, live, lastActive,
	)
	return err
}

// EndRoom marks a room as ended — sets is_live=false, ended_at, last_active_at.
func (s *PGStore) EndRoom(ctx context.Context, roomID string) error {
	now := time.Now()
	_, err := s.pool.Exec(ctx,
		`UPDATE rooms SET is_live = false, ended_at = $2, last_active_at = $2 WHERE id = $1`,
		roomID, now,
	)
	return err
}

// DeleteRoom permanently removes a room and its associated data (queue, chat, etc.).
// Use for rooms that were created but never went live.
func (s *PGStore) DeleteRoom(ctx context.Context, roomID string) error {
	// CASCADE on foreign keys handles queue_entries, chat_messages, now_playing, etc.
	_, err := s.pool.Exec(ctx, `DELETE FROM rooms WHERE id = $1`, roomID)
	return err
}

// WasRoomEverLive returns true if the room was ever set to live or has an ended_at timestamp.
func (s *PGStore) WasRoomEverLive(ctx context.Context, roomID string) (bool, error) {
	var isLive bool
	var endedAt, lastActive *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT is_live, ended_at, last_active_at FROM rooms WHERE id = $1`, roomID,
	).Scan(&isLive, &endedAt, &lastActive)
	if err != nil {
		return false, err
	}
	// Room was "ever live" if it's currently live, has ended, or has a last_active timestamp
	return isLive || endedAt != nil || lastActive != nil, nil
}

// CleanupGhostRooms deletes rooms that were created but never went live
// and are older than the given age. Returns the number of rooms deleted.
func (s *PGStore) CleanupGhostRooms(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM rooms
		WHERE is_live = false
			AND ended_at IS NULL
			AND last_active_at IS NULL
			AND scheduled_start IS NULL
			AND is_autoplay = false
			AND created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *PGStore) UpdateRoomPolicy(ctx context.Context, roomID string, policy models.RequestPolicy) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE rooms SET request_policy = $2 WHERE id = $1`,
		roomID, policy,
	)
	return err
}

// ==================== Tracks ====================

func (s *PGStore) UpsertTrack(ctx context.Context, t *models.Track) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tracks (id, title, artist, duration, source, source_url, album_gradient, info_snippet, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (id) DO NOTHING`,
		t.ID, t.Title, t.Artist, t.Duration, t.Source, t.SourceURL, t.AlbumGradient, t.InfoSnippet, t.CreatedAt,
	)
	return err
}

// UpsertTracks inserts many tracks in a single multi-row statement
// (ON CONFLICT DO NOTHING, matching UpsertTrack). One round trip regardless
// of track count — playlist pre-load used to issue one INSERT per track.
func (s *PGStore) UpsertTracks(ctx context.Context, tracks []*models.Track) error {
	if len(tracks) == 0 {
		return nil
	}
	n := len(tracks)
	ids := make([]string, n)
	titles := make([]string, n)
	artists := make([]string, n)
	durations := make([]int32, n)
	sources := make([]string, n)
	sourceURLs := make([]string, n)
	gradients := make([]string, n)
	snippets := make([]string, n)
	createdAts := make([]time.Time, n)
	for i, t := range tracks {
		ids[i] = t.ID
		titles[i] = t.Title
		artists[i] = t.Artist
		durations[i] = int32(t.Duration)
		sources[i] = string(t.Source)
		sourceURLs[i] = t.SourceURL
		gradients[i] = t.AlbumGradient
		snippets[i] = t.InfoSnippet
		createdAts[i] = t.CreatedAt
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tracks (id, title, artist, duration, source, source_url, album_gradient, info_snippet, created_at)
		SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::int4[], $5::text[], $6::text[], $7::text[], $8::text[], $9::timestamptz[])
		ON CONFLICT (id) DO NOTHING`,
		ids, titles, artists, durations, sources, sourceURLs, gradients, snippets, createdAts,
	)
	return err
}

func (s *PGStore) UpdateTrackDuration(ctx context.Context, trackID string, duration int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE tracks SET duration = $2 WHERE id = $1 AND duration = 0`, trackID, duration)
	return err
}

func (s *PGStore) UpdateTrackInfoSnippet(ctx context.Context, trackID string, snippet string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE tracks SET info_snippet = $2 WHERE id = $1`, trackID, snippet)
	return err
}

func (s *PGStore) GetTrack(ctx context.Context, id string) (*models.Track, error) {
	t := &models.Track{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, title, artist, duration, source, source_url, album_gradient, created_at
		FROM tracks WHERE id = $1`, id,
	).Scan(&t.ID, &t.Title, &t.Artist, &t.Duration, &t.Source, &t.SourceURL, &t.AlbumGradient, &t.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// GetTracksByIDs fetches many tracks in a single query (WHERE id = ANY).
// Returns a map keyed by track ID; IDs with no matching row are absent.
// Used by the rooms list to avoid one GetTrack round-trip per live room.
func (s *PGStore) GetTracksByIDs(ctx context.Context, ids []string) (map[string]*models.Track, error) {
	out := make(map[string]*models.Track, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, title, artist, duration, source, source_url, album_gradient, created_at
		FROM tracks WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		t := &models.Track{}
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Duration, &t.Source, &t.SourceURL, &t.AlbumGradient, &t.CreatedAt); err != nil {
			return nil, err
		}
		out[t.ID] = t
	}
	return out, rows.Err()
}

// ==================== Queue ====================

func (s *PGStore) AddToQueue(ctx context.Context, entry *models.QueueEntry) error {
	// Get next position
	var maxPos int
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(position), 0) FROM queue_entries WHERE room_id = $1 AND status IN ('pending','approved')`,
		entry.RoomID,
	).Scan(&maxPos)
	if err != nil {
		return err
	}
	entry.Position = maxPos + 1

	_, err = s.pool.Exec(ctx, `
		INSERT INTO queue_entries (id, room_id, track_id, submitted_by, session_id, status, position, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		entry.ID, entry.RoomID, entry.Track.ID, entry.SubmittedBy, entry.SessionID,
		entry.Status, entry.Position, entry.CreatedAt,
	)
	return err
}

// AddTracksToQueue appends many entries to a room's queue in one statement.
// Positions are computed in SQL — a single MAX read plus the row ordinal —
// so there is no SELECT MAX round trip per entry and no cross-statement
// read-then-write race within the batch (the MAX subquery and the inserts
// share one snapshot).
func (s *PGStore) AddTracksToQueue(ctx context.Context, roomID string, entries []*models.QueueEntry) error {
	if len(entries) == 0 {
		return nil
	}
	n := len(entries)
	ids := make([]string, n)
	trackIDs := make([]string, n)
	submitters := make([]string, n)
	sessionIDs := make([]string, n)
	statuses := make([]string, n)
	createdAts := make([]time.Time, n)
	for i, e := range entries {
		ids[i] = e.ID
		trackIDs[i] = e.Track.ID
		submitters[i] = e.SubmittedBy
		sessionIDs[i] = e.SessionID
		statuses[i] = string(e.Status)
		createdAts[i] = e.CreatedAt
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO queue_entries (id, room_id, track_id, submitted_by, session_id, status, position, created_at)
		SELECT e.id, $1, e.track_id, e.submitted_by, e.session_id, e.status,
			(SELECT COALESCE(MAX(position), 0) FROM queue_entries
			 WHERE room_id = $1 AND status IN ('pending','approved')) + e.ord::int,
			e.created_at
		FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::timestamptz[])
			WITH ORDINALITY AS e(id, track_id, submitted_by, session_id, status, created_at, ord)`,
		roomID, ids, trackIDs, submitters, sessionIDs, statuses, createdAts,
	)
	return err
}

func (s *PGStore) GetQueue(ctx context.Context, roomID string) ([]models.QueueEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT q.id, q.room_id, q.submitted_by, q.session_id, q.status, q.position, q.created_at,
			t.id, t.title, t.artist, t.duration, t.source, t.source_url, t.album_gradient
		FROM queue_entries q
		JOIN tracks t ON t.id = q.track_id
		WHERE q.room_id = $1 AND q.status = 'approved'
		ORDER BY q.position ASC`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []models.QueueEntry
	for rows.Next() {
		var e models.QueueEntry
		if err := rows.Scan(
			&e.ID, &e.RoomID, &e.SubmittedBy, &e.SessionID, &e.Status, &e.Position, &e.CreatedAt,
			&e.Track.ID, &e.Track.Title, &e.Track.Artist, &e.Track.Duration,
			&e.Track.Source, &e.Track.SourceURL, &e.Track.AlbumGradient,
		); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (s *PGStore) GetPendingRequests(ctx context.Context, roomID string) ([]models.QueueEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT q.id, q.room_id, q.submitted_by, q.session_id, q.status, q.position, q.created_at,
			t.id, t.title, t.artist, t.duration, t.source, t.source_url, t.album_gradient
		FROM queue_entries q
		JOIN tracks t ON t.id = q.track_id
		WHERE q.room_id = $1 AND q.status = 'pending'
		ORDER BY q.created_at ASC`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []models.QueueEntry
	for rows.Next() {
		var e models.QueueEntry
		if err := rows.Scan(
			&e.ID, &e.RoomID, &e.SubmittedBy, &e.SessionID, &e.Status, &e.Position, &e.CreatedAt,
			&e.Track.ID, &e.Track.Title, &e.Track.Artist, &e.Track.Duration,
			&e.Track.Source, &e.Track.SourceURL, &e.Track.AlbumGradient,
		); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// GetApprovedQueueCounts returns the number of approved queue entries per
// room in one aggregate query. Rooms with no approved entries are absent
// from the map. Used by the idle monitor instead of one full GetQueue round
// trip per live room per tick.
func (s *PGStore) GetApprovedQueueCounts(ctx context.Context, roomIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT room_id, COUNT(*)
		FROM queue_entries
		WHERE room_id = ANY($1) AND status = 'approved'
		GROUP BY room_id`, roomIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		out[id] = count
	}
	return out, rows.Err()
}

func (s *PGStore) UpdateQueueEntryStatus(ctx context.Context, entryID string, status models.QueueEntryStatus) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE queue_entries SET status = $2 WHERE id = $1`, entryID, status)
	return err
}

func (s *PGStore) PopNextTrack(ctx context.Context, roomID string) (*models.QueueEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var e models.QueueEntry
	err = tx.QueryRow(ctx, `
		SELECT q.id, q.room_id, q.submitted_by, q.session_id, q.status, q.position, q.created_at,
			t.id, t.title, t.artist, t.duration, t.source, t.source_url, t.album_gradient
		FROM queue_entries q
		JOIN tracks t ON t.id = q.track_id
		WHERE q.room_id = $1 AND q.status = 'approved'
		ORDER BY q.position ASC
		LIMIT 1
		FOR UPDATE OF q SKIP LOCKED`, roomID,
	).Scan(
		&e.ID, &e.RoomID, &e.SubmittedBy, &e.SessionID, &e.Status, &e.Position, &e.CreatedAt,
		&e.Track.ID, &e.Track.Title, &e.Track.Artist, &e.Track.Duration,
		&e.Track.Source, &e.Track.SourceURL, &e.Track.AlbumGradient,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx, `UPDATE queue_entries SET status = 'played' WHERE id = $1`, e.ID)
	if err != nil {
		return nil, err
	}

	return &e, tx.Commit(ctx)
}

// ==================== Chat ====================

// GetPlayedTracks returns tracks that have been played in a room, most recent first.
func (s *PGStore) GetPlayedTracks(ctx context.Context, roomID string) ([]models.QueueEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT q.id, q.room_id, q.submitted_by, q.session_id, q.status, q.position, q.created_at,
			t.id, t.title, t.artist, t.duration, t.source, t.source_url, t.album_gradient
		FROM queue_entries q
		JOIN tracks t ON t.id = q.track_id
		WHERE q.room_id = $1 AND q.status = 'played'
		ORDER BY q.position DESC`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []models.QueueEntry
	for rows.Next() {
		var e models.QueueEntry
		if err := rows.Scan(
			&e.ID, &e.RoomID, &e.SubmittedBy, &e.SessionID, &e.Status, &e.Position, &e.CreatedAt,
			&e.Track.ID, &e.Track.Title, &e.Track.Artist, &e.Track.Duration,
			&e.Track.Source, &e.Track.SourceURL, &e.Track.AlbumGradient,
		); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// GetAllSessionTracks returns played + approved tracks for a room session (for save-session).
func (s *PGStore) GetAllSessionTracks(ctx context.Context, roomID string) ([]models.QueueEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT q.id, q.room_id, q.submitted_by, q.session_id, q.status, q.position, q.created_at,
			t.id, t.title, t.artist, t.duration, t.source, t.source_url, t.album_gradient
		FROM queue_entries q
		JOIN tracks t ON t.id = q.track_id
		WHERE q.room_id = $1 AND q.status IN ('played', 'approved')
		ORDER BY q.position ASC`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []models.QueueEntry
	for rows.Next() {
		var e models.QueueEntry
		if err := rows.Scan(
			&e.ID, &e.RoomID, &e.SubmittedBy, &e.SessionID, &e.Status, &e.Position, &e.CreatedAt,
			&e.Track.ID, &e.Track.Title, &e.Track.Artist, &e.Track.Duration,
			&e.Track.Source, &e.Track.SourceURL, &e.Track.AlbumGradient,
		); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (s *PGStore) InsertChatMessage(ctx context.Context, msg *models.ChatMessage) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chat_messages (id, room_id, session_id, username, avatar_color, message, msg_type, created_at, media_url, media_type)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		msg.ID, msg.RoomID, msg.SessionID, msg.Username, msg.AvatarColor,
		msg.Message, msg.Type, msg.Timestamp,
		nilIfEmpty(msg.MediaURL), nilIfEmpty(msg.MediaType),
	)
	return err
}

func (s *PGStore) GetRecentChat(ctx context.Context, roomID string, limit int) ([]models.ChatMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, room_id, session_id, username, avatar_color, message, msg_type, created_at,
		       COALESCE(media_url, ''), COALESCE(media_type, '')
		FROM chat_messages
		WHERE room_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, roomID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []models.ChatMessage
	for rows.Next() {
		var m models.ChatMessage
		if err := rows.Scan(
			&m.ID, &m.RoomID, &m.SessionID, &m.Username, &m.AvatarColor,
			&m.Message, &m.Type, &m.Timestamp,
			&m.MediaURL, &m.MediaType,
		); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	// Reverse so oldest first
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// ==================== Now Playing ====================

func (s *PGStore) SetNowPlaying(ctx context.Context, roomID, trackID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO now_playing (room_id, track_id, started_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (room_id) DO UPDATE SET track_id = $2, started_at = NOW()`,
		roomID, trackID,
	)
	return err
}

func (s *PGStore) GetNowPlaying(ctx context.Context, roomID string) (*models.Track, error) {
	t := &models.Track{}
	err := s.pool.QueryRow(ctx, `
		SELECT t.id, t.title, t.artist, t.duration, t.source, t.source_url, t.album_gradient, t.info_snippet, t.created_at
		FROM now_playing np
		JOIN tracks t ON t.id = np.track_id
		WHERE np.room_id = $1`, roomID,
	).Scan(&t.ID, &t.Title, &t.Artist, &t.Duration, &t.Source, &t.SourceURL, &t.AlbumGradient, &t.InfoSnippet, &t.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return t, err
}

func (s *PGStore) ClearNowPlaying(ctx context.Context, roomID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM now_playing WHERE room_id = $1`, roomID)
	return err
}
