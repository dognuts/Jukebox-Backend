package store

import (
	"context"
	"time"
)

// ==================== Admin Metrics ====================

type DailyCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type GenreCount struct {
	Genre string `json:"genre"`
	Count int    `json:"count"`
}

type MetricsSummary struct {
	TotalUsers       int    `json:"totalUsers"`
	TotalRooms       int    `json:"totalRooms"`
	LiveRooms        int    `json:"liveRooms"`
	TotalListenHours float64 `json:"totalListenHours"`
	PlusMembers      int    `json:"plusMembers"`
	SignupsToday     int    `json:"signupsToday"`
	RoomsCreatedToday int   `json:"roomsCreatedToday"`
}

// GetMetricsSummary returns high-level counts for the admin dashboard.
func (s *PGStore) GetMetricsSummary(ctx context.Context) (*MetricsSummary, error) {
	m := &MetricsSummary{}

	s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&m.TotalUsers)
	s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM rooms`).Scan(&m.TotalRooms)
	s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM rooms WHERE is_live = true`).Scan(&m.LiveRooms)
	s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE is_plus = true`).Scan(&m.PlusMembers)
	s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE created_at >= CURRENT_DATE`).Scan(&m.SignupsToday)
	s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM rooms WHERE created_at >= CURRENT_DATE`).Scan(&m.RoomsCreatedToday)

	// Total listen hours from listen_events
	var totalMinutes float64
	s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(EXTRACT(EPOCH FROM (COALESCE(ended_at, NOW()) - started_at)) / 3600), 0)
		FROM listen_events
		WHERE started_at > NOW() - INTERVAL '30 days'
	`).Scan(&totalMinutes)
	m.TotalListenHours = totalMinutes

	return m, nil
}

// GetSignupsPerDay returns daily signup counts for the last N days.
func (s *PGStore) GetSignupsPerDay(ctx context.Context, days int) ([]DailyCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d::date::text AS date, COUNT(u.id) AS count
		FROM generate_series(
			CURRENT_DATE - ($1 || ' days')::interval,
			CURRENT_DATE,
			'1 day'
		) d
		LEFT JOIN users u ON u.created_at::date = d::date
		GROUP BY d::date
		ORDER BY d::date ASC`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []DailyCount
	for rows.Next() {
		var dc DailyCount
		if err := rows.Scan(&dc.Date, &dc.Count); err != nil {
			return nil, err
		}
		result = append(result, dc)
	}
	return result, nil
}

// GetRoomsCreatedPerDay returns daily room creation counts for the last N days.
func (s *PGStore) GetRoomsCreatedPerDay(ctx context.Context, days int) ([]DailyCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d::date::text AS date, COUNT(r.id) AS count
		FROM generate_series(
			CURRENT_DATE - ($1 || ' days')::interval,
			CURRENT_DATE,
			'1 day'
		) d
		LEFT JOIN rooms r ON r.created_at::date = d::date
		GROUP BY d::date
		ORDER BY d::date ASC`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []DailyCount
	for rows.Next() {
		var dc DailyCount
		if err := rows.Scan(&dc.Date, &dc.Count); err != nil {
			return nil, err
		}
		result = append(result, dc)
	}
	return result, nil
}

// GetActiveRoomsPerDay returns daily peak live room counts for the last N days.
// Since we don't have a time-series of live counts, we count rooms that were
// live at some point on each day (created before end of day, ended after start of day or still live).
func (s *PGStore) GetActiveRoomsPerDay(ctx context.Context, days int) ([]DailyCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d::date::text AS date, COUNT(DISTINCT r.id) AS count
		FROM generate_series(
			CURRENT_DATE - ($1 || ' days')::interval,
			CURRENT_DATE,
			'1 day'
		) d
		LEFT JOIN rooms r ON r.created_at::date <= d::date
			AND (r.ended_at IS NULL OR r.ended_at::date >= d::date)
			AND r.is_live = true OR r.ended_at IS NOT NULL
		GROUP BY d::date
		ORDER BY d::date ASC`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []DailyCount
	for rows.Next() {
		var dc DailyCount
		if err := rows.Scan(&dc.Date, &dc.Count); err != nil {
			return nil, err
		}
		result = append(result, dc)
	}
	return result, nil
}

// GetTopGenres returns the most popular genres by room count.
func (s *PGStore) GetTopGenres(ctx context.Context, limit int) ([]GenreCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT genre, COUNT(*) AS count
		FROM rooms
		WHERE genre != ''
		GROUP BY genre
		ORDER BY count DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []GenreCount
	for rows.Next() {
		var gc GenreCount
		if err := rows.Scan(&gc.Genre, &gc.Count); err != nil {
			return nil, err
		}
		result = append(result, gc)
	}
	return result, nil
}

// GetListenHoursPerDay returns daily listen hours for the last N days.
func (s *PGStore) GetListenHoursPerDay(ctx context.Context, days int) ([]DailyCount, error) {
	type DailyFloat struct {
		Date  string
		Hours float64
	}

	rows, err := s.pool.Query(ctx, `
		SELECT d::date::text AS date,
			COALESCE(SUM(EXTRACT(EPOCH FROM (
				LEAST(COALESCE(le.ended_at, NOW()), d::date + '1 day'::interval) -
				GREATEST(le.started_at, d::date)
			)) / 3600), 0)::int AS hours
		FROM generate_series(
			CURRENT_DATE - ($1 || ' days')::interval,
			CURRENT_DATE,
			'1 day'
		) d
		LEFT JOIN listen_events le ON le.started_at < d::date + '1 day'::interval
			AND COALESCE(le.ended_at, NOW()) > d::date
		GROUP BY d::date
		ORDER BY d::date ASC`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []DailyCount
	for rows.Next() {
		var dc DailyCount
		if err := rows.Scan(&dc.Date, &dc.Count); err != nil {
			return nil, err
		}
		result = append(result, dc)
	}
	return result, nil
}

// GetPeakListenerCounts returns peak concurrent listeners per day.
// This is approximate — uses the max listener set size at query time for current rooms.
func (s *PGStore) GetRecentPeakListeners(ctx context.Context) (int, error) {
	// Count current unique listeners across all live rooms
	var count int
	rows, err := s.pool.Query(ctx, `SELECT id FROM rooms WHERE is_live = true`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	// Note: actual peak tracking would need a time-series store.
	// For now we just report current live listener count.
	_ = count
	return 0, nil // placeholder — would need Redis SUNIONSTORE across all listener sets
}

// GetNewUsersThisWeek returns signup count for the current week.
func (s *PGStore) GetNewUsersThisWeek(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM users
		WHERE created_at >= DATE_TRUNC('week', CURRENT_DATE)
	`).Scan(&count)
	return count, err
}

// StartOfDay returns midnight for a given time.
func StartOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
