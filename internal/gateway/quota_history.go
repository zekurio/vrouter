package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// Quota history keeps what each account's provider reported for its usage
// windows, so the dashboard can draw a pool's allowance over time. A row is a
// run of identical reports, from when it was first seen to when it was last
// seen. Reports arrive only while vrouter reads quotas, for routing or for the
// dashboard, so a gap between runs is time nobody looked.
const (
	quotaHistoryRetention = 30 * 24 * time.Hour
	// Providers that report the time left rather than a reset time move the
	// reset by the request's latency. A later window resets hours later.
	quotaResetJitter = 5 * time.Minute
	// The most windows and days one history request may ask for.
	quotaHistoryWindows = 8
	quotaHistoryDays    = 30
)

// quotaRun is one row of history. Times are Unix milliseconds. ResetAt is
// zero when the window reported none.
type quotaRun struct {
	Since     int64   `json:"since"`
	Until     int64   `json:"until"`
	Remaining float64 `json:"remaining"`
	ResetAt   int64   `json:"resetAt,omitempty"`
}

// quotaWindowHistory is the history of one window of one account.
type quotaWindowHistory struct {
	AccountID string     `json:"accountId"`
	Window    string     `json:"window"`
	Runs      []quotaRun `json:"runs"`
}

// recordQuota adds a report to each window's history. A report that repeats
// the latest run only extends it. Runs past the retention are dropped.
func (s *usageStore) recordQuota(ctx context.Context, accountID string, windows []QuotaWindow, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	now := at.UnixMilli()
	for _, w := range windows {
		var reset int64
		if w.ResetAt != nil {
			reset = w.ResetAt.UnixMilli()
		}
		var last quotaRun
		err := tx.QueryRowContext(ctx, `SELECT since,remaining,reset_at FROM quota_history WHERE account_id=? AND window_id=? ORDER BY since DESC LIMIT 1`, accountID, w.ID).Scan(&last.Since, &last.Remaining, &last.ResetAt)
		switch {
		case err == nil && last.Remaining == w.Remaining && max(last.ResetAt-reset, reset-last.ResetAt) <= quotaResetJitter.Milliseconds():
			_, err = tx.ExecContext(ctx, `UPDATE quota_history SET until=MAX(until,?) WHERE account_id=? AND window_id=? AND since=?`, now, accountID, w.ID, last.Since)
		case err == nil || errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO quota_history VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING`, accountID, w.ID, now, now, w.Remaining, reset)
		}
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM quota_history WHERE until<?`, at.Add(-quotaHistoryRetention).UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// forgetQuota drops the history of a removed account.
func (s *usageStore) forgetQuota(ctx context.Context, accountID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM quota_history WHERE account_id=?`, accountID)
	return err
}

// quotaHistory returns the runs of the given accounts and windows that lasted
// until from or later, oldest first.
func (s *usageStore) quotaHistory(ctx context.Context, accounts, windows []string, from time.Time) ([]quotaWindowHistory, error) {
	history := []quotaWindowHistory{}
	if len(accounts) == 0 || len(windows) == 0 {
		return history, nil
	}
	// The lists travel as JSON arrays, so the statement stays fixed. String
	// slices always encode.
	accountList, _ := json.Marshal(accounts)
	windowList, _ := json.Marshal(windows)
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,window_id,since,until,remaining,reset_at FROM quota_history WHERE until>=? AND account_id IN (SELECT value FROM json_each(?)) AND window_id IN (SELECT value FROM json_each(?)) ORDER BY account_id,window_id,since`, from.UnixMilli(), string(accountList), string(windowList))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var account, window string
		var run quotaRun
		if err := rows.Scan(&account, &window, &run.Since, &run.Until, &run.Remaining, &run.ResetAt); err != nil {
			return nil, err
		}
		if n := len(history); n == 0 || history[n-1].AccountID != account || history[n-1].Window != window {
			history = append(history, quotaWindowHistory{AccountID: account, Window: window, Runs: []quotaRun{}})
		}
		last := &history[len(history)-1]
		last.Runs = append(last.Runs, run)
	}
	return history, rows.Err()
}

// recordQuota keeps a successful report. A failed write only costs the chart
// a point, so it is logged rather than returned.
func (s *server) recordQuota(ctx context.Context, accountID string, q quotaCache) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := s.store.data.usage.recordQuota(ctx, accountID, q.Windows, q.ObservedAt); err != nil {
		slog.Warn("quota history not recorded", "account", accountID, "error", err)
	}
}

// quotaHistoryHandler reports the windows named by ?window= over the last
// ?days= days, seven by default, for the accounts the user can see.
func (s *server) quotaHistoryHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	windows := query["window"]
	if len(windows) == 0 || len(windows) > quotaHistoryWindows || slices.ContainsFunc(windows, func(id string) bool { return id == "" || len(id) > 64 }) {
		writeJSON(w, 400, map[string]string{"error": "Name between one and eight quota windows"})
		return
	}
	days := 7
	if raw := query.Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > quotaHistoryDays {
			writeJSON(w, 400, map[string]string{"error": "Ask for between 1 and 30 days of quota history"})
			return
		}
		days = n
	}
	user := currentUser(r)
	var accounts []string
	for _, a := range s.store.snapshot().Accounts {
		if accountVisible(a, user.ID) {
			accounts = append(accounts, a.ID)
		}
	}
	from := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	history, err := s.store.data.usage.quotaHistory(r.Context(), accounts, windows, from)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "Quota history is unavailable. Try again shortly."})
		return
	}
	writeJSON(w, 200, map[string]any{"history": history})
}
