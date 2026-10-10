package gateway

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // Zone names resolve on hosts without a zone database.
)

type (
	// usageTally is what every row of the Usage page shows. Cost is in dollars
	// at list prices, and tokens count input and output together.
	usageTally struct {
		Requests int64   `json:"requests"`
		Measured int64   `json:"measured"`
		Unpriced int64   `json:"unpriced"`
		Cost     float64 `json:"cost"`
		Tokens   int64   `json:"tokens"`
	}
	usageGroup struct {
		usageTally

		Name     string `json:"name"`
		Provider string `json:"provider"`
	}
	usageCostTypes struct {
		Input  float64 `json:"input"`
		Read   float64 `json:"read"`
		Write  float64 `json:"write"`
		Output float64 `json:"output"`
	}
	usageTotal struct {
		usageTally

		ByType usageCostTypes `json:"byType"`
		// Cost by speed tier, then by provider.
		ByTier map[string]map[string]float64 `json:"byTier"`
		// What the cached input would have cost at the full input rate.
		Saved    float64 `json:"saved"`
		Uncached int64   `json:"uncached"`
		Cached   int64   `json:"cached"`
		Output   int64   `json:"output"`
	}
	// usagePeriod is a day or an hour in the reader's time zone that saw
	// requests. Start is in milliseconds.
	usagePeriod struct {
		usageTally

		Start      int64              `json:"start"`
		ByProvider map[string]float64 `json:"byProvider"`
	}
	usageReport struct {
		Total     usageTotal   `json:"total"`
		Providers []usageGroup `json:"providers"`
		Models    []usageGroup `json:"models"`
		Keys      []usageGroup `json:"keys"`
		// Oldest first. Hours cover the last two days.
		Days  []usagePeriod `json:"days"`
		Hours []usagePeriod `json:"hours"`
		// When the first request finished, in milliseconds. Zero before any.
		First int64 `json:"first"`
		// The newest requests, newest first. The totals above cover every
		// request, including those the log has dropped.
		Requests       []telemetryRecord `json:"requests"`
		RetentionLimit int               `json:"retentionLimit"`
	}
)

// The hourly chart covers this much, plus a margin so the reader's first hour
// is whole.
const usageHourlySpan = 50 * time.Hour

// usageSum adds up bucket rows. Its fields follow usageColumns, and costs stay
// in nanodollars until the report is built.
type usageSum struct {
	requests, measured, unpriced, input, output, cached int64
	cost                                                usageCost
}

func (t *usageSum) scanFields() []any {
	return []any{&t.requests, &t.measured, &t.unpriced, &t.input, &t.output, &t.cached, &t.cost.input, &t.cost.read, &t.cost.write, &t.cost.output, &t.cost.saved}
}

func usageAdd(a, b int64) int64 { return clampTokenCount(saturatingAdd(a, b)) }

func (t *usageSum) add(o usageSum) {
	t.requests, t.measured, t.unpriced = usageAdd(t.requests, o.requests), usageAdd(t.measured, o.measured), usageAdd(t.unpriced, o.unpriced)
	t.input, t.output, t.cached = usageAdd(t.input, o.input), usageAdd(t.output, o.output), usageAdd(t.cached, o.cached)
	t.cost.input, t.cost.read = usageAdd(t.cost.input, o.cost.input), usageAdd(t.cost.read, o.cost.read)
	t.cost.write, t.cost.output = usageAdd(t.cost.write, o.cost.write), usageAdd(t.cost.output, o.cost.output)
	t.cost.saved = usageAdd(t.cost.saved, o.cost.saved)
}

func (t *usageSum) nanos() int64 {
	return usageAdd(usageAdd(t.cost.input, t.cost.read), usageAdd(t.cost.write, t.cost.output))
}

func usageDollars(nanos int64) float64 { return float64(nanos) / 1e9 }

func (t *usageSum) tally() usageTally {
	return usageTally{Requests: t.requests, Measured: t.measured, Unpriced: t.unpriced, Cost: usageDollars(t.nanos()), Tokens: usageAdd(t.input, t.output)}
}

type (
	usageGroupSum struct {
		usageSum

		name, provider string
	}
	usagePeriodSum struct {
		usageSum

		start      int64
		byProvider map[string]int64
	}
	// usageRow is one bucket row: five minutes of one provider, model, key, and
	// speed.
	usageRow struct {
		usageSum

		bucket                                 int64
		provider, model, keyID, speed, keyName string
	}
	// usageFold builds a report from rows read oldest first, the way the page
	// once summed the request log.
	usageFold struct {
		loc *time.Location
		// Buckets from here on also count toward an hour.
		hourly, dayEnd, first   int64
		total                   usageSum
		tiers                   map[string]map[string]int64
		providers, models, keys map[string]*usageGroupSum
		days, hours             []usagePeriodSum
	}
)

func newUsageFold(loc *time.Location) *usageFold {
	return &usageFold{
		loc:       loc,
		hourly:    time.Now().Add(-usageHourlySpan).Unix(),
		tiers:     map[string]map[string]int64{"standard": {}, speedFast: {}, speedUltrafast: {}},
		providers: map[string]*usageGroupSum{},
		models:    map[string]*usageGroupSum{},
		keys:      map[string]*usageGroupSum{},
	}
}

// usageGroupFor finds or starts a group. A model keeps the first provider
// that served it.
func usageGroupFor(groups map[string]*usageGroupSum, id, name, provider string) *usageGroupSum {
	group := groups[id]
	if group == nil {
		group = &usageGroupSum{}
		groups[id] = group
	}
	group.name = name
	if group.provider == "" {
		group.provider = provider
	}
	return group
}

func usageKeyLabel(id, name string) string {
	switch {
	case name != "":
		return name
	case id != "":
		return "Unnamed key"
	default:
		return "Server key"
	}
}

func (f *usageFold) add(r usageRow) {
	if f.first == 0 {
		f.first = r.bucket * 1000
	}
	cost := r.nanos()
	f.total.add(r.usageSum)
	// A request refused before routing has no provider. It still counts
	// toward its model, so models group by name alone.
	if r.provider != "" {
		usageGroupFor(f.providers, r.provider, r.provider, r.provider).add(r.usageSum)
		tier := r.speed
		if tier == "" {
			tier = "standard"
		}
		if costs, known := f.tiers[tier]; known && cost > 0 {
			costs[r.provider] = usageAdd(costs[r.provider], cost)
		}
	}
	model := r.model
	if model == "" {
		model = "Unknown model"
	}
	usageGroupFor(f.models, model, model, r.provider).add(r.usageSum)
	// Rows arrive oldest first, so a key ends up with its latest name.
	usageGroupFor(f.keys, r.keyID, usageKeyLabel(r.keyID, r.keyName), "").add(r.usageSum)
	f.addPeriods(r, cost)
}

// addPeriods counts the row toward its day and, when recent, its hour, in the
// reader's time zone. A period is complete once a later one starts.
func (f *usageFold) addPeriods(r usageRow, cost int64) {
	at := time.Unix(r.bucket, 0).In(f.loc)
	if r.bucket >= f.dayEnd {
		year, month, day := at.Date()
		start := time.Date(year, month, day, 0, 0, 0, 0, f.loc)
		f.dayEnd = start.AddDate(0, 0, 1).Unix()
		f.days = append(f.days, usagePeriodSum{start: start.UnixMilli(), byProvider: map[string]int64{}})
	}
	f.days[len(f.days)-1].count(r, cost)
	if r.bucket < f.hourly {
		return
	}
	// Counted back from the instant, so the repeated hour at the end of
	// daylight saving stays two hours.
	hour := (r.bucket - int64(at.Minute()*60+at.Second())) * 1000
	if n := len(f.hours); n == 0 || f.hours[n-1].start != hour {
		f.hours = append(f.hours, usagePeriodSum{start: hour, byProvider: map[string]int64{}})
	}
	f.hours[len(f.hours)-1].count(r, cost)
}

func (p *usagePeriodSum) count(r usageRow, cost int64) {
	p.add(r.usageSum)
	if r.provider != "" && cost > 0 {
		p.byProvider[r.provider] = usageAdd(p.byProvider[r.provider], cost)
	}
}

func usageCosts(nanos map[string]int64) map[string]float64 {
	costs := make(map[string]float64, len(nanos))
	for name, value := range nanos {
		costs[name] = usageDollars(value)
	}
	return costs
}

// usageRanked lists groups by cost, then tokens, then requests.
func usageRanked(sums map[string]*usageGroupSum) []usageGroup {
	groups := make([]usageGroup, 0, len(sums))
	for _, sum := range sums {
		groups = append(groups, usageGroup{usageTally: sum.tally(), Name: sum.name, Provider: sum.provider})
	}
	slices.SortFunc(groups, func(a, b usageGroup) int {
		return cmp.Or(cmp.Compare(b.Cost, a.Cost), cmp.Compare(b.Tokens, a.Tokens), cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Name, b.Name))
	})
	return groups
}

func usagePeriods(sums []usagePeriodSum) []usagePeriod {
	periods := make([]usagePeriod, 0, len(sums))
	for _, sum := range sums {
		periods = append(periods, usagePeriod{usageTally: sum.tally(), Start: sum.start, ByProvider: usageCosts(sum.byProvider)})
	}
	return periods
}

func (f *usageFold) report() usageReport {
	total := usageTotal{
		usageTally: f.total.tally(),
		ByType:     usageCostTypes{Input: usageDollars(f.total.cost.input), Read: usageDollars(f.total.cost.read), Write: usageDollars(f.total.cost.write), Output: usageDollars(f.total.cost.output)},
		ByTier:     map[string]map[string]float64{},
		Saved:      usageDollars(f.total.cost.saved),
		// Cache reads are a subset of the input count.
		Uncached: f.total.input - f.total.cached,
		Cached:   f.total.cached,
		Output:   f.total.output,
	}
	for tier, costs := range f.tiers {
		total.ByTier[tier] = usageCosts(costs)
	}
	return usageReport{
		Total:          total,
		Providers:      usageRanked(f.providers),
		Models:         usageRanked(f.models),
		Keys:           usageRanked(f.keys),
		Days:           usagePeriods(f.days),
		Hours:          usagePeriods(f.hours),
		First:          f.first,
		RetentionLimit: usageRequestRetention,
	}
}

// report totals every request the gateway has finished. The totals never
// shrink: they come from the buckets, not from the bounded request log. One
// pass in primary-key order reads every bucket.
func (s *usageStore) report(ctx context.Context, gateway string, loc *time.Location) (usageReport, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return usageReport{}, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only transaction
	fold := newUsageFold(loc)
	if err := readUsageRows(ctx, tx, gateway, fold); err != nil {
		return usageReport{}, err
	}
	report := fold.report()
	if report.Requests, err = readUsageRequests(ctx, tx, gateway); err != nil {
		return usageReport{}, err
	}
	return report, tx.Commit()
}

func readUsageRows(ctx context.Context, tx *sql.Tx, gateway string, fold *usageFold) error {
	//nolint:gosec // usageColumns holds only fixed schema columns; the gateway is a bound parameter.
	rows, err := tx.QueryContext(ctx, `SELECT bucket,provider,model,key_id,speed,key_name,`+strings.Join(usageColumns(), ",")+` FROM usage_buckets WHERE gateway=? ORDER BY bucket`, gateway)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var row usageRow
		fields := append([]any{&row.bucket, &row.provider, &row.model, &row.keyID, &row.speed, &row.keyName}, row.scanFields()...)
		if err := rows.Scan(fields...); err != nil {
			return err
		}
		fold.add(row)
	}
	return rows.Err()
}

func readUsageRequests(ctx context.Context, tx *sql.Tx, gateway string) ([]telemetryRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT record FROM usage_requests WHERE gateway=? ORDER BY finished DESC,id DESC LIMIT ?`, gateway, usageRequestRetention)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []telemetryRecord{}
	for rows.Next() {
		var raw string
		var record telemetryRecord
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// usageZone resolves the reader's time zone, which decides where a day
// starts. An unknown name reads as UTC.
func usageZone(name string) *time.Location {
	if len(name) > 64 {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

func (s *server) usageHandler(w http.ResponseWriter, r *http.Request) {
	report, err := s.store.data.usage.report(r.Context(), s.gatewayID, usageZone(r.URL.Query().Get("tz")))
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "Usage accounting is unavailable. Try again shortly."})
		return
	}
	writeJSON(w, 200, report)
}
