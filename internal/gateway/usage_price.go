package gateway

import (
	"cmp"
	"slices"
	"strings"
	"sync/atomic"
)

// Prices estimate what a request would cost at public API list prices. They
// are not subscription charges. vrouter reads them from the models.dev
// catalog (price_catalog.go) and falls back to the built-in table below for a
// model the catalog lacks, or before the catalog first loads. Each bucket
// keeps the estimate made at settlement. Only a bucket that had no price at
// all is priced later, once its model gets one.

// usageRate is in nanodollars per token, which is dollars per million tokens
// times 1000.
type usageRate struct {
	input, read, write, output int64
}

// priceTier replaces the standard rate for a prompt longer than above input
// tokens, cache reads and writes included.
type priceTier struct {
	above int64
	rate  usageRate
}

type modelPrice struct {
	base usageRate
	// tiers are ordered by their threshold, lowest first.
	tiers []priceTier
	// The listed rates of the premium speeds. Nil means the usual multiple of
	// the standard rate.
	fast, ultrafast *usageRate
}

// priceBook maps a provider channel, codex or claude, to its models' prices.
type priceBook map[string]map[string]modelPrice

// builtinPrices are the list prices of November 2026.
// https://platform.claude.com/docs/en/about-claude/pricing
// https://developers.openai.com/api/docs/pricing
func builtinPrices() priceBook {
	opus4 := modelPrice{base: usageRate{5000, 500, 6250, 25000}}
	openAILong := func(base, long usageRate) modelPrice {
		return modelPrice{base: base, tiers: []priceTier{{272000, long}}}
	}
	return priceBook{
		"claude": {
			"claude-fable-5-1":  {base: usageRate{10000, 250, 12500, 50000}},
			"claude-fable-5":    {base: usageRate{10000, 1000, 12500, 50000}},
			"claude-mythos-5-1": {base: usageRate{10000, 250, 12500, 50000}},
			"claude-mythos-5":   {base: usageRate{10000, 1000, 12500, 50000}},
			"claude-opus-5-5":   {base: usageRate{4000, 200, 5000, 20000}},
			"claude-opus-5":     opus4,
			"claude-opus-4-8":   opus4,
			"claude-opus-4-7":   opus4,
			"claude-opus-4-6":   opus4,
			"claude-opus-4-5":   opus4,
			"claude-opus-4-1":   {base: usageRate{15000, 1500, 18750, 75000}},
			"claude-opus-4":     {base: usageRate{15000, 1500, 18750, 75000}},
			"claude-sonnet-5-5": {base: usageRate{2000, 100, 2500, 10000}},
			"claude-sonnet-5":   {base: usageRate{2000, 200, 2500, 10000}},
			"claude-sonnet-4":   {base: usageRate{3000, 300, 3750, 15000}},
			"claude-haiku-5-5":  {base: usageRate{100, 10, 125, 500}, tiers: []priceTier{{100000, usageRate{500, 50, 625, 2500}}}},
			"claude-haiku-4-5":  {base: usageRate{1000, 100, 1250, 5000}},
		},
		"codex": {
			"gpt-6-astra":   openAILong(usageRate{10000, 1000, 12500, 50000}, usageRate{20000, 2000, 25000, 75000}),
			"gpt-6.1-sol":   openAILong(usageRate{2000, 100, 2500, 10000}, usageRate{4000, 200, 5000, 15000}),
			"gpt-6.1-terra": openAILong(usageRate{1000, 50, 1250, 5000}, usageRate{2000, 100, 2500, 7500}),
			"gpt-6-sol":     openAILong(usageRate{2000, 200, 2500, 10000}, usageRate{4000, 400, 5000, 15000}),
			"gpt-6-luna":    openAILong(usageRate{100, 10, 125, 500}, usageRate{200, 20, 250, 750}),
			"gpt-5.3-codex": {base: usageRate{1750, 175, 1750, 14000}},
		},
	}
}

// find returns a model's price and the length of the ID that matched: the
// whole model ID, or the longest listed ID that starts it before a dash, so a
// dated snapshot takes its model's price. Zero means no match.
func (b priceBook) find(channel, model string) (modelPrice, int) {
	channels := []string{channel}
	if channel == "" {
		channels = []string{"claude", "codex"}
	}
	var best modelPrice
	length := 0
	for _, c := range channels {
		for id, price := range b[c] {
			if (model == id || strings.HasPrefix(model, id+"-")) && len(id) > length {
				best, length = price, len(id)
			}
		}
	}
	return best, length
}

// usagePrices answers price lookups from the downloaded catalog and the
// built-in table. The catalog is swapped whole, so readers never lock.
type usagePrices struct {
	builtin priceBook
	remote  atomic.Pointer[priceBook]
	// missing asks the catalog watcher for an early refresh. It holds one
	// signal at most.
	missing chan struct{}
}

func newUsagePrices() *usagePrices {
	return &usagePrices{builtin: builtinPrices(), missing: make(chan struct{}, 1)}
}

// lookup prefers the closer match, and the catalog when both match as well.
func (p *usagePrices) lookup(channel, model string) (modelPrice, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return modelPrice{}, false
	}
	price, length := p.builtin.find(channel, model)
	if remote := p.remote.Load(); remote != nil {
		if found, n := remote.find(channel, model); n > 0 && n >= length {
			price, length = found, n
		}
	}
	return price, length > 0
}

// price estimates one request's cost. A request on a model without a price
// asks for an early catalog refresh.
func (p *usagePrices) price(r telemetryRecord) (usageCost, bool) {
	price, ok := p.lookup(r.Provider, firstNonEmpty(r.NativeModel, r.Model))
	if !ok {
		select {
		case p.missing <- struct{}{}:
		default:
		}
		return usageCost{}, false
	}
	return price.cost(r.InputTokens, r.CachedTokens, r.CacheWriteTokens, r.OutputTokens, r.Speed, true), true
}

// usageCost is one request's estimate by the kind of token billed. saved is
// what its cache reads would have added at the full input rate.
type usageCost struct{ input, read, write, output, saved int64 }

// cost prices a request's tokens. Cache reads and writes are subsets of the
// input count. tiered applies long-prompt prices, which only make sense for a
// single request.
func (m modelPrice) cost(input, cached, written, output int64, speed string, tiered bool) usageCost {
	rate := m.base
	if tiered {
		for _, tier := range m.tiers {
			if input > tier.above {
				rate = tier.rate
			}
		}
	}
	switch speed {
	case speedFast:
		rate = premiumRate(rate, m.base, m.fast, 2)
	case speedUltrafast:
		rate = premiumRate(rate, m.base, m.ultrafast, 6)
	}
	read := min(max(cached, 0), input)
	write := min(max(written, 0), input-read)
	return usageCost{
		input:  pricedTokens(input-read-write, rate.input),
		read:   pricedTokens(read, rate.read),
		write:  pricedTokens(write, rate.write),
		output: pricedTokens(output, rate.output),
		saved:  pricedTokens(read, rate.input-rate.read),
	}
}

// premiumRate is a speed tier's rate. A listed rate scales by how far the
// applied rate is from the standard one, so a long prompt at a premium speed
// pays both premiums. An unlisted rate is a multiple of the applied one.
func premiumRate(rate, base usageRate, listed *usageRate, multiplier int64) usageRate {
	if listed == nil {
		return usageRate{rate.input * multiplier, rate.read * multiplier, rate.write * multiplier, rate.output * multiplier}
	}
	scale := func(listed, applied, standard int64) int64 {
		if standard <= 0 || applied == standard {
			return listed
		}
		return listed * applied / standard
	}
	return usageRate{
		input:  scale(listed.input, rate.input, base.input),
		read:   scale(listed.read, rate.read, base.read),
		write:  scale(listed.write, rate.write, base.write),
		output: scale(listed.output, rate.output, base.output),
	}
}

func pricedTokens(tokens, rate int64) int64 {
	if tokens <= 0 || rate <= 0 {
		return 0
	}
	if tokens > maxTokenCount/rate {
		return maxTokenCount
	}
	return tokens * rate
}

// sortTiers orders tiers by their threshold, so the last that applies wins.
func sortTiers(tiers []priceTier) {
	slices.SortFunc(tiers, func(a, b priceTier) int { return cmp.Compare(a.above, b.above) })
}
