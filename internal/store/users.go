package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jukebox/backend/internal/models"
)

// ==================== Users ====================

func (s *PGStore) CreateUser(ctx context.Context, user *models.User) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, email, email_verified, password_hash, display_name, avatar_color, avatar_url, bio, favorite_genres, created_at, updated_at, stage_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		user.ID, user.Email, user.EmailVerified, user.PasswordHash, user.DisplayName,
		user.AvatarColor, user.AvatarURL, user.Bio, user.FavoriteGenres, user.CreatedAt, user.UpdatedAt, user.StageName,
	)
	return err
}

func (s *PGStore) GetUserByEmail(ctx context.Context, email string) (*models.User, error) {
	u := &models.User{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, email_verified, password_hash, display_name, avatar_color, avatar_url, bio, favorite_genres, created_at, updated_at, is_admin, city, region, country, stage_name,
		       is_plus, plus_since, plus_expires_at, neon_balance, stripe_customer_id
		FROM users WHERE email = $1`, email,
	).Scan(&u.ID, &u.Email, &u.EmailVerified, &u.PasswordHash, &u.DisplayName,
		&u.AvatarColor, &u.AvatarURL, &u.Bio, &u.FavoriteGenres, &u.CreatedAt, &u.UpdatedAt, &u.IsAdmin, &u.City, &u.Region, &u.Country, &u.StageName,
		&u.IsPlus, &u.PlusSince, &u.PlusExpiresAt, &u.NeonBalance, &u.StripeCustomerID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (s *PGStore) GetUserByID(ctx context.Context, id string) (*models.User, error) {
	u := &models.User{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, email_verified, password_hash, display_name, avatar_color, avatar_url, bio, favorite_genres, created_at, updated_at, is_admin, city, region, country, stage_name,
		       is_plus, plus_since, plus_expires_at, neon_balance, stripe_customer_id
		FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.EmailVerified, &u.PasswordHash, &u.DisplayName,
		&u.AvatarColor, &u.AvatarURL, &u.Bio, &u.FavoriteGenres, &u.CreatedAt, &u.UpdatedAt, &u.IsAdmin, &u.City, &u.Region, &u.Country, &u.StageName,
		&u.IsPlus, &u.PlusSince, &u.PlusExpiresAt, &u.NeonBalance, &u.StripeCustomerID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (s *PGStore) UpdateUserProfile(ctx context.Context, userID string, displayName, bio, avatarColor string, favoriteGenres []string, stageName string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE users SET display_name = $2, bio = $3, avatar_color = $4, favorite_genres = $5, stage_name = $6, updated_at = NOW()
		WHERE id = $1`,
		userID, displayName, bio, avatarColor, favoriteGenres, stageName,
	)
	return err
}

func (s *PGStore) UpdateUserPassword(ctx context.Context, userID, passwordHash string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = NOW() WHERE id = $1`, userID, passwordHash)
	return err
}

func (s *PGStore) UpdateUserEmail(ctx context.Context, userID, email string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET email = $2, email_verified = FALSE, updated_at = NOW() WHERE id = $1`, userID, email)
	return err
}

func (s *PGStore) SetEmailVerified(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET email_verified = TRUE, updated_at = NOW() WHERE id = $1`, userID)
	return err
}

func (s *PGStore) DeleteUser(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	return err
}

// ==================== Email Verification ====================

func (s *PGStore) CreateEmailVerification(ctx context.Context, v *models.EmailVerification) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO email_verifications (id, user_id, token, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		v.ID, v.UserID, v.Token, v.ExpiresAt, v.CreatedAt,
	)
	return err
}

func (s *PGStore) GetEmailVerificationByToken(ctx context.Context, token string) (*models.EmailVerification, error) {
	v := &models.EmailVerification{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, token, expires_at, used_at, created_at
		FROM email_verifications WHERE token = $1`, token,
	).Scan(&v.ID, &v.UserID, &v.Token, &v.ExpiresAt, &v.UsedAt, &v.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return v, err
}

func (s *PGStore) MarkEmailVerificationUsed(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE email_verifications SET used_at = NOW() WHERE id = $1`, id)
	return err
}

// ==================== Password Reset ====================

func (s *PGStore) CreatePasswordReset(ctx context.Context, pr *models.PasswordReset) error {
	// Invalidate any existing reset tokens for this user
	_, _ = s.pool.Exec(ctx, `UPDATE password_resets SET used_at = NOW() WHERE user_id = $1 AND used_at IS NULL`, pr.UserID)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO password_resets (id, user_id, token, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		pr.ID, pr.UserID, pr.Token, pr.ExpiresAt, pr.CreatedAt,
	)
	return err
}

func (s *PGStore) GetPasswordResetByToken(ctx context.Context, token string) (*models.PasswordReset, error) {
	pr := &models.PasswordReset{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, token, expires_at, used_at, created_at
		FROM password_resets WHERE token = $1`, token,
	).Scan(&pr.ID, &pr.UserID, &pr.Token, &pr.ExpiresAt, &pr.UsedAt, &pr.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return pr, err
}

func (s *PGStore) MarkPasswordResetUsed(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE password_resets SET used_at = NOW() WHERE id = $1`, id)
	return err
}

// ==================== Refresh Tokens ====================

func (s *PGStore) CreateRefreshToken(ctx context.Context, rt *models.RefreshToken) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		rt.ID, rt.UserID, rt.TokenHash, rt.ExpiresAt, rt.CreatedAt,
	)
	return err
}

func (s *PGStore) GetRefreshTokenByHash(ctx context.Context, hash string) (*models.RefreshToken, error) {
	rt := &models.RefreshToken{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, token_hash, expires_at, revoked_at, created_at
		FROM refresh_tokens WHERE token_hash = $1`, hash,
	).Scan(&rt.ID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt, &rt.RevokedAt, &rt.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return rt, err
}

func (s *PGStore) RevokeRefreshToken(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1`, id)
	return err
}

func (s *PGStore) RevokeAllUserRefreshTokens(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// Cleanup expired tokens periodically
func (s *PGStore) CleanupExpiredTokens(ctx context.Context) error {
	now := time.Now()
	_, _ = s.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < $1`, now)
	_, _ = s.pool.Exec(ctx, `DELETE FROM password_resets WHERE expires_at < $1`, now)
	_, _ = s.pool.Exec(ctx, `DELETE FROM email_verifications WHERE expires_at < $1`, now)
	return nil
}

// ==================== Admin ====================

// BootstrapAdmin ensures the user with the given email has admin privileges.
func (s *PGStore) BootstrapAdmin(ctx context.Context, email string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET is_admin = true WHERE email = $1`, email)
	return err
}

func (s *PGStore) UpdateUserLocation(ctx context.Context, userID, city, region, country string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET city = $2, region = $3, country = $4 WHERE id = $1`,
		userID, city, region, country)
	return err
}

// ==================== Listen Tracking ====================

func (s *PGStore) StartListenEvent(ctx context.Context, evt *models.ListenEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO listen_events (id, user_id, room_id, started_at)
		VALUES ($1, $2, $3, $4)`,
		evt.ID, evt.UserID, evt.RoomID, evt.StartedAt)
	return err
}

func (s *PGStore) EndListenEvent(ctx context.Context, eventID string, tracksHeard int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE listen_events
		SET ended_at = NOW(),
		    duration_seconds = EXTRACT(EPOCH FROM (NOW() - started_at))::int,
		    tracks_heard = $2
		WHERE id = $1 AND ended_at IS NULL`,
		eventID, tracksHeard)
	return err
}

func (s *PGStore) EndListenEventsByUser(ctx context.Context, userID, roomID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE listen_events
		SET ended_at = NOW(),
		    duration_seconds = EXTRACT(EPOCH FROM (NOW() - started_at))::int
		WHERE user_id = $1 AND room_id = $2 AND ended_at IS NULL`,
		userID, roomID)
	return err
}

func (s *PGStore) GetUserStats(ctx context.Context, userID string) (*models.UserStats, error) {
	stats := &models.UserStats{}
	err := s.pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(duration_seconds) / 60, 0),
			COUNT(DISTINCT room_id),
			COALESCE(SUM(tracks_heard), 0)
		FROM listen_events
		WHERE user_id = $1 AND ended_at IS NOT NULL`, userID,
	).Scan(&stats.TotalListenMinutes, &stats.RoomsVisited, &stats.TracksListened)
	if err != nil {
		return &models.UserStats{}, nil
	}
	return stats, nil
}

func (s *PGStore) GetUserFavoriteRooms(ctx context.Context, userID string, limit int) ([]models.FavoriteRoom, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, `
		SELECT le.room_id, r.name, r.slug, r.genre, COALESCE(r.cover_art_url, ''),
			SUM(le.duration_seconds) / 60 AS listen_minutes,
			COUNT(*) AS visit_count
		FROM listen_events le
		JOIN rooms r ON r.id = le.room_id
		WHERE le.user_id = $1 AND le.ended_at IS NOT NULL
		GROUP BY le.room_id, r.name, r.slug, r.genre, r.cover_art_url
		ORDER BY listen_minutes DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var favs []models.FavoriteRoom
	for rows.Next() {
		var f models.FavoriteRoom
		if err := rows.Scan(&f.RoomID, &f.RoomName, &f.RoomSlug, &f.RoomGenre, &f.CoverArtURL,
			&f.ListenMinutes, &f.VisitCount); err != nil {
			return nil, err
		}
		favs = append(favs, f)
	}
	return favs, nil
}

func (s *PGStore) SetRoomFeatured(ctx context.Context, roomID string, featured bool) error {
	// Clear existing featured if setting a new one
	if featured {
		_, _ = s.pool.Exec(ctx, `UPDATE rooms SET is_featured = false WHERE is_featured = true`)
	}
	_, err := s.pool.Exec(ctx, `UPDATE rooms SET is_featured = $2 WHERE id = $1`, roomID, featured)
	return err
}

func (s *PGStore) SetRoomOfficial(ctx context.Context, roomID string, official bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE rooms SET is_official = $2 WHERE id = $1`, roomID, official)
	return err
}

func (s *PGStore) SetRoomExpiry(ctx context.Context, roomID string, expiresAt *time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE rooms SET expires_at = $2 WHERE id = $1`, roomID, expiresAt)
	return err
}

func (s *PGStore) GetFeaturedRoom(ctx context.Context) (*models.Room, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+roomColumns+` FROM rooms WHERE is_featured = true AND is_live = true LIMIT 1`)
	r, err := scanRoom(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	r.IsFeatured = true
	return &r, err
}

// IsStageNameTaken checks if a stage_name is already in use (case-insensitive).
// excludeUserID can be set to exclude the current user (for profile updates).
func (s *PGStore) IsStageNameTaken(ctx context.Context, stageName string, excludeUserID string) (bool, error) {
	var count int
	var err error
	if excludeUserID != "" {
		err = s.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM users WHERE LOWER(stage_name) = LOWER($1) AND id != $2`,
			stageName, excludeUserID,
		).Scan(&count)
	} else {
		err = s.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM users WHERE LOWER(stage_name) = LOWER($1)`,
			stageName,
		).Scan(&count)
	}
	return count > 0, err
}
