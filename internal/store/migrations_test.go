package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jukebox/backend/internal/models"
)

// testMigrationsDir is the real migrations directory, relative to this package.
const testMigrationsDir = "../../migrations"

// TestListUpMigrations guards against the hardcoded-list regression: every
// *.up.sql on disk must be discovered, in order, including 013_chat_media.
func TestListUpMigrations(t *testing.T) {
	files, err := listUpMigrations(testMigrationsDir)
	if err != nil {
		t.Fatalf("listUpMigrations: %v", err)
	}
	if len(files) < 13 {
		t.Fatalf("want at least 13 migrations, got %d: %v", len(files), files)
	}
	if files[0] != "001_initial.up.sql" {
		t.Errorf("first migration = %s, want 001_initial.up.sql", files[0])
	}
	if !sort.StringsAreSorted(files) {
		t.Errorf("migrations not sorted: %v", files)
	}
	found013 := false
	for _, f := range files {
		if !strings.HasSuffix(f, ".up.sql") {
			t.Errorf("non-up migration listed: %s", f)
		}
		if f == "013_chat_media.up.sql" {
			found013 = true
		}
	}
	if !found013 {
		t.Error("013_chat_media.up.sql not discovered — chat media columns would never be created")
	}
}

// TestIsAlreadyAppliedError: legacy already-applied migrations must be
// classified by SQLSTATE code, never by message text — Postgres localizes
// error messages under non-English lc_messages, where "already exists"
// never appears.
func TestIsAlreadyAppliedError(t *testing.T) {
	byCode := func(code string) error {
		// A localized server message: string matching would misclassify it.
		return fmt.Errorf("exec: %w", &pgconn.PgError{Code: code, Message: "die Tabelle existiert bereits"})
	}
	for _, code := range []string{"42P07", "42701", "42710", "23505"} {
		if !isAlreadyAppliedError(byCode(code)) {
			t.Errorf("SQLSTATE %s: want already-applied, got not", code)
		}
	}
	// Other SQLSTATEs (e.g. syntax error) are real failures.
	if isAlreadyAppliedError(byCode("42601")) {
		t.Error("SQLSTATE 42601 (syntax_error) misclassified as already applied")
	}
	// A non-Postgres error whose text happens to contain the English words
	// must not match: classification is by code, not message.
	if isAlreadyAppliedError(errors.New(`ERROR: relation "rooms" already exists`)) {
		t.Error("plain error with 'already exists' text misclassified as already applied")
	}
	if isAlreadyAppliedError(nil) {
		t.Error("nil error misclassified as already applied")
	}
}

// newMigrationTestDB creates a throwaway database on the Postgres server
// referenced by TEST_DATABASE_URL and returns a PGStore connected to it.
// Skips the test when TEST_DATABASE_URL is unset (see `make test-migrations`).
// The database is dropped on cleanup.
func newMigrationTestDB(t *testing.T) *PGStore {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping migration integration test (run `make test-migrations`)")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	dbName := fmt.Sprintf("jukebox_migtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close(ctx)
		t.Fatalf("create database %s: %v", dbName, err)
	}

	cfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.Database = dbName
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", dbName, err)
		}
		admin.Close(ctx)
	})
	return &PGStore{pool: wrapPool(pool, defaultQueryTimeout)}
}

// assertChatWithMediaWorks proves the migrated schema supports the chat
// queries that reference media_url/media_type end to end.
func assertChatWithMediaWorks(t *testing.T, s *PGStore) {
	t.Helper()
	ctx := context.Background()

	room := &models.Room{
		ID:            "room-migtest",
		Slug:          "migtest",
		Name:          "Migration Test Room",
		Vibes:         []string{},
		RequestPolicy: models.RequestPolicyOpen,
		DJKeyHash:     "not-a-real-hash",
		CreatedAt:     time.Now(),
	}
	if err := s.CreateRoom(ctx, room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	msg := &models.ChatMessage{
		ID:          "msg-migtest",
		RoomID:      room.ID,
		SessionID:   "sess-migtest",
		Username:    "tester",
		AvatarColor: "#00ffcc",
		Message:     "check this out",
		Type:        models.ChatTypeMessage,
		Timestamp:   time.Now(),
		MediaURL:    "https://cdn.example.com/pic.gif",
		MediaType:   "gif",
	}
	if err := s.InsertChatMessage(ctx, msg); err != nil {
		t.Fatalf("InsertChatMessage: %v", err)
	}

	msgs, err := s.GetRecentChat(ctx, room.ID, 10)
	if err != nil {
		t.Fatalf("GetRecentChat: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("GetRecentChat returned %d messages, want 1", len(msgs))
	}
	if got := msgs[0]; got.MediaURL != msg.MediaURL || got.MediaType != msg.MediaType {
		t.Errorf("media round-trip: got (%q, %q), want (%q, %q)",
			got.MediaURL, got.MediaType, msg.MediaURL, msg.MediaType)
	}
}

func countAppliedMigrations(t *testing.T, s *PGStore) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

func migrationRecorded(t *testing.T, s *PGStore, filename string) bool {
	t.Helper()
	var recorded bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)`,
		filename).Scan(&recorded); err != nil {
		t.Fatalf("query schema_migrations for %s: %v", filename, err)
	}
	return recorded
}

// TestRunMigrationsFreshDB: on a brand-new database every migration applies,
// including 013, and the chat media columns work. A second run is a no-op.
func TestRunMigrationsFreshDB(t *testing.T) {
	s := newMigrationTestDB(t)
	ctx := context.Background()

	if err := s.RunMigrations(ctx, testMigrationsDir); err != nil {
		t.Fatalf("RunMigrations on fresh DB: %v", err)
	}

	files, err := listUpMigrations(testMigrationsDir)
	if err != nil {
		t.Fatalf("listUpMigrations: %v", err)
	}
	if n := countAppliedMigrations(t, s); n != len(files) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(files))
	}
	if !migrationRecorded(t, s, "013_chat_media.up.sql") {
		t.Error("013_chat_media.up.sql not recorded as applied")
	}

	assertChatWithMediaWorks(t, s)

	// Re-running must be a no-op.
	before := countAppliedMigrations(t, s)
	if err := s.RunMigrations(ctx, testMigrationsDir); err != nil {
		t.Fatalf("RunMigrations second run: %v", err)
	}
	if after := countAppliedMigrations(t, s); after != before {
		t.Errorf("second run changed schema_migrations rows: %d -> %d", before, after)
	}
}

// TestRunMigrationsConcurrent: two server replicas booting simultaneously
// must not race — without the advisory lock both see every migration
// unapplied, and the loser's schema_migrations INSERT fails with a
// duplicate-key error. With the lock, one run migrates and the other finds
// everything recorded.
func TestRunMigrationsConcurrent(t *testing.T) {
	s := newMigrationTestDB(t)
	ctx := context.Background()

	const replicas = 3
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.RunMigrations(ctx, testMigrationsDir)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent RunMigrations #%d: %v", i, err)
		}
	}

	files, err := listUpMigrations(testMigrationsDir)
	if err != nil {
		t.Fatalf("listUpMigrations: %v", err)
	}
	if n := countAppliedMigrations(t, s); n != len(files) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(files))
	}
	assertChatWithMediaWorks(t, s)
}

// TestRunMigrationsLegacyDB reproduces a production database migrated by the
// old hardcoded runner: 001–012 applied, no schema_migrations table, no 013.
// RunMigrations must backfill the bookkeeping and apply 013.
func TestRunMigrationsLegacyDB(t *testing.T) {
	s := newMigrationTestDB(t)
	ctx := context.Background()

	legacy := []string{
		"001_initial.up.sql", "002_user_accounts.up.sql", "003_messages_playlists.up.sql",
		"004_room_ended.up.sql", "005_admin.up.sql", "006_location_listen.up.sql",
		"007_stage_name.up.sql", "008_unique_stage_name.up.sql", "009_monetization.up.sql",
		"010_add_banned.up.sql", "011_autoplay.up.sql", "012_track_info_snippet.up.sql",
	}
	for _, f := range legacy {
		data, err := os.ReadFile(testMigrationsDir + "/" + f)
		if err != nil {
			t.Fatalf("read legacy migration %s: %v", f, err)
		}
		if _, err := s.pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("apply legacy migration %s: %v", f, err)
		}
	}

	if err := s.RunMigrations(ctx, testMigrationsDir); err != nil {
		t.Fatalf("RunMigrations on legacy DB: %v", err)
	}

	files, err := listUpMigrations(testMigrationsDir)
	if err != nil {
		t.Fatalf("listUpMigrations: %v", err)
	}
	if n := countAppliedMigrations(t, s); n != len(files) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(files))
	}
	if !migrationRecorded(t, s, "013_chat_media.up.sql") {
		t.Error("013_chat_media.up.sql not recorded as applied")
	}

	assertChatWithMediaWorks(t, s)
}

// TestRunMigrationsStrictAfterCutoff: a post-cutoff migration that collides
// with existing objects must fail loudly and must NOT be recorded as applied
// (its statements roll back atomically).
func TestRunMigrationsStrictAfterCutoff(t *testing.T) {
	s := newMigrationTestDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	sql := "CREATE TABLE strict_probe (id TEXT PRIMARY KEY);\nCREATE TABLE strict_probe_extra (id TEXT PRIMARY KEY);\n"
	if err := os.WriteFile(filepath.Join(dir, "099_strict.up.sql"), []byte(sql), 0o644); err != nil {
		t.Fatalf("write temp migration: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE TABLE strict_probe (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("pre-create colliding table: %v", err)
	}

	err := s.RunMigrations(ctx, dir)
	if err == nil {
		t.Fatal("RunMigrations succeeded; want 'already exists' failure for post-cutoff migration")
	}
	if !strings.Contains(err.Error(), "099_strict.up.sql") {
		t.Errorf("error does not name the failing migration: %v", err)
	}
	if migrationRecorded(t, s, "099_strict.up.sql") {
		t.Error("failed post-cutoff migration was recorded as applied")
	}
}

// TestMigration015BackfillsVerifiedAt: 015 must copy the newest used
// verification token's used_at onto users.verified_at, for verified AND
// admin-unverified users, and must ignore tokens that were only marked
// used by a resend (older token) — the newest row per user wins.
func TestMigration015BackfillsVerifiedAt(t *testing.T) {
	s := newMigrationTestDB(t)
	ctx := context.Background()

	// Apply everything before 015 from a temp copy of the migrations dir.
	pre := t.TempDir()
	files, err := listUpMigrations(testMigrationsDir)
	if err != nil {
		t.Fatalf("listUpMigrations: %v", err)
	}
	for _, f := range files {
		if f >= "015" {
			break
		}
		b, err := os.ReadFile(filepath.Join(testMigrationsDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pre, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RunMigrations(ctx, pre); err != nil {
		t.Fatalf("RunMigrations pre-015: %v", err)
	}

	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// u1: verified, resent once. Old token marked used at resend (+5s),
	// new token used at +15s. Backfill must pick +15s.
	mustExec(`INSERT INTO users (id, email, password_hash, display_name, avatar_color, created_at, updated_at, email_verified)
	          VALUES ('u1','u1@x.com','h','u1','c',$1,$1,TRUE)`, base)
	mustExec(`INSERT INTO email_verifications (id, user_id, token, expires_at, used_at, created_at)
	          VALUES ('t1a','u1','tok1a',$1,$2,$3)`, base.Add(24*time.Hour), base.Add(5*time.Second), base)
	mustExec(`INSERT INTO email_verifications (id, user_id, token, expires_at, used_at, created_at)
	          VALUES ('t1b','u1','tok1b',$1,$2,$3)`, base.Add(24*time.Hour), base.Add(15*time.Second), base.Add(5*time.Second))
	// u2: admin un-verified later, but token was used at +20m. Still backfilled.
	mustExec(`INSERT INTO users (id, email, password_hash, display_name, avatar_color, created_at, updated_at, email_verified)
	          VALUES ('u2','u2@x.com','h','u2','c',$1,$1,FALSE)`, base)
	mustExec(`INSERT INTO email_verifications (id, user_id, token, expires_at, used_at, created_at)
	          VALUES ('t2','u2','tok2',$1,$2,$3)`, base.Add(24*time.Hour), base.Add(20*time.Minute), base)
	// u3: never clicked. verified_at stays NULL.
	mustExec(`INSERT INTO users (id, email, password_hash, display_name, avatar_color, created_at, updated_at, email_verified)
	          VALUES ('u3','u3@x.com','h','u3','c',$1,$1,FALSE)`, base)
	mustExec(`INSERT INTO email_verifications (id, user_id, token, expires_at, created_at)
	          VALUES ('t3','u3','tok3',$1,$2)`, base.Add(24*time.Hour), base)

	if err := s.RunMigrations(ctx, testMigrationsDir); err != nil {
		t.Fatalf("RunMigrations full: %v", err)
	}
	if !migrationRecorded(t, s, "015_signup_forensics.up.sql") {
		t.Fatal("015 not recorded")
	}

	got := map[string]*time.Time{}
	rows, err := s.pool.Query(ctx, `SELECT id, verified_at FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var v *time.Time
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		got[id] = v
	}
	want := map[string]*time.Time{
		"u1": ptrTime(base.Add(15 * time.Second)),
		"u2": ptrTime(base.Add(20 * time.Minute)),
		"u3": nil,
	}
	for id, w := range want {
		g := got[id]
		switch {
		case w == nil && g != nil:
			t.Errorf("%s: verified_at = %v, want NULL", id, *g)
		case w != nil && g == nil:
			t.Errorf("%s: verified_at = NULL, want %v", id, *w)
		case w != nil && g != nil && !g.Equal(*w):
			t.Errorf("%s: verified_at = %v, want %v", id, *g, *w)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
