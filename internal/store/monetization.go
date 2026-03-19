package store

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jukebox/backend/internal/models"
)

// ---------- Plus Subscription ----------

func (s *PGStore) ActivatePlus(ctx context.Context, userID string, periodEnd time.Time, stripeSubID string) error {
	now := time.Now()
	_, err := s.pool.Exec(ctx, `
		UPDATE users SET is_plus = true, plus_since = COALESCE(plus_since, $2), plus_expires_at = $3, updated_at = $2 WHERE id = $1`,
		userID, now, periodEnd)
	if err != nil {
		return err
	}
	// Upsert subscription record
	_, err = s.pool.Exec(ctx, `
		INSERT INTO subscriptions (id, user_id, type, price_cents, status, stripe_sub_id, current_period_start, current_period_end)
		VALUES ($1, $2, 'plus', 799, 'active', $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET status = 'active', current_period_end = $5`,
		"plus:"+userID, userID, stripeSubID, now, periodEnd)
	return err
}

func (s *PGStore) DeactivatePlus(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET is_plus = false, updated_at = NOW() WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE subscriptions SET status = 'cancelled', cancelled_at = NOW() WHERE user_id = $1 AND type = 'plus' AND status = 'active'`, userID)
	return err
}

// ---------- DJ Subscription Settings ----------

func (s *PGStore) GetDJSubSettings(ctx context.Context, djUserID string) (*models.DJSubSettings, error) {
	ds := &models.DJSubSettings{}
	err := s.pool.QueryRow(ctx, `SELECT user_id, price_cents, is_enabled, updated_at FROM dj_sub_settings WHERE user_id = $1`, djUserID).
		Scan(&ds.UserID, &ds.PriceCents, &ds.IsEnabled, &ds.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return ds, err
}

func (s *PGStore) UpsertDJSubSettings(ctx context.Context, djUserID string, priceCents int, isEnabled bool) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO dj_sub_settings (user_id, price_cents, is_enabled, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (user_id) DO UPDATE SET price_cents = $2, is_enabled = $3, updated_at = NOW()`,
		djUserID, priceCents, isEnabled)
	return err
}

// ---------- DJ Channel Subscriptions ----------

func (s *PGStore) SubscribeToDJ(ctx context.Context, userID, djUserID string, priceCents int, stripeSubID string) error {
	periodEnd := time.Now().AddDate(0, 1, 0)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO subscriptions (id, user_id, type, target_user_id, price_cents, status, stripe_sub_id, current_period_start, current_period_end)
		VALUES ($1, $2, 'dj_sub', $3, $4, 'active', $5, NOW(), $6)`,
		uuid.New().String(), userID, djUserID, priceCents, stripeSubID, periodEnd)
	return err
}

func (s *PGStore) GetDJSubscription(ctx context.Context, userID, djUserID string) (*models.Subscription, error) {
	sub := &models.Subscription{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, type, target_user_id, price_cents, status, current_period_start, current_period_end, cancelled_at, created_at
		FROM subscriptions WHERE user_id = $1 AND target_user_id = $2 AND type = 'dj_sub' AND status = 'active'
		LIMIT 1`, userID, djUserID).
		Scan(&sub.ID, &sub.UserID, &sub.Type, &sub.TargetUserID, &sub.PriceCents, &sub.Status,
			&sub.CurrentPeriodStart, &sub.CurrentPeriodEnd, &sub.CancelledAt, &sub.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return sub, err
}

func (s *PGStore) GetDJSubscriberCount(ctx context.Context, djUserID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM subscriptions WHERE target_user_id = $1 AND type = 'dj_sub' AND status = 'active'`, djUserID).Scan(&count)
	return count, err
}

// ---------- Neon Balance ----------

func (s *PGStore) CreditNeon(ctx context.Context, userID string, amount int, packID string, priceCents int, stripePaymentID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `UPDATE users SET neon_balance = neon_balance + $2, updated_at = NOW() WHERE id = $1`, userID, amount)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO neon_purchases (id, user_id, pack_id, neon_amount, price_cents, stripe_payment_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), userID, packID, amount, priceCents, stripePaymentID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PGStore) SpendNeon(ctx context.Context, fromUserID, toRoomID string, toDJUserID *string, amount int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Deduct from user
	tag, err := tx.Exec(ctx, `UPDATE users SET neon_balance = neon_balance - $2, updated_at = NOW() WHERE id = $1 AND neon_balance >= $2`, fromUserID, amount)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("insufficient neon balance")
	}

	// Record transaction
	_, err = tx.Exec(ctx, `
		INSERT INTO neon_transactions (id, from_user_id, to_room_id, to_dj_user_id, amount)
		VALUES ($1, $2, $3, $4, $5)`,
		uuid.New().String(), fromUserID, toRoomID, toDJUserID, amount)
	if err != nil {
		return err
	}

	// Update tube
	_, err = tx.Exec(ctx, `
		INSERT INTO neon_tubes (room_id, level, fill_amount, fill_target, total_neon, updated_at)
		VALUES ($1, 1, $2, 100, $3::bigint, NOW())
		ON CONFLICT (room_id) DO UPDATE SET
			fill_amount = neon_tubes.fill_amount + $2,
			total_neon = neon_tubes.total_neon + $3::bigint,
			updated_at = NOW()`,
		toRoomID, amount, int64(amount))
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *PGStore) GetNeonBalance(ctx context.Context, userID string) (int, error) {
	var balance int
	err := s.pool.QueryRow(ctx, `SELECT neon_balance FROM users WHERE id = $1`, userID).Scan(&balance)
	return balance, err
}

// ---------- Neon Tube ----------

func (s *PGStore) GetNeonTube(ctx context.Context, roomID string) (*models.NeonTube, error) {
	tube := &models.NeonTube{}
	err := s.pool.QueryRow(ctx, `SELECT room_id, level, fill_amount, fill_target, total_neon, updated_at FROM neon_tubes WHERE room_id = $1`, roomID).
		Scan(&tube.RoomID, &tube.Level, &tube.FillAmount, &tube.FillTarget, &tube.TotalNeon, &tube.UpdatedAt)
	if err == pgx.ErrNoRows {
		// Return default tube
		return &models.NeonTube{RoomID: roomID, Level: 1, FillAmount: 0, FillTarget: 100, TotalNeon: 0}, nil
	}
	return tube, err
}

func (s *PGStore) LevelUpTube(ctx context.Context, roomID string, newLevel int, newFillTarget int, overflow int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE neon_tubes SET level = $2, fill_amount = $4, fill_target = $3, updated_at = NOW()
		WHERE room_id = $1`,
		roomID, newLevel, newFillTarget, overflow)
	return err
}

// ---------- Creator Pool ----------

func (s *PGStore) ComputeCreatorPool(ctx context.Context, month string, poolPct int) (*models.CreatorPoolMonth, []models.CreatorPoolAllocation, error) {
	monthID := "pool:" + month

	// Count active plus subscribers during this month
	var totalRevenue int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) * 799 FROM subscriptions
		WHERE type = 'plus' AND status IN ('active','cancelled')
		AND current_period_start <= ($1 || '-01')::date + INTERVAL '1 month'
		AND current_period_end >= ($1 || '-01')::date`,
		month).Scan(&totalRevenue)
	if err != nil {
		return nil, nil, err
	}

	poolAmount := totalRevenue * poolPct / 100

	// Get Plus listening minutes per creator for the month
	// Only count minutes from users who were Plus during that period
	rows, err := s.pool.Query(ctx, `
		SELECT r.dj_session_id, COALESCE(SUM(le.duration_seconds)/60, 0) as minutes
		FROM listen_events le
		JOIN rooms r ON r.id = le.room_id
		JOIN users u ON u.id = le.user_id
		WHERE u.is_plus = true
		AND le.started_at >= ($1 || '-01')::date
		AND le.started_at < ($1 || '-01')::date + INTERVAL '1 month'
		AND le.duration_seconds > 0
		GROUP BY r.dj_session_id
		HAVING SUM(le.duration_seconds) > 0
		ORDER BY minutes DESC`, month)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type creatorMinutes struct {
		sessionID string
		minutes   int64
	}
	var creators []creatorMinutes
	var totalMinutes int64
	for rows.Next() {
		var cm creatorMinutes
		if err := rows.Scan(&cm.sessionID, &cm.minutes); err != nil {
			return nil, nil, err
		}
		// Anti-abuse: cap at 480 min/day ~= 14400 min/month per creator
		if cm.minutes > 14400 {
			cm.minutes = 14400
		}
		creators = append(creators, cm)
		totalMinutes += cm.minutes
	}

	// Save pool month
	now := time.Now()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO creator_pool_months (id, month, total_plus_revenue_cents, pool_pct, pool_amount_cents, total_plus_minutes, computed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (month) DO UPDATE SET total_plus_revenue_cents=$3, pool_pct=$4, pool_amount_cents=$5, total_plus_minutes=$6, computed_at=$7`,
		monthID, month, totalRevenue, poolPct, poolAmount, totalMinutes, now)
	if err != nil {
		return nil, nil, err
	}

	// Delete old allocations for this month
	if _, err := s.pool.Exec(ctx, `DELETE FROM creator_pool_allocations WHERE month_id = $1`, monthID); err != nil {
		return nil, nil, fmt.Errorf("delete old allocations: %w", err)
	}

	// Create allocations
	var allocations []models.CreatorPoolAllocation
	for _, cm := range creators {
		var sharePct float64
		var earnings int
		if totalMinutes > 0 {
			sharePct = float64(cm.minutes) / float64(totalMinutes) * 100
			earnings = int(float64(poolAmount) * float64(cm.minutes) / float64(totalMinutes))
		}
		alloc := models.CreatorPoolAllocation{
			ID:            uuid.New().String(),
			MonthID:       monthID,
			CreatorUserID: cm.sessionID,
			ListenMinutes: cm.minutes,
			SharePct:      sharePct,
			EarningsCents: earnings,
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO creator_pool_allocations (id, month_id, creator_user_id, listen_minutes, share_pct, earnings_cents)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			alloc.ID, alloc.MonthID, alloc.CreatorUserID, alloc.ListenMinutes, alloc.SharePct, alloc.EarningsCents); err != nil {
			log.Printf("[pool] failed to insert allocation for creator=%s: %v", alloc.CreatorUserID, err)
		}
		allocations = append(allocations, alloc)
	}

	pm := &models.CreatorPoolMonth{
		ID:                    monthID,
		Month:                 month,
		TotalPlusRevenueCents: totalRevenue,
		PoolPct:               poolPct,
		PoolAmountCents:       poolAmount,
		TotalPlusMinutes:      totalMinutes,
		ComputedAt:            &now,
	}

	return pm, allocations, nil
}
