package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// models.dev lists the API prices of most providers. vrouter reads its OpenAI
// and Anthropic entries, keeps a trimmed copy in usage.sqlite so a restart
// without network still has them, and checks for changes twice a day.

const (
	defaultPriceCatalog  = "https://models.dev/api.json"
	priceCatalogMax      = 64 << 20
	priceRefreshInterval = 12 * time.Hour
	priceRetryInterval   = 30 * time.Minute
	// A request on a model without a price triggers a refresh at most this
	// often, so a new model gets its price within the hour.
	priceMissingInterval = time.Hour
	// maxListPrice bounds a catalog price in dollars per million tokens.
	maxListPrice = 100000.0
)

// priceCatalogSource reads VROUTER_PRICE_CATALOG: empty for models.dev, off
// for the built-in prices alone, or the URL of a catalog in the same format.
func priceCatalogSource(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "":
		return defaultPriceCatalog, nil
	case "off":
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", errors.New("VROUTER_PRICE_CATALOG must be an http(s) URL or off")
	}
	return parsed.String(), nil
}

type (
	modelsDevCost struct {
		Input      *float64 `json:"input,omitempty"`
		Output     *float64 `json:"output,omitempty"`
		CacheRead  *float64 `json:"cache_read,omitempty"`
		CacheWrite *float64 `json:"cache_write,omitempty"`
	}
	modelsDevTier struct {
		modelsDevCost

		Tier struct {
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tier"`
	}
	modelsDevPrice struct {
		modelsDevCost

		Tiers []modelsDevTier `json:"tiers,omitempty"`
	}
	modelsDevMode struct {
		Cost *modelsDevCost `json:"cost,omitempty"`
	}
	modelsDevExtras struct {
		Modes map[string]modelsDevMode `json:"modes,omitempty"`
	}
	modelsDevModel struct {
		Cost         *modelsDevPrice  `json:"cost,omitempty"`
		Experimental *modelsDevExtras `json:"experimental,omitempty"`
	}
	modelsDevProvider struct {
		Models map[string]modelsDevModel `json:"models"`
	}
	// modelsDevCatalog is the part of the catalog vrouter keeps, and the shape
	// of its stored copy.
	modelsDevCatalog struct {
		OpenAI    modelsDevProvider `json:"openai"`
		Anthropic modelsDevProvider `json:"anthropic"`
	}
)

// parseModelsDev reads a catalog's prices. A model whose entry does not
// parse is skipped, so one odd entry cannot discard the rest. It also returns
// the catalog trimmed to the priced models and the fields vrouter reads, for
// storage.
func parseModelsDev(raw []byte) (priceBook, []byte, error) {
	type providerEntries struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	var catalog struct {
		OpenAI    providerEntries `json:"openai"`
		Anthropic providerEntries `json:"anthropic"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, nil, errors.New("price catalog is not valid JSON")
	}
	book := priceBook{"codex": {}, "claude": {}}
	trimmed := modelsDevCatalog{OpenAI: modelsDevProvider{Models: map[string]modelsDevModel{}}, Anthropic: modelsDevProvider{Models: map[string]modelsDevModel{}}}
	for _, source := range []struct {
		channel string
		from    map[string]json.RawMessage
		to      map[string]modelsDevModel
	}{
		{"codex", catalog.OpenAI.Models, trimmed.OpenAI.Models},
		{"claude", catalog.Anthropic.Models, trimmed.Anthropic.Models},
	} {
		for id, entry := range source.from {
			if id == "" || len(id) > maxTelemetryField {
				continue
			}
			model := decodeModelsDevModel(entry)
			if price, ok := model.price(source.channel); ok {
				book[source.channel][strings.ToLower(id)] = price
				source.to[id] = model.trimmed()
			}
		}
	}
	if len(book["codex"])+len(book["claude"]) == 0 {
		return nil, nil, errors.New("price catalog lists no OpenAI or Anthropic prices")
	}
	stored, err := json.Marshal(trimmed)
	if err != nil {
		return nil, nil, err
	}
	return book, stored, nil
}

// decodeModelsDevModel reads one model's prices. Unreadable speed modes are
// dropped, and an unreadable price leaves the model without one.
func decodeModelsDevModel(raw json.RawMessage) modelsDevModel {
	var fields struct {
		Cost         json.RawMessage `json:"cost"`
		Experimental json.RawMessage `json:"experimental"`
	}
	var model modelsDevModel
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields.Cost, &model.Cost) != nil {
		return modelsDevModel{}
	}
	if len(fields.Experimental) > 0 && json.Unmarshal(fields.Experimental, &model.Experimental) != nil {
		model.Experimental = nil
	}
	return model
}

func (m modelsDevModel) price(channel string) (modelPrice, bool) {
	if m.Cost == nil {
		return modelPrice{}, false
	}
	base, ok := m.Cost.rate(channel)
	if !ok {
		return modelPrice{}, false
	}
	price := modelPrice{base: base}
	for _, tier := range m.Cost.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size <= 0 {
			continue
		}
		if rate, ok := tier.rate(channel); ok {
			price.tiers = append(price.tiers, priceTier{above: tier.Tier.Size, rate: rate})
		}
	}
	sortTiers(price.tiers)
	price.fast, price.ultrafast = m.mode(speedFast, channel), m.mode(speedUltrafast, channel)
	return price, true
}

func (m modelsDevModel) mode(name, channel string) *usageRate {
	if m.Experimental == nil || m.Experimental.Modes[name].Cost == nil {
		return nil
	}
	rate, ok := m.Experimental.Modes[name].Cost.rate(channel)
	if !ok {
		return nil
	}
	return &rate
}

// trimmed keeps the prices and the premium speed modes.
func (m modelsDevModel) trimmed() modelsDevModel {
	out := modelsDevModel{Cost: m.Cost}
	for _, name := range []string{speedFast, speedUltrafast} {
		if m.Experimental == nil || m.Experimental.Modes[name].Cost == nil {
			continue
		}
		if out.Experimental == nil {
			out.Experimental = &modelsDevExtras{Modes: map[string]modelsDevMode{}}
		}
		out.Experimental.Modes[name] = m.Experimental.Modes[name]
	}
	return out
}

// rate converts dollars per million tokens. A model without an input and an
// output price has none. A missing cache read costs the input rate. A missing
// cache write costs 1.25 times input at Anthropic, which always charges for
// writes, and the input rate at OpenAI, whose older models bill writes as
// input.
func (c modelsDevCost) rate(channel string) (usageRate, bool) {
	input, inputOK := listPrice(c.Input)
	output, outputOK := listPrice(c.Output)
	if !inputOK || !outputOK || input+output == 0 {
		return usageRate{}, false
	}
	rate := usageRate{input: input, read: input, write: input, output: output}
	if channel == "claude" {
		rate.write = input * 5 / 4
	}
	for _, field := range []struct {
		value *float64
		to    *int64
	}{{c.CacheRead, &rate.read}, {c.CacheWrite, &rate.write}} {
		if field.value == nil {
			continue
		}
		value, ok := listPrice(field.value)
		if !ok {
			return usageRate{}, false
		}
		*field.to = value
	}
	return rate, true
}

func listPrice(value *float64) (int64, bool) {
	if value == nil || math.IsNaN(*value) || *value < 0 || *value > maxListPrice {
		return 0, false
	}
	return int64(math.Round(*value * 1000)), true
}

// ---------------------------------------------------------------------------
// Stored copy

// storedPrices is the catalog copy in usage.sqlite. checked is when its
// source last confirmed it current.
type storedPrices struct {
	source, etag string
	checked      time.Time
	body         []byte
}

func (s *usageStore) loadPrices(ctx context.Context) (storedPrices, bool, error) {
	var stored storedPrices
	var checked int64
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT source,etag,checked,body FROM price_catalog WHERE id=1`).Scan(&stored.source, &stored.etag, &checked, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return stored, false, nil
	}
	if err != nil {
		return stored, false, err
	}
	stored.checked, stored.body = time.UnixMilli(checked), []byte(body)
	return stored, true, nil
}

func (s *usageStore) savePrices(ctx context.Context, p storedPrices) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO price_catalog (id,source,etag,checked,body) VALUES (1,?,?,?,?) ON CONFLICT(id) DO UPDATE SET source=excluded.source,etag=excluded.etag,checked=excluded.checked,body=excluded.body`, p.source, p.etag, p.checked.UnixMilli(), string(p.body))
	return err
}

func (s *usageStore) confirmPrices(ctx context.Context, checked time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE price_catalog SET checked=? WHERE id=1`, checked.UnixMilli())
	return err
}

// reprice prices the buckets that have no price yet once their model has
// one. native names the provider's model behind a public alias. A bucket does
// not keep cache writes apart from other input, so they cost the input rate,
// and long-prompt prices, which depend on one request's length, do not apply.
func (s *usageStore) reprice(ctx context.Context, native func(provider, model string) string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	updates, err := s.unpricedBuckets(ctx, tx, native)
	if err != nil {
		return 0, err
	}
	for _, u := range updates {
		values := append([]any{u.cost.input, u.cost.read, u.cost.write, u.cost.output, u.cost.saved}, u.key...)
		if _, err := tx.ExecContext(ctx, `UPDATE usage_buckets SET unpriced=0,cost_input=?,cost_read=?,cost_write=?,cost_output=?,cost_saved=? WHERE user_id=? AND bucket=? AND provider=? AND model=? AND key_id=? AND speed=? AND scope=?`, values...); err != nil {
			return 0, err
		}
	}
	return len(updates), tx.Commit()
}

// repricedBucket is the primary key of a bucket and its new cost.
type repricedBucket struct {
	key  []any
	cost usageCost
}

// unpricedBuckets prices the buckets without a price whose model has one.
func (s *usageStore) unpricedBuckets(ctx context.Context, tx *sql.Tx, native func(provider, model string) string) ([]repricedBucket, error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_id,bucket,provider,model,key_id,speed,scope,input_tokens,output_tokens,cached_tokens FROM usage_buckets WHERE measured>0 AND unpriced=measured`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var updates []repricedBucket
	for rows.Next() {
		var userID, provider, model, keyID, speed, scope string
		var bucket, input, output, cached int64
		if err := rows.Scan(&userID, &bucket, &provider, &model, &keyID, &speed, &scope, &input, &output, &cached); err != nil {
			return nil, err
		}
		if price, ok := s.prices.lookup(provider, native(provider, model)); ok {
			updates = append(updates, repricedBucket{key: []any{userID, bucket, provider, model, keyID, speed, scope}, cost: price.cost(input, cached, 0, output, speed, false)})
		}
	}
	return updates, rows.Err()
}

// ---------------------------------------------------------------------------
// Refresh

// priceWatch tracks the catalog refreshes.
type priceWatch struct {
	source, etag string
	// checked is when the source last confirmed the catalog current, tried
	// when vrouter last asked.
	checked, tried time.Time
}

// loadStoredPrices puts the stored catalog to use. A copy from another
// source still serves until the configured one answers.
func (s *server) loadStoredPrices(ctx context.Context, source string) priceWatch {
	if source == "" {
		return priceWatch{}
	}
	usage := s.store.data.usage
	stored, ok, err := usage.loadPrices(ctx)
	if err != nil || !ok {
		if err != nil {
			slog.Warn("stored price catalog unreadable", "error", err)
		}
		return priceWatch{source: source}
	}
	book, _, err := parseModelsDev(stored.body)
	if err != nil {
		slog.Warn("stored price catalog unreadable", "error", err)
		return priceWatch{source: source}
	}
	usage.prices.remote.Store(&book)
	if stored.source != source {
		return priceWatch{source: source}
	}
	return priceWatch{source: source, etag: stored.etag, checked: stored.checked}
}

// watchPrices keeps the catalog current and prices earlier usage whenever
// prices change. Without a source it only prices earlier usage once, which a
// release with new built-in prices may allow.
func (s *server) watchPrices(ctx context.Context, watch priceWatch) {
	s.repriceUsage(ctx)
	if watch.source == "" {
		return
	}
	missing := s.store.data.usage.prices.missing
	for {
		timer := time.NewTimer(watch.wait(time.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-missing:
			timer.Stop()
			if time.Since(watch.tried) < priceMissingInterval {
				continue
			}
		}
		if s.refreshPrices(ctx, &watch) {
			s.repriceUsage(ctx)
		}
	}
}

// wait is the time until the next refresh: twice a day, and half an hour
// after a failed attempt.
func (w priceWatch) wait(now time.Time) time.Duration {
	next := w.checked.Add(priceRefreshInterval)
	if w.tried.After(w.checked) {
		next = later(next, w.tried.Add(priceRetryInterval))
	}
	return max(next.Sub(now), 0)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// refreshPrices asks the source for a newer catalog and reports whether the
// prices changed.
func (s *server) refreshPrices(ctx context.Context, watch *priceWatch) bool {
	usage := s.store.data.usage
	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	watch.tried = time.Now()
	body, etag, err := s.fetchPriceCatalog(fetchCtx, watch.source, watch.etag)
	var book priceBook
	var trimmed []byte
	if err == nil && body != nil {
		book, trimmed, err = parseModelsDev(body)
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("price catalog not refreshed", "source", watch.source, "error", err)
		}
		return false
	}
	watch.checked = time.Now()
	if body == nil {
		// Not modified.
		if err := usage.confirmPrices(ctx, watch.checked); err != nil {
			slog.Warn("price catalog check not saved", "error", err)
		}
		return false
	}
	watch.etag = etag
	usage.prices.remote.Store(&book)
	if err := usage.savePrices(ctx, storedPrices{source: watch.source, etag: etag, checked: watch.checked, body: trimmed}); err != nil {
		slog.Warn("price catalog not saved", "error", err)
	}
	return true
}

// fetchPriceCatalog downloads the catalog. A nil body with no error means it
// has not changed since etag.
func (s *server) fetchPriceCatalog(ctx context.Context, source, etag string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vrouter")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := s.priceClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		if etag != "" {
			return nil, etag, nil
		}
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, priceCatalogMax+1))
		if err != nil {
			return nil, "", err
		}
		if len(body) > priceCatalogMax {
			return nil, "", errors.New("price catalog exceeds 64 MiB")
		}
		return body, boundedField(resp.Header.Get("ETag")), nil
	}
	return nil, "", fmt.Errorf("price catalog returned HTTP %d", resp.StatusCode)
}

// repriceUsage prices the earlier buckets whose models have prices now.
func (s *server) repriceUsage(ctx context.Context) {
	aliases := s.store.snapshot().Policy.Aliases
	native := func(provider, model string) string {
		for _, alias := range aliases[policyChannel(provider)] {
			if alias.Alias == model {
				return alias.Name
			}
		}
		return model
	}
	n, err := s.store.data.usage.reprice(ctx, native)
	switch {
	case err != nil && ctx.Err() == nil:
		slog.Warn("earlier unpriced usage not priced", "error", err)
	case n > 0:
		slog.Info("priced earlier unpriced usage", "buckets", n)
	}
}
