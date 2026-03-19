package store

import (
	"context"
	"time"

	"github.com/jukebox/backend/internal/models"
)

// ==================== Direct Messages ====================

func (s *PGStore) SendDirectMessage(ctx context.Context, msg *models.DirectMessage) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO direct_messages (id, from_user_id, to_user_id, message, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		msg.ID, msg.FromUserID, msg.ToUserID, msg.Message, msg.CreatedAt,
	)
	return err
}

// GetConversation returns messages between two users, most recent last.
func (s *PGStore) GetConversation(ctx context.Context, userA, userB string, limit int) ([]models.DirectMessage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	rows, err := s.pool.Query(ctx, `
		SELECT dm.id, dm.from_user_id, dm.to_user_id, dm.message, dm.read_at, dm.created_at,
			   u.display_name, u.avatar_color
		FROM direct_messages dm
		JOIN users u ON u.id = dm.from_user_id
		WHERE (dm.from_user_id = $1 AND dm.to_user_id = $2)
		   OR (dm.from_user_id = $2 AND dm.to_user_id = $1)
		ORDER BY dm.created_at DESC
		LIMIT $3`, userA, userB, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []models.DirectMessage
	for rows.Next() {
		var m models.DirectMessage
		if err := rows.Scan(&m.ID, &m.FromUserID, &m.ToUserID, &m.Message, &m.ReadAt, &m.CreatedAt,
			&m.FromDisplayName, &m.FromAvatarColor); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}

	// Reverse to chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// GetConversationList returns a summary of all conversations for a user.
func (s *PGStore) GetConversationList(ctx context.Context, userID string) ([]models.ConversationSummary, error) {
	rows, err := s.pool.Query(ctx, `
		WITH ranked AS (
			SELECT dm.*,
				CASE WHEN dm.from_user_id = $1 THEN dm.to_user_id ELSE dm.from_user_id END AS other_user_id,
				ROW_NUMBER() OVER (
					PARTITION BY LEAST(dm.from_user_id, dm.to_user_id), GREATEST(dm.from_user_id, dm.to_user_id)
					ORDER BY dm.created_at DESC
				) AS rn
			FROM direct_messages dm
			WHERE dm.from_user_id = $1 OR dm.to_user_id = $1
		)
		SELECT r.other_user_id, u.display_name, u.avatar_color, r.message, r.created_at,
			(SELECT COUNT(*) FROM direct_messages dm2
			 WHERE dm2.from_user_id = r.other_user_id AND dm2.to_user_id = $1 AND dm2.read_at IS NULL) AS unread
		FROM ranked r
		JOIN users u ON u.id = r.other_user_id
		WHERE r.rn = 1
		ORDER BY r.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convos []models.ConversationSummary
	for rows.Next() {
		var c models.ConversationSummary
		if err := rows.Scan(&c.UserID, &c.DisplayName, &c.AvatarColor, &c.LastMessage, &c.LastAt, &c.UnreadCount); err != nil {
			return nil, err
		}
		convos = append(convos, c)
	}
	return convos, nil
}

// MarkConversationRead marks all messages from otherUser to userID as read.
func (s *PGStore) MarkConversationRead(ctx context.Context, userID, otherUserID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE direct_messages SET read_at = $3
		WHERE from_user_id = $1 AND to_user_id = $2 AND read_at IS NULL`,
		otherUserID, userID, time.Now(),
	)
	return err
}
