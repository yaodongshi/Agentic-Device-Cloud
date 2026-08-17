package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeviceLadder(t *testing.T) {
	cases := []struct {
		name    string
		count   int
		inc     int
		t400    int
		t300    int
		t250    int
		annual  int64
		monthly int64
	}{
		{"zero devices", 0, 0, 0, 0, 0, 0, 0},
		{"below inclusion", 5, 5, 0, 0, 0, 0, 0},
		{"exact inclusion boundary", 20, 20, 0, 0, 0, 0, 0},
		{"first paid device", 21, 20, 1, 0, 0, 40000, 3333},
		{"tier400 upper boundary", 100, 20, 80, 0, 0, 3200000, 266667},
		{"tier300 entry", 101, 20, 80, 1, 0, 3230000, 269167},
		{"tier300 upper boundary", 500, 20, 80, 400, 0, 15200000, 1266667},
		{"tier250 entry", 501, 20, 80, 400, 1, 15225000, 1268750},
		{"far beyond tiers", 1000, 20, 80, 400, 500, 27700000, 2308333},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := deviceLadder(c.count)
			if b.IncludedDevices != c.inc || b.Tier400Devices != c.t400 ||
				b.Tier300Devices != c.t300 || b.Tier250Devices != c.t250 {
				t.Fatalf("tier split = %d/%d/%d/%d, want %d/%d/%d/%d",
					b.IncludedDevices, b.Tier400Devices, b.Tier300Devices, b.Tier250Devices,
					c.inc, c.t400, c.t300, c.t250)
			}
			if b.AnnualFeeFen != c.annual {
				t.Fatalf("annual fee = %d fen, want %d", b.AnnualFeeFen, c.annual)
			}
			if b.MonthlyFeeFen != c.monthly {
				t.Fatalf("monthly fee = %d fen, want %d", b.MonthlyFeeFen, c.monthly)
			}
		})
	}
}

func TestTokenFeeFen(t *testing.T) {
	cases := []struct {
		tokens int64
		want   int64
	}{
		{-1, 0},
		{0, 0},
		{49, 0}, // 0.49 fen rounds down
		{50, 1}, // 0.50 fen rounds half up
		{99, 1},
		{149, 1}, // 1.49 fen rounds down
		{150, 2}, // 1.50 fen rounds half up
		{549, 5},
		{550, 6}, // 5.50 fen rounds half up
		{999, 10},
		{1000, 10},       // 0.10 yuan exactly
		{1234, 12},       // 12.34 fen -> 12
		{8000000, 80000}, // 800 yuan
	}
	for _, c := range cases {
		if got := tokenFeeFen(c.tokens); got != c.want {
			t.Fatalf("tokenFeeFen(%d) = %d fen, want %d", c.tokens, got, c.want)
		}
	}
}

func TestAmortizeMonthly(t *testing.T) {
	cases := []struct {
		annual int64
		want   int64
	}{
		{0, 0},
		{1, 0}, // 1/12 fen rounds down
		{5, 0},
		{6, 1}, // 0.5 rounds half up
		{12, 1},
		{40000, 3333},     // 400 yuan/device/year -> 33.33 yuan/month
		{3200000, 266667}, // 80 devices at 400 yuan -> 2666.67 yuan/month
		{3980000, 331667}, // subscription 39800 yuan/year -> 3316.67 yuan/month
	}
	for _, c := range cases {
		if got := amortizeMonthly(c.annual); got != c.want {
			t.Fatalf("amortizeMonthly(%d) = %d, want %d", c.annual, got, c.want)
		}
	}
	if got := SubscriptionMonthlyFen(); got != 331667 {
		t.Fatalf("SubscriptionMonthlyFen = %d, want 331667", got)
	}
}

func TestMonthRange(t *testing.T) {
	loc := Shanghai()
	cases := []struct {
		year, month      int
		wantFrom, wantTo string
	}{
		{2026, 8, "2026-08-01 00:00:00 +08:00", "2026-09-01 00:00:00 +08:00"},
		{2028, 2, "2028-02-01 00:00:00 +08:00", "2028-03-01 00:00:00 +08:00"}, // leap year
		{2026, 12, "2026-12-01 00:00:00 +08:00", "2027-01-01 00:00:00 +08:00"},
	}
	for _, c := range cases {
		from, to := MonthRange(c.year, c.month, loc)
		if from.Format("2006-01-02 15:04:05 Z07:00") != c.wantFrom || to.Format("2006-01-02 15:04:05 Z07:00") != c.wantTo {
			t.Fatalf("MonthRange(%d,%d) = [%s, %s), want [%s, %s)",
				c.year, c.month, from.Format("2006-01-02 15:04:05 Z07:00"),
				to.Format("2006-01-02 15:04:05 Z07:00"), c.wantFrom, c.wantTo)
		}
	}
}

// fakeAggregator records the aggregation window and returns a fixed
// summary, letting the engine tests assert the window construction
// without a database.
type fakeAggregator struct {
	sum      *UsageSummary
	err      error
	tenantID string
	from     time.Time
	to       time.Time
}

func (f *fakeAggregator) Aggregate(ctx context.Context, tenantID string, from, to time.Time) (*UsageSummary, error) {
	f.tenantID = tenantID
	f.from = from
	f.to = to
	if f.err != nil {
		return nil, f.err
	}
	if f.sum == nil {
		return &UsageSummary{}, nil
	}
	cp := *f.sum
	return &cp, nil
}

func TestEngineBuild(t *testing.T) {
	loc := Shanghai()
	agg := &fakeAggregator{sum: &UsageSummary{DevicePeak: 100, ToolCalls: 5000, Tokens: 8000000}}
	e := NewEngine(agg)

	st, err := e.Build(context.Background(), "tenant-1", 2026, 8)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantFrom := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)
	wantTo := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	if agg.tenantID != "tenant-1" || !agg.from.Equal(wantFrom) || !agg.to.Equal(wantTo) {
		t.Fatalf("aggregation window = (%q, %s, %s), want (tenant-1, %s, %s)",
			agg.tenantID, agg.from, agg.to, wantFrom, wantTo)
	}
	if st.DevicePeak != 100 || st.ToolCalls != 5000 || st.Tokens != 8000000 {
		t.Fatalf("usage = %d/%d/%d, want 100/5000/8000000", st.DevicePeak, st.ToolCalls, st.Tokens)
	}
	// 100 devices: 20 included + 80 at 400 yuan/year = 3.2M fen/year ->
	// 2666.67 yuan/month; tokens 8M -> 800 yuan/month.
	if st.SubscriptionFeeFen != 331667 || st.DeviceFeeFen != 266667 || st.TokenFeeFen != 80000 {
		t.Fatalf("fees = %d/%d/%d, want 331667/266667/80000",
			st.SubscriptionFeeFen, st.DeviceFeeFen, st.TokenFeeFen)
	}
	if st.TotalFeeFen != 331667+266667+80000 {
		t.Fatalf("total = %d, want 678334", st.TotalFeeFen)
	}
	if st.Breakdown.Tier400Devices != 80 || st.Breakdown.IncludedDevices != 20 {
		t.Fatalf("breakdown = %+v, want 20 included / 80 tier-400", st.Breakdown)
	}
}

func TestEngineBuildErrors(t *testing.T) {
	if _, err := NewEngine(nil).Build(context.Background(), "t", 2026, 8); err == nil {
		t.Fatal("want error for engine without aggregator, got nil")
	}
	if _, err := NewEngine(&fakeAggregator{}).Build(context.Background(), "t", 1999, 1); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("want ErrInvalidPeriod, got %v", err)
	}
	if _, err := NewEngine(&fakeAggregator{}).Build(context.Background(), "t", 2026, 13); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("want ErrInvalidPeriod, got %v", err)
	}
	boom := errors.New("agg down")
	_, err := NewEngine(&fakeAggregator{err: boom}).Build(context.Background(), "t", 2026, 8)
	if !errors.Is(err, boom) {
		t.Fatalf("want aggregator error, got %v", err)
	}
}
