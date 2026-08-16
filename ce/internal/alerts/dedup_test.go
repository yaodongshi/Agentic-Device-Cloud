package alerts

import (
	"context"
	"testing"
	"time"
)

func TestMemoryDeduperClaimWindow(t *testing.T) {
	ctx := context.Background()
	d := NewMemoryDeduper()
	now := fixedClock
	d.now = func() time.Time { return now }

	ok, err := d.Claim(ctx, DedupKey("r1"), DefaultDedupWindow)
	if err != nil || !ok {
		t.Fatalf("first claim = %v, %v; want true, nil", ok, err)
	}
	// Same key within the window: suppressed (FR-017 aggregation).
	ok, err = d.Claim(ctx, DedupKey("r1"), DefaultDedupWindow)
	if err != nil || ok {
		t.Fatalf("second claim = %v, %v; want false, nil", ok, err)
	}
	// Different rule key is independent.
	ok, err = d.Claim(ctx, DedupKey("r2"), DefaultDedupWindow)
	if err != nil || !ok {
		t.Fatalf("other key claim = %v, %v; want true, nil", ok, err)
	}
	// Window expiry allows a re-claim.
	now = now.Add(DefaultDedupWindow + time.Second)
	ok, err = d.Claim(ctx, DedupKey("r1"), DefaultDedupWindow)
	if err != nil || !ok {
		t.Fatalf("post-window claim = %v, %v; want true, nil", ok, err)
	}
}

func TestMemoryDeduperClaimExactlyAtWindowBoundary(t *testing.T) {
	ctx := context.Background()
	d := NewMemoryDeduper()
	now := fixedClock
	d.now = func() time.Time { return now }
	if ok, _ := d.Claim(ctx, "k", DefaultDedupWindow); !ok {
		t.Fatal("first claim failed")
	}
	now = now.Add(DefaultDedupWindow - time.Nanosecond)
	if ok, _ := d.Claim(ctx, "k", DefaultDedupWindow); ok {
		t.Fatal("claim just before window expiry succeeded, want suppressed")
	}
	now = now.Add(2 * time.Nanosecond)
	if ok, _ := d.Claim(ctx, "k", DefaultDedupWindow); !ok {
		t.Fatal("claim after window expiry failed, want success")
	}
}

func TestDedupKeyShape(t *testing.T) {
	if got := DedupKey("abc"); got != dedupKeyPrefix+"abc" {
		t.Fatalf("DedupKey = %q, want %q", got, dedupKeyPrefix+"abc")
	}
}
