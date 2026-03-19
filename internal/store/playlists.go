package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jukebox/backend/internal/models"
)

// ==================== Playlists ====================

func (s *PGStore) CreatePlaylist(ctx context.Context, pl *models.Playlist) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO playlists (id, user_id, name, is_liked, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		pl.ID, pl.UserID, pl.Name, pl.IsLiked, pl.CreatedAt, pl.UpdatedAt,
	)
	return err
}

func (s *PGStore) GetPlaylistsByUser(ctx context.Context, userID string) ([]models.Playlist, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.user_id, p.name, p.is_liked, p.created_at, p.updated_at,
			(SELECT COUNT(*) FROM playlist_tracks pt WHERE pt.playlist_id = p.id) AS track_count
		FROM playlists p
		WHERE p.user_id = $1
		ORDER BY p.is_liked DESC, p.updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var playlists []models.Playlist
	for rows.Next() {
		var p models.Playlist
		var trackCount int
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.IsLiked, &p.CreatedAt, &p.UpdatedAt, &trackCount); err != nil {
			return nil, err
		}
		_ = trackCount // available for response if needed
		playlists = append(playlists, p)
	}
	return playlists, nil
}

func (s *PGStore) GetPlaylistByID(ctx context.Context, id string) (*models.Playlist, error) {
	p := &models.Playlist{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, is_liked, created_at, updated_at
		FROM playlists WHERE id = $1`, id,
	).Scan(&p.ID, &p.UserID, &p.Name, &p.IsLiked, &p.CreatedAt, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (s *PGStore) GetPlaylistWithTracks(ctx context.Context, playlistID string) (*models.Playlist, error) {
	p, err := s.GetPlaylistByID(ctx, playlistID)
	if err != nil || p == nil {
		return p, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT pt.id, pt.track_id, pt.position, pt.added_at, t.title, t.artist, t.duration
		FROM playlist_tracks pt
		JOIN tracks t ON t.id = pt.track_id
		WHERE pt.playlist_id = $1
		ORDER BY pt.position ASC`, playlistID)
	if err != nil {
		return p, err
	}
	defer rows.Close()

	for rows.Next() {
		var t models.PlaylistTrack
		if err := rows.Scan(&t.ID, &t.TrackID, &t.Position, &t.AddedAt, &t.Title, &t.Artist, &t.Duration); err != nil {
			return p, err
		}
		p.Tracks = append(p.Tracks, t)
	}
	return p, nil
}

func (s *PGStore) UpdatePlaylistName(ctx context.Context, id, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE playlists SET name = $2, updated_at = NOW() WHERE id = $1`, id, name)
	return err
}

func (s *PGStore) DeletePlaylist(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM playlists WHERE id = $1`, id)
	return err
}

func (s *PGStore) AddTrackToPlaylist(ctx context.Context, pt *models.PlaylistTrack, playlistID string) error {
	// Get next position
	var maxPos int
	s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(position), -1) FROM playlist_tracks WHERE playlist_id = $1`, playlistID).Scan(&maxPos)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO playlist_tracks (id, playlist_id, track_id, position, added_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (playlist_id, track_id) DO NOTHING`,
		pt.ID, playlistID, pt.TrackID, maxPos+1, time.Now(),
	)
	if err == nil {
		s.pool.Exec(ctx, `UPDATE playlists SET updated_at = NOW() WHERE id = $1`, playlistID)
	}
	return err
}

func (s *PGStore) RemoveTrackFromPlaylist(ctx context.Context, playlistID, trackID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM playlist_tracks WHERE playlist_id = $1 AND track_id = $2`, playlistID, trackID)
	if err == nil {
		s.pool.Exec(ctx, `UPDATE playlists SET updated_at = NOW() WHERE id = $1`, playlistID)
	}
	return err
}

// EnsureLikedPlaylist creates the "Liked Tracks" playlist if it doesn't exist.
func (s *PGStore) EnsureLikedPlaylist(ctx context.Context, userID string) (*models.Playlist, error) {
	p := &models.Playlist{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, is_liked, created_at, updated_at
		FROM playlists WHERE user_id = $1 AND is_liked = TRUE`, userID,
	).Scan(&p.ID, &p.UserID, &p.Name, &p.IsLiked, &p.CreatedAt, &p.UpdatedAt)

	if err == pgx.ErrNoRows {
		p = &models.Playlist{
			ID:        "liked-" + userID,
			UserID:    userID,
			Name:      "Liked Tracks",
			IsLiked:   true,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		if err := s.CreatePlaylist(ctx, p); err != nil {
			return nil, err
		}
		return p, nil
	}
	return p, err
}
