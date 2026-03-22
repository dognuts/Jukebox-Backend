package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jukebox/backend/internal/models"
)

type PGStore struct {
	pool *pgxpool.Pool
}

func NewPGStore(ctx context.Context, databaseURL string) (*PGStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &PGStore{pool: pool}, nil
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

// RunMigrations reads and executes the migration files in order.
func (s *PGStore) RunMigrations(ctx context.Context, migrationsDir string) error {
	files := []string{"001_initial.up.sql", "002_user_accounts.up.sql", "003_messages_playlists.up.sql", "004_room_ended.up.sql", "005_admin.up.sql", "006_location_listen.up.sql", "007_stage_name.up.sql", "008_unique_stage_name.up.sql", "009_monetization.up.sql", "010_add_banned.up.sql", "011_autoplay.up.sql"}
	for _, f := range files {
		data, err := os.ReadFile(migrationsDir + "/" + f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}
		_, err = s.pool.Exec(ctx, string(data))
		if err != nil {
			if !strings.Contains(err.Error(), "already exists") &&
				!strings.Contains(err.Error(), "duplicate key") {
				return fmt.Errorf("exec migration %s: %w", f, err)
			}
		}
	}
	return nil
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
		INSERT INTO tracks (id, title, artist, duration, source, source_url, album_gradient, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO NOTHING`,
		t.ID, t.Title, t.Artist, t.Duration, t.Source, t.SourceURL, t.AlbumGradient, t.CreatedAt,
	)
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
		INSERT INTO chat_messages (id, room_id, session_id, username, avatar_color, message, msg_type, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		msg.ID, msg.RoomID, msg.SessionID, msg.Username, msg.AvatarColor,
		msg.Message, msg.Type, msg.Timestamp,
	)
	return err
}

func (s *PGStore) GetRecentChat(ctx context.Context, roomID string, limit int) ([]models.ChatMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, room_id, session_id, username, avatar_color, message, msg_type, created_at
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
		SELECT t.id, t.title, t.artist, t.duration, t.source, t.source_url, t.album_gradient, t.created_at
		FROM now_playing np
		JOIN tracks t ON t.id = np.track_id
		WHERE np.room_id = $1`, roomID,
	).Scan(&t.ID, &t.Title, &t.Artist, &t.Duration, &t.Source, &t.SourceURL, &t.AlbumGradient, &t.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return t, err
}

func (s *PGStore) ClearNowPlaying(ctx context.Context, roomID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM now_playing WHERE room_id = $1`, roomID)
	return err
}
