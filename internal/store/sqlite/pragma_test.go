package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenEnforcesForeignKeysOnEachConnection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "review.db"), Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.db.SetMaxOpenConns(2)
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_, err = store.db.ExecContext(ctx, `
		INSERT INTO review_units (
			id, run_id, unit_key, file_path, risk, status, attempt, created_at, updated_at
		) VALUES ('unit-orphan', 'missing-run', 'unit-key', 'file.go', 'low', 'pending', 0, '2026-08-22T00:00:00Z', '2026-08-22T00:00:00Z')`)
	if err == nil {
		t.Fatal("inserting a review unit without a run succeeded; foreign keys are not enforced")
	}
}

func TestOpenSetsBusyTimeoutOnEachConnection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "review.db"), Options{BusyTimeout: 75 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.db.SetMaxOpenConns(2)
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var timeout int
	if err := store.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 75 {
		t.Fatalf("busy_timeout = %d, want 75", timeout)
	}
}

func TestOpenEnablesWALJournalMode(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "review.db"), Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var mode string
	if err := store.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}
}
