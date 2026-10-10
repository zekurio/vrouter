package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Every counter is nonnegative and saturates, including when a provider sends
// implausibly large counts. SQLite's integer addition cannot overflow here.
func usageColumns() []string {
	return []string{"requests", "measured", "unpriced", "input_tokens", "output_tokens", "cached_tokens", "cost_input", "cost_read", "cost_write", "cost_output", "cost_saved"}
}

// usageCounts is what one request adds to its bucket, in usageColumns order.
// A request without a trustworthy usage report adds no tokens and no cost.
func usageCounts(r telemetryRecord) []any {
	if !r.UsageKnown {
		return []any{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	}
	cost, priced := usagePrice(r)
	unpriced := 1
	if priced {
		unpriced = 0
	}
	return []any{1, 1, unpriced, r.InputTokens, r.OutputTokens, min(r.CachedTokens, r.InputTokens), cost.input, cost.read, cost.write, cost.output, cost.saved}
}

// The key's name follows its latest request, so a rename shows up.
func usageUpsert() string {
	updates := make([]string, 0, len(usageColumns())+1)
	updates = append(updates, "key_name=excluded.key_name")
	for _, column := range usageColumns() {
		updates = append(updates, column+"=MIN("+column+"+excluded."+column+","+usageCounterCap+")")
	}
	return `INSERT INTO usage_buckets VALUES (?,?,?,?,?,?,?` + strings.Repeat(",?", len(usageColumns())) + `) ON CONFLICT(gateway,bucket,provider,model,key_id,speed) DO UPDATE SET ` + strings.Join(updates, ",")
}

// Delete the pending row in the same transaction as adding the totals. A
// repeated settlement cannot count twice.
func (s *usageStore) settle(ctx context.Context, r telemetryRecord, accepted bool) error {
	if err := validateTelemetryRecord(r); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	result, err := tx.ExecContext(ctx, `DELETE FROM usage_pending WHERE id=? AND gateway=?`, r.ID, r.GatewayID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	now := time.Now().UTC()
	bucket := []any{r.GatewayID, now.Unix() / usageBucketSeconds * usageBucketSeconds, r.Provider, r.Model, r.KeyID, r.Speed, r.KeyName}
	if _, err := tx.ExecContext(ctx, usageUpsert(), append(bucket, usageCounts(r)...)...); err != nil {
		return err
	}
	if accepted && r.KeyID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE usage_keys SET tokens=MIN(tokens+?,`+usageCounterCap+`) WHERE gateway=? AND key_id=?`, r.TotalTokens, r.GatewayID, r.KeyID); err != nil {
			return err
		}
	}
	if err := logRequest(ctx, tx, r, now); err != nil {
		return err
	}
	return tx.Commit()
}

// logRequest keeps the request for the Requests view and drops the oldest
// beyond the retention limit. The totals already hold what it added.
func logRequest(ctx context.Context, tx *sql.Tx, r telemetryRecord, now time.Time) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_requests VALUES (?,?,?,?)`, r.ID, r.GatewayID, now.UnixMilli(), string(raw)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_requests WHERE gateway=? AND id NOT IN (SELECT id FROM usage_requests WHERE gateway=? ORDER BY finished DESC,id DESC LIMIT ?)`, r.GatewayID, r.GatewayID, usageRequestRetention)
	return err
}

type keyUsage struct{ requests, tokens int64 }

func (s *usageStore) keys(ctx context.Context, gateway string) (map[string]keyUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_id,requests,tokens FROM usage_keys WHERE gateway=?`, gateway)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]keyUsage{}
	for rows.Next() {
		var id string
		var counts keyUsage
		if err := rows.Scan(&id, &counts.requests, &counts.tokens); err != nil {
			return nil, err
		}
		result[id] = counts
	}
	return result, rows.Err()
}
