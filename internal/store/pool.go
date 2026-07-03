package store

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool sizing / timeout defaults. Every value is overridable via env (see
// newPoolConfig) so small and large hosts can be tuned without a rebuild.
const (
	// defaultQueryTimeout is the client-side deadline applied to any
	// Exec/Query/QueryRow whose caller didn't set one (hubs and background
	// services use context.Background(); HTTP request contexts carry no
	// deadline either).
	defaultQueryTimeout = 10 * time.Second
	// defaultStatementTimeout is the server-side backstop: Postgres kills
	// any statement running longer than this, so a hung query can never pin
	// a pooled connection forever.
	defaultStatementTimeout = 30 * time.Second
	defaultConnectTimeout   = 5 * time.Second
	defaultMinConns         = 2
	defaultMaxConnLifetime  = 30 * time.Minute
	defaultMaxConnIdleTime  = 5 * time.Minute
	defaultHealthCheck      = time.Minute
)

// defaultMaxConns sizes the pool for the whole process — HTTP handlers, every
// room hub, the sync service and the idle monitor all share it. The pgx
// default of max(4, NumCPU) starves a 1-2 vCPU host.
func defaultMaxConns() int32 {
	n := int32(4 * runtime.NumCPU())
	if n < 8 {
		n = 8
	}
	return n
}

func envInt32(key string, fallback int32) int32 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			return int32(n)
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// newPoolConfig parses databaseURL and applies explicit pool tuning instead of
// relying on pgx defaults. Values already present in the URL (e.g.
// pool_max_conns, connect_timeout, statement_timeout) win over env/defaults.
func newPoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.ParseConfig: %w", err)
	}

	cfg.MaxConns = envInt32("PG_MAX_CONNS", defaultMaxConns())
	cfg.MinConns = envInt32("PG_MIN_CONNS", defaultMinConns)
	cfg.MaxConnLifetime = envDuration("PG_MAX_CONN_LIFETIME", defaultMaxConnLifetime)
	cfg.MaxConnIdleTime = envDuration("PG_MAX_CONN_IDLE_TIME", defaultMaxConnIdleTime)
	cfg.HealthCheckPeriod = envDuration("PG_HEALTH_CHECK_PERIOD", defaultHealthCheck)

	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = envDuration("PG_CONNECT_TIMEOUT", defaultConnectTimeout)
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !ok {
		st := envDuration("PG_STATEMENT_TIMEOUT", defaultStatementTimeout)
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(st.Milliseconds(), 10)
	}
	return cfg, nil
}

// dbPool wraps pgxpool.Pool so every Exec/Query/QueryRow gets a default
// deadline when the caller didn't set one. The deadline also bounds how long
// an operation can block acquiring a connection from an exhausted pool.
// Begin/Ping/Close pass through via embedding — transactions keep the
// caller's context and rely on the server-side statement_timeout backstop.
type dbPool struct {
	*pgxpool.Pool
	queryTimeout time.Duration
}

func wrapPool(pool *pgxpool.Pool, queryTimeout time.Duration) *dbPool {
	return &dbPool{Pool: pool, queryTimeout: queryTimeout}
}

// opCtx applies the default per-query timeout unless the caller already
// carries a deadline.
func (p *dbPool) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.queryTimeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, p.queryTimeout)
}

func (p *dbPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	ctx, cancel := p.opCtx(ctx)
	defer cancel()
	return p.Pool.Exec(ctx, sql, args...)
}

func (p *dbPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	ctx, cancel := p.opCtx(ctx)
	rows, err := p.Pool.Query(ctx, sql, args...)
	if err != nil {
		cancel()
		return nil, err
	}
	// The deadline must survive until the caller finishes reading the rows;
	// cancel when they Close (all store methods defer rows.Close()).
	return &cancelRows{Rows: rows, cancel: cancel}, nil
}

func (p *dbPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	ctx, cancel := p.opCtx(ctx)
	// pgx defers query execution to Scan, so cancel there.
	return &cancelRow{row: p.Pool.QueryRow(ctx, sql, args...), cancel: cancel}
}

// cancelRows releases the per-query context when the rows are closed.
type cancelRows struct {
	pgx.Rows
	cancel context.CancelFunc
}

func (r *cancelRows) Close() {
	r.Rows.Close()
	r.cancel()
}

// cancelRow releases the per-query context once the row has been scanned.
type cancelRow struct {
	row    pgx.Row
	cancel context.CancelFunc
}

func (r *cancelRow) Scan(dest ...any) error {
	defer r.cancel()
	return r.row.Scan(dest...)
}
