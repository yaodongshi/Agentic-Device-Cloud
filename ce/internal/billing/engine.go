// Package billing implements the FR-016 billing engine (design/82 B3.1).
// Usage windows are aggregated from adc_usage_events (see pkg/metering
// for the producer side) and priced with the doc/04 three-tier formula:
//
//   - platform subscription: 39800 yuan/year amortized monthly, the first
//     20 devices included (doc/04 section 5, EE SaaS row)
//   - device ladder: 400/300/250 yuan per device per year for devices
//     21-100 / 101-500 / 501+, billed on the natural-month daily peak
//     (doc/04 7.2 "按自然月峰值计")
//   - LLM tokens: 0.10 yuan per 1000 tokens (doc/04 section 5)
//
// All monetary amounts are integer fen (1 yuan = 100 fen) so the
// arithmetic never touches floating point. Every price is an assumption
// pending the doc/04 section 10 pricing experiments.
package billing

import (
	"context"
	"errors"
	"time"
)

// Pricing constants, doc/04 section 5 (EE SaaS row), expressed in fen.
const (
	// SubscriptionAnnualFen is the EE SaaS platform subscription,
	// 39800 yuan/year, including the first IncludedDevices devices.
	SubscriptionAnnualFen int64 = 39800 * 100
	// IncludedDevices is the device count covered by the subscription.
	IncludedDevices = 20
	// Device ladder prices per device per year, in fen.
	DeviceTier400Fen int64 = 400 * 100 // devices 21-100
	DeviceTier300Fen int64 = 300 * 100 // devices 101-500
	DeviceTier250Fen int64 = 250 * 100 // devices 501+
	// Ladder tier boundaries (inclusive upper bounds, doc/04 section 5).
	tier400UpperBound = 100
	tier300UpperBound = 500
	// TokenPriceFenPerK is the LLM token unit price: 0.10 yuan per 1000
	// tokens = 10 fen per 1000 tokens.
	TokenPriceFenPerK int64 = 10
	// MonthsPerYear amortizes annual fees into monthly statements.
	MonthsPerYear int64 = 12
)

// DeviceBreakdown records how the billable device count (the period's
// daily-peak maximum, doc/04 7.2) distributes over the ladder tiers.
type DeviceBreakdown struct {
	IncludedDevices int   `json:"included_devices"` // covered by the subscription
	Tier400Devices  int   `json:"tier_400_devices"` // devices 21-100
	Tier300Devices  int   `json:"tier_300_devices"` // devices 101-500
	Tier250Devices  int   `json:"tier_250_devices"` // devices 501+
	AnnualFeeFen    int64 `json:"annual_device_fee_fen"`
	MonthlyFeeFen   int64 `json:"monthly_device_fee_fen"`
}

// deviceLadder slices the billable device peak into the doc/04 tiers and
// prices it; the annual total is amortized to the statement month.
func deviceLadder(count int) DeviceBreakdown {
	var b DeviceBreakdown
	if count <= 0 {
		return b
	}
	if count <= IncludedDevices {
		b.IncludedDevices = count
		return b
	}
	b.IncludedDevices = IncludedDevices
	rest := count - IncludedDevices
	if rest <= tier400UpperBound-IncludedDevices {
		b.Tier400Devices = rest
	} else {
		b.Tier400Devices = tier400UpperBound - IncludedDevices
		rest -= b.Tier400Devices
		if rest <= tier300UpperBound-tier400UpperBound {
			b.Tier300Devices = rest
		} else {
			b.Tier300Devices = tier300UpperBound - tier400UpperBound
			b.Tier250Devices = rest - b.Tier300Devices
		}
	}
	b.AnnualFeeFen = int64(b.Tier400Devices)*DeviceTier400Fen +
		int64(b.Tier300Devices)*DeviceTier300Fen +
		int64(b.Tier250Devices)*DeviceTier250Fen
	b.MonthlyFeeFen = amortizeMonthly(b.AnnualFeeFen)
	return b
}

// amortizeMonthly splits an annual fen amount across months, rounding the
// fen half up so the twelve monthly statements never under-sum the year.
func amortizeMonthly(annualFen int64) int64 {
	q, r := annualFen/MonthsPerYear, annualFen%MonthsPerYear
	if 2*r >= MonthsPerYear {
		q++
	}
	return q
}

// SubscriptionMonthlyFen is the monthly amortized subscription fee:
// 39800 yuan/year -> 3316.67 yuan/month (331667 fen).
func SubscriptionMonthlyFen() int64 {
	return amortizeMonthly(SubscriptionAnnualFen)
}

// tokenFeeFen prices tokens at TokenPriceFenPerK fen per 1000 tokens,
// rounding the fen result half up (0.10 yuan per 1000 = 0.01 fen per
// token).
func tokenFeeFen(tokens int64) int64 {
	if tokens <= 0 {
		return 0
	}
	num := tokens * TokenPriceFenPerK
	q, r := num/1000, num%1000
	if 2*r >= 1000 {
		q++
	}
	return q
}

// Shanghai returns the business calendar location: billing periods follow
// adc_tenants.timezone (default 'Asia/Shanghai', migration 0001). China
// observes no DST, so a fixed zone is a safe fallback when tzdata is
// unavailable.
func Shanghai() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("Asia/Shanghai", 8*3600)
}

// MonthRange returns the half-open [from, to) window of the given month
// in loc, aligned to the month's first second.
func MonthRange(year, month int, loc *time.Location) (time.Time, time.Time) {
	from := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc)
	return from, from.AddDate(0, 1, 0)
}

// DayRange returns the half-open [from, to) window of one business day
// (daily usage aggregation, design/82 B3.1).
func DayRange(year, month, day int, loc *time.Location) (time.Time, time.Time) {
	from := time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
	return from, from.AddDate(0, 0, 1)
}

// ValidPeriod rejects out-of-range billing months.
func ValidPeriod(year, month int) bool {
	return year >= 2000 && year <= 2200 && month >= 1 && month <= 12
}

// Engine turns aggregated usage into a priced statement. The aggregation
// function is injected so tests can feed synthetic summaries without a
// database; NewPGAggregator is the production implementation.
type Engine struct {
	agg Aggregator
}

// NewEngine builds a pricing engine over the given aggregator.
func NewEngine(agg Aggregator) *Engine { return &Engine{agg: agg} }

// Build aggregates the tenant's usage over the whole month (business
// calendar, Asia/Shanghai) and prices it: subscription (amortized) +
// device ladder on the month's daily-peak maximum + token fees. The
// returned statement carries usage, fees and the ladder breakdown but no
// ID or status — those are assigned by the Service when the statement is
// persisted.
func (e *Engine) Build(ctx context.Context, tenantID string, year, month int) (*Statement, error) {
	if e == nil || e.agg == nil {
		return nil, errors.New("billing: engine has no aggregator")
	}
	if !ValidPeriod(year, month) {
		return nil, ErrInvalidPeriod
	}
	from, to := MonthRange(year, month, Shanghai())
	sum, err := e.agg.Aggregate(ctx, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	if sum == nil {
		sum = &UsageSummary{}
	}
	bd := deviceLadder(sum.DevicePeak)
	st := &Statement{
		TenantID:           tenantID,
		Year:               year,
		Month:              month,
		DevicePeak:         sum.DevicePeak,
		ToolCalls:          sum.ToolCalls,
		Tokens:             sum.Tokens,
		SubscriptionFeeFen: SubscriptionMonthlyFen(),
		DeviceFeeFen:       bd.MonthlyFeeFen,
		TokenFeeFen:        tokenFeeFen(sum.Tokens),
		Breakdown:          bd,
	}
	st.TotalFeeFen = st.SubscriptionFeeFen + st.DeviceFeeFen + st.TokenFeeFen
	return st, nil
}
