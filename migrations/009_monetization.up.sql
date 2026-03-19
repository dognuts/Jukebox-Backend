-- ============================================================
-- Jukebox Monetization: Plus, DJ Subs, Neon Gifts, Creator Pool
-- ============================================================

-- User monetization columns
ALTER TABLE users ADD COLUMN IF NOT EXISTS is_plus BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN IF NOT EXISTS plus_since TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS plus_expires_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS neon_balance INT NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS stripe_customer_id TEXT NOT NULL DEFAULT '';

-- DJ subscription settings (one row per DJ user)
CREATE TABLE IF NOT EXISTS dj_sub_settings (
    user_id        TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    price_cents    INT NOT NULL DEFAULT 499,  -- $4.99 default
    is_enabled     BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Subscriptions (both Plus and DJ channel subs)
CREATE TABLE IF NOT EXISTS subscriptions (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type                TEXT NOT NULL,  -- 'plus' or 'dj_sub'
    target_user_id      TEXT,           -- DJ user ID for dj_sub, NULL for plus
    price_cents         INT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active',  -- active, cancelled, expired
    stripe_sub_id       TEXT NOT NULL DEFAULT '',
    current_period_start TIMESTAMPTZ NOT NULL,
    current_period_end  TIMESTAMPTZ NOT NULL,
    cancelled_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_subs_user ON subscriptions(user_id, type);
CREATE INDEX IF NOT EXISTS idx_subs_target ON subscriptions(target_user_id) WHERE target_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_subs_stripe ON subscriptions(stripe_sub_id) WHERE stripe_sub_id != '';

-- Neon purchase history
CREATE TABLE IF NOT EXISTS neon_purchases (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pack_id         TEXT NOT NULL,      -- e.g. 'starter', 'popular', 'mega'
    neon_amount     INT NOT NULL,
    price_cents     INT NOT NULL,
    stripe_payment_id TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_neon_purchases_user ON neon_purchases(user_id);

-- Neon transactions (gifts sent in rooms)
CREATE TABLE IF NOT EXISTS neon_transactions (
    id              TEXT PRIMARY KEY,
    from_user_id    TEXT NOT NULL REFERENCES users(id),
    to_room_id      TEXT NOT NULL REFERENCES rooms(id),
    to_dj_user_id   TEXT,             -- room creator (for earnings tracking)
    amount          INT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_neon_tx_room ON neon_transactions(to_room_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_neon_tx_dj ON neon_transactions(to_dj_user_id) WHERE to_dj_user_id IS NOT NULL;

-- Neon tube state per room (current level + fill progress)
CREATE TABLE IF NOT EXISTS neon_tubes (
    room_id         TEXT PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
    level           INT NOT NULL DEFAULT 1,     -- 1=cyan, 2=magenta, 3=amber, 4=rainbow
    fill_amount     INT NOT NULL DEFAULT 0,     -- 0-100
    fill_target     INT NOT NULL DEFAULT 100,   -- neon needed for this level
    total_neon      BIGINT NOT NULL DEFAULT 0,  -- lifetime total neon received
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Creator rewards pool (monthly allocations)
CREATE TABLE IF NOT EXISTS creator_pool_months (
    id              TEXT PRIMARY KEY,
    month           TEXT NOT NULL UNIQUE,  -- '2026-03' format
    total_plus_revenue_cents INT NOT NULL DEFAULT 0,
    pool_pct        INT NOT NULL DEFAULT 40,    -- percentage allocated
    pool_amount_cents INT NOT NULL DEFAULT 0,
    total_plus_minutes BIGINT NOT NULL DEFAULT 0,
    computed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-creator allocation within a month
CREATE TABLE IF NOT EXISTS creator_pool_allocations (
    id              TEXT PRIMARY KEY,
    month_id        TEXT NOT NULL REFERENCES creator_pool_months(id),
    creator_user_id TEXT NOT NULL REFERENCES users(id),
    listen_minutes  BIGINT NOT NULL DEFAULT 0,
    share_pct       NUMERIC(8,4) NOT NULL DEFAULT 0,
    earnings_cents  INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_pool_alloc_month ON creator_pool_allocations(month_id);
CREATE INDEX IF NOT EXISTS idx_pool_alloc_creator ON creator_pool_allocations(creator_user_id);

-- Anti-abuse: daily listen caps for pool tracking
-- (Uses existing listen_events table — cap is enforced in app logic)

-- Track room creator user ID for DJ subscriptions
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS creator_user_id TEXT DEFAULT '';
