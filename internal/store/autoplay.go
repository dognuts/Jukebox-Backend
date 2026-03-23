package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jukebox/backend/internal/models"
)

// ==================== Autoplay Playlists ====================

// GetAutoplayPlaylist returns a playlist by room and status ("live" or "staged").
func (s *PGStore) GetAutoplayPlaylist(ctx context.Context, roomID, status string) (*models.AutoplayPlaylist, error) {
	var p models.AutoplayPlaylist
	var tracksJSON []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, room_id, status, name, tracks, current_index, created_at, activated_at
		FROM autoplay_playlists WHERE room_id = $1 AND status = $2`, roomID, status,
	).Scan(&p.ID, &p.RoomID, &p.Status, &p.Name, &tracksJSON, &p.CurrentIndex, &p.CreatedAt, &p.ActivatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal(tracksJSON, &p.Tracks)
	return &p, nil
}

// GetAutoplayPlaylists returns both live and staged playlists for a room.
func (s *PGStore) GetAutoplayPlaylists(ctx context.Context, roomID string) ([]models.AutoplayPlaylist, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, room_id, status, name, tracks, current_index, created_at, activated_at
		FROM autoplay_playlists WHERE room_id = $1 ORDER BY status`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var playlists []models.AutoplayPlaylist
	for rows.Next() {
		var p models.AutoplayPlaylist
		var tracksJSON []byte
		if err := rows.Scan(&p.ID, &p.RoomID, &p.Status, &p.Name, &tracksJSON, &p.CurrentIndex, &p.CreatedAt, &p.ActivatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal(tracksJSON, &p.Tracks)
		playlists = append(playlists, p)
	}
	return playlists, nil
}

// SaveAutoplayPlaylist creates or updates a playlist (upsert by room_id + status).
func (s *PGStore) SaveAutoplayPlaylist(ctx context.Context, p *models.AutoplayPlaylist) error {
	tracksJSON, err := json.Marshal(p.Tracks)
	if err != nil {
		return err
	}
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO autoplay_playlists (id, room_id, status, name, tracks, current_index, created_at, activated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (room_id, status) DO UPDATE SET
			name = EXCLUDED.name,
			tracks = EXCLUDED.tracks,
			current_index = EXCLUDED.current_index,
			activated_at = EXCLUDED.activated_at`,
		p.ID, p.RoomID, p.Status, p.Name, tracksJSON, p.CurrentIndex, p.CreatedAt, p.ActivatedAt)
	return err
}

// UpdateAutoplayIndex updates the current_index for the live playlist.
func (s *PGStore) UpdateAutoplayIndex(ctx context.Context, roomID string, index int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE autoplay_playlists SET current_index = $2 WHERE room_id = $1 AND status = 'live'`,
		roomID, index)
	return err
}

// ActivateStagedPlaylist promotes the staged playlist to live, deleting the old live one.
func (s *PGStore) ActivateStagedPlaylist(ctx context.Context, roomID string) (*models.AutoplayPlaylist, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Delete old live playlist
	_, err = tx.Exec(ctx, `DELETE FROM autoplay_playlists WHERE room_id = $1 AND status = 'live'`, roomID)
	if err != nil {
		return nil, fmt.Errorf("delete old live: %w", err)
	}

	// Promote staged → live, reset index to 0
	now := time.Now()
	_, err = tx.Exec(ctx,
		`UPDATE autoplay_playlists SET status = 'live', current_index = 0, activated_at = $2 WHERE room_id = $1 AND status = 'staged'`,
		roomID, now)
	if err != nil {
		return nil, fmt.Errorf("promote staged: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return s.GetAutoplayPlaylist(ctx, roomID, "live")
}

// DeleteAutoplayPlaylist deletes a playlist by room and status.
func (s *PGStore) DeleteAutoplayPlaylist(ctx context.Context, roomID, status string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM autoplay_playlists WHERE room_id = $1 AND status = $2`, roomID, status)
	return err
}

// GetNextAutoplayTrack returns the next track from the live playlist, looping if needed.
// Returns nil if no live playlist or empty tracks.
func (s *PGStore) GetNextAutoplayTrack(ctx context.Context, roomID string) (*models.AutoplayTrack, int, error) {
	playlist, err := s.GetAutoplayPlaylist(ctx, roomID, "live")
	if err != nil || playlist == nil || len(playlist.Tracks) == 0 {
		return nil, 0, err
	}

	// Loop: wrap index around
	idx := playlist.CurrentIndex % len(playlist.Tracks)
	track := &playlist.Tracks[idx]

	// Advance index for next call
	nextIdx := (idx + 1) % len(playlist.Tracks)
	s.UpdateAutoplayIndex(ctx, roomID, nextIdx)

	return track, idx, nil
}

// GetAutoplayRooms returns all rooms with is_autoplay = true.
func (s *PGStore) GetAutoplayRooms(ctx context.Context) ([]models.Room, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, slug, name, description, genre, is_live, is_official, is_autoplay
		FROM rooms WHERE is_autoplay = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rooms []models.Room
	for rows.Next() {
		var r models.Room
		if err := rows.Scan(&r.ID, &r.Slug, &r.Name, &r.Description, &r.Genre, &r.IsLive, &r.IsOfficial, &r.IsAutoplay); err != nil {
			return nil, err
		}
		rooms = append(rooms, r)
	}
	return rooms, nil
}

// SetRoomAutoplay sets the is_autoplay and is_live flags on a room.
func (s *PGStore) SetRoomAutoplay(ctx context.Context, roomID string, autoplay bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE rooms SET is_autoplay = $2, is_live = $2, ended_at = NULL, last_active_at = NOW() WHERE id = $1`, roomID, autoplay)
	return err
}

// CreateAutoplayRoom inserts a new room with is_autoplay = true.
func (s *PGStore) CreateAutoplayRoom(ctx context.Context, room *models.Room) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO rooms (id, slug, name, description, genre, cover_gradient, request_policy, is_live, is_official, is_autoplay, created_at, dj_key_hash, dj_session_id, dj_display_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, '', '', '24/7 Radio')`,
		room.ID, room.Slug, room.Name, room.Description, room.Genre, room.CoverGradient,
		room.RequestPolicy, room.IsLive, room.IsOfficial, room.IsAutoplay, room.CreatedAt)
	return err
}
