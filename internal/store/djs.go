package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jukebox/backend/internal/models"
)

// ==================== DJ Public Profiles ====================

// GetUserByStageName resolves a user by stage name, case-insensitively.
// Returns (nil, nil) when no user has claimed the name (or the account is
// banned — banned DJs have no public profile). The lookup matches the
// partial unique index idx_users_stage_name_unique, which covers
// LOWER(stage_name) for non-empty stage names, so at most one row can match.
func (s *PGStore) GetUserByStageName(ctx context.Context, stageName string) (*models.User, error) {
	u := &models.User{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, display_name, stage_name, avatar_color, avatar_url, bio, favorite_genres, created_at
		FROM users
		WHERE stage_name != '' AND LOWER(stage_name) = LOWER($1) AND is_banned = false`, stageName,
	).Scan(&u.ID, &u.DisplayName, &u.StageName, &u.AvatarColor, &u.AvatarURL, &u.Bio, &u.FavoriteGenres, &u.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// GetLiveRoomByCreator returns the room the creator is currently live in,
// or (nil, nil) when they are off-air.
func (s *PGStore) GetLiveRoomByCreator(ctx context.Context, userID string) (*models.Room, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+roomColumns+` FROM rooms
		WHERE creator_user_id = $1 AND is_live = true
		ORDER BY last_active_at DESC NULLS LAST, created_at DESC
		LIMIT 1`, userID)
	r, err := scanRoom(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &r, err
}

// GetDJStats aggregates a DJ's show history: totalShows counts rooms that
// actually went on air (currently live or ended), totalListeners counts
// distinct logged-in listeners across all their rooms (anonymous listeners
// leave no listen_events rows, so this is a floor, not a fabrication).
func (s *PGStore) GetDJStats(ctx context.Context, userID string) (models.DJStats, error) {
	var st models.DJStats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM rooms
			 WHERE creator_user_id = $1 AND (is_live = true OR ended_at IS NOT NULL)),
			(SELECT COUNT(DISTINCT le.user_id) FROM listen_events le
			 JOIN rooms r ON r.id = le.room_id
			 WHERE r.creator_user_id = $1)`, userID,
	).Scan(&st.TotalShows, &st.TotalListeners)
	return st, err
}

// GetDJRecentShows returns the DJ's most recently ended rooms, newest first,
// each with the number of tracks played during the session. The currently
// live room (if any) is not a "recent show" — it is surfaced via
// isLive/currentRoomSlug instead.
func (s *PGStore) GetDJRecentShows(ctx context.Context, userID string, limit int) ([]models.DJRecentShow, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.name, r.ended_at,
			(SELECT COUNT(*) FROM queue_entries q WHERE q.room_id = r.id AND q.status = 'played')
		FROM rooms r
		WHERE r.creator_user_id = $1 AND r.ended_at IS NOT NULL
		ORDER BY r.ended_at DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var shows []models.DJRecentShow
	for rows.Next() {
		var s models.DJRecentShow
		if err := rows.Scan(&s.RoomName, &s.Date, &s.TrackCount); err != nil {
			return nil, err
		}
		shows = append(shows, s)
	}
	return shows, rows.Err()
}

// GetDJGenre returns the genre of the DJ's most relevant room — the live one
// first, then the most recently active — or "" when none of their rooms has
// a genre set (users have no genre column of their own).
func (s *PGStore) GetDJGenre(ctx context.Context, userID string) (string, error) {
	var genre string
	err := s.pool.QueryRow(ctx, `
		SELECT genre FROM rooms
		WHERE creator_user_id = $1 AND genre != ''
		ORDER BY is_live DESC, COALESCE(ended_at, last_active_at, created_at) DESC
		LIMIT 1`, userID,
	).Scan(&genre)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return genre, err
}
