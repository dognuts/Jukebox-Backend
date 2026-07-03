package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
