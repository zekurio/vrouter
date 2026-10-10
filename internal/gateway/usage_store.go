package gateway

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // Registers the pure-Go SQLite driver.
)

const usageMetaSchema = `
CREATE TABLE IF NOT EXISTS usage_meta (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL);
INSERT OR IGNORE INTO usage_meta (id,version) VALUES (1,2);
`

// usage_buckets holds running totals for five minutes of one provider, model,
// key, and speed. Costs are nanodollars by the kind of token billed. Rows sit
// in key order, so a gateway's history reads oldest first without a sort.
// usage_requests is the bounded log behind the Requests view.
const usageSchema = `
CREATE TABLE IF NOT EXISTS usage_pending (id TEXT PRIMARY KEY, gateway TEXT NOT NULL, key_id TEXT NOT NULL, key_name TEXT NOT NULL, model TEXT NOT NULL, provider TEXT NOT NULL, started INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS usage_keys (gateway TEXT NOT NULL, key_id TEXT NOT NULL, requests INTEGER NOT NULL, tokens INTEGER NOT NULL, PRIMARY KEY(gateway,key_id));
CREATE TABLE IF NOT EXISTS usage_buckets (
 gateway TEXT NOT NULL, bucket INTEGER NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, key_id TEXT NOT NULL, speed TEXT NOT NULL,
 key_name TEXT NOT NULL,
 requests INTEGER NOT NULL, measured INTEGER NOT NULL, unpriced INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, cached_tokens INTEGER NOT NULL,
 cost_input INTEGER NOT NULL, cost_read INTEGER NOT NULL, cost_write INTEGER NOT NULL, cost_output INTEGER NOT NULL, cost_saved INTEGER NOT NULL,
 PRIMARY KEY(gateway,bucket,provider,model,key_id,speed)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS usage_requests (id TEXT PRIMARY KEY, gateway TEXT NOT NULL, finished INTEGER NOT NULL, record TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS usage_requests_gateway ON usage_requests(gateway,finished DESC);
`

const (
	usageVersion          = 2
	usageBucketSeconds    = int64(300)
	usageRequestRetention = 1000
	// Counters stop here instead of overflowing. It equals maxTokenCount.
	usageCounterCap = "4611686018427387904"
)

type usageStore struct {
	db *sql.DB
}

func openUsageStore(dir string) (*usageStore, error) {
	path := filepath.Join(dir, "usage.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := storeRefuseSymlink(path + suffix); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed filename in the private data directory
	if err != nil {
		return nil, err
	}
	if err := errors.Join(f.Chmod(0o600), f.Close()); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: abs}
	options := url.Values{"_pragma": {"busy_timeout(5000)", "synchronous(FULL)"}}
	dsn.RawQuery = options.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &usageStore{db: db}
	if err := s.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *usageStore) initialize() error {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL;`); err != nil {
		return err
	}
	if err := s.migrate(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, usageSchema); err != nil {
		return err
	}
	return s.recoverPending(ctx)
}

// Version 1 came from an unreleased build that kept less about each request,
// so its totals cannot be carried over. Per-key counters survive.
func (s *usageStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, usageMetaSchema); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM usage_meta WHERE id=1`).Scan(&version); err != nil {
		return err
	}
	switch version {
	case usageVersion:
		return nil
	case 1:
		slog.Warn("usage history from an unreleased build was reset")
		_, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS usage_buckets; DROP TABLE IF EXISTS usage_errors; UPDATE usage_meta SET version=2 WHERE id=1;`)
		return err
	default:
		return errors.New("gateway: unsupported usage database version")
	}
}

// A process that stops mid-response cannot claim success or known token usage.
func (s *usageStore) recoverPending(ctx context.Context) error {
	pending, err := s.pending(ctx)
	if err != nil {
		return err
	}
	for _, r := range pending {
		if err := s.settle(ctx, r, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *usageStore) pending(ctx context.Context) ([]telemetryRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,gateway,key_id,key_name,model,provider,started FROM usage_pending`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []telemetryRecord
	for rows.Next() {
		var r telemetryRecord
		var started int64
		if err := rows.Scan(&r.ID, &r.GatewayID, &r.KeyID, &r.KeyName, &r.Model, &r.Provider, &started); err != nil {
			return nil, err
		}
		r.StartedAt = time.UnixMilli(started).UTC()
		r.Outcome = outcomeIncomplete
		pending = append(pending, r)
	}
	return pending, rows.Err()
}

func (s *usageStore) begin(ctx context.Context, r telemetryRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_pending VALUES (?,?,?,?,?,?,?)`, r.ID, r.GatewayID, r.KeyID, r.KeyName, r.Model, r.Provider, r.StartedAt.UnixMilli()); err != nil {
		return err
	}
	if r.KeyID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_keys VALUES (?,?,1,0) ON CONFLICT(gateway,key_id) DO UPDATE SET requests=MIN(requests+1,4611686018427387904)`, r.GatewayID, r.KeyID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *usageStore) identify(ctx context.Context, id, model, provider string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE usage_pending SET model=?,provider=? WHERE id=?`, model, provider, id)
	return err
}
