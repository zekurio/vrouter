package gateway

import "strings"

// Nanodollars per token, equivalent to dollars per million tokens * 1000.
// Freeze the estimated cost in each bucket at settlement. These are list-price
// estimates, not subscription charges. Rates match the October 2026 UI catalog.
// https://platform.claude.com/docs/en/about-claude/pricing
// https://developers.openai.com/api/docs/pricing

type usageRate struct {
	model                      string
	input, read, write, output int64
}

func usageRates() []usageRate {
	return []usageRate{
		{"claude-fable-5-1", 10000, 250, 12500, 50000},
		{"claude-fable-5", 10000, 1000, 12500, 50000},
		{"claude-opus-5-5", 4000, 200, 5000, 20000},
		{"claude-opus-5", 5000, 500, 6250, 25000},
		{"claude-opus-4-1", 15000, 1500, 18750, 75000},
		{"claude-opus-4", 5000, 500, 6250, 25000},
		{"claude-sonnet-5-5", 2000, 200, 2500, 10000},
		{"claude-sonnet-5", 2000, 200, 2500, 10000},
		{"claude-sonnet-4", 3000, 300, 3750, 15000},
		{"claude-haiku-5-5", 100, 10, 125, 500},
		{"claude-haiku-4-5", 1000, 100, 1250, 5000},
		{"gpt-6-astra", 10000, 1000, 10000, 50000},
		{"gpt-6.1-sol", 2000, 100, 2000, 10000},
		{"gpt-6-sol", 2000, 200, 2000, 10000},
		{"gpt-6-luna", 100, 10, 100, 500},
	}
}

// usageCost is one request's estimate by the kind of token billed. saved is
// what its cache reads would have added at the full input rate.
type usageCost struct{ input, read, write, output, saved int64 }

func usagePrice(r telemetryRecord) (usageCost, bool) {
	model := strings.ToLower(r.NativeModel)
	if model == "" {
		model = strings.ToLower(r.Model)
	}
	for _, rate := range usageRates() {
		if model != rate.model && !strings.HasPrefix(model, rate.model+"-") {
			continue
		}
		if rate.model == "claude-haiku-5-5" && r.InputTokens > 100000 {
			rate.input, rate.read, rate.write, rate.output = 500, 50, 625, 2500
		}
		multiplier := int64(1)
		switch r.Speed {
		case speedFast:
			multiplier = 2
		case speedUltrafast:
			multiplier = 6
		}
		// Cache reads and writes are subsets of the input count.
		read := min(r.CachedTokens, r.InputTokens)
		write := min(r.CacheWriteTokens, r.InputTokens-read)
		return usageCost{
			input:  pricedTokens(r.InputTokens-read-write, rate.input*multiplier),
			read:   pricedTokens(read, rate.read*multiplier),
			write:  pricedTokens(write, rate.write*multiplier),
			output: pricedTokens(r.OutputTokens, rate.output*multiplier),
			saved:  pricedTokens(read, (rate.input-rate.read)*multiplier),
		}, true
	}
	return usageCost{}, false
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
