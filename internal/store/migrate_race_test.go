package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentOpenOfV1Database opens one schema-v1 file from two stores at
// once, as `sessionhub serve` and `sessionhub machine add` can. The hook holds both after
// they read version 1, so without the re-read under the write lock both would
// run the v2 migration and the second would fail on a duplicate column.
func TestConcurrentOpenOfV1Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessionhub.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rollbackV3(t, s)
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE sessions DROP COLUMN state_ts"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	var barrier sync.WaitGroup
	barrier.Add(2)
	afterVersionRead = func() { barrier.Done(); barrier.Wait() }
	t.Cleanup(func() { afterVersionRead = func() {} })

	type result struct {
		st  *Store
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			st, err := Open(path, Options{})
			results <- result{st, err}
		}()
	}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Errorf("concurrent open failed: %v", r.err)
			continue
		}
		defer r.st.Close()
		var v int
		if err := r.st.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
			t.Errorf("user_version %d %v, want %d", v, err, schemaVersion)
		}
	}
}
