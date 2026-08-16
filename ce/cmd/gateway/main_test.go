package main

import (
	"context"
	"testing"
	"time"

	"adc.dev/ce/pkg/ratelimit"
)

func TestGetenv(t *testing.T) {
	t.Setenv("ADC_TEST_KEY", "hello")
	if v := getenv("ADC_TEST_KEY", "def"); v != "hello" {
		t.Fatalf("getenv must read the env var, got %q", v)
	}
	if v := getenv("ADC_TEST_MISSING", "def"); v != "def" {
		t.Fatalf("getenv must fall back, got %q", v)
	}
	if v := getenv("ADC_TEST_EMPTY", "def"); v != "def" {
		t.Fatalf("empty env must fall back, got %q", v)
	}
	t.Setenv("ADC_TEST_EMPTY", "")
	if v := getenv("ADC_TEST_EMPTY", "def"); v != "def" {
		t.Fatalf("empty env must fall back, got %q", v)
	}
}

func TestGetenvBool(t *testing.T) {
	t.Setenv("ADC_TEST_BOOL", "true")
	if !getenvBool("ADC_TEST_BOOL", false) {
		t.Fatal("true must parse")
	}
	t.Setenv("ADC_TEST_BOOL", "false")
	if getenvBool("ADC_TEST_BOOL", true) {
		t.Fatal("false must parse")
	}
	t.Setenv("ADC_TEST_BOOL", "garbage")
	if !getenvBool("ADC_TEST_BOOL", true) {
		t.Fatal("invalid value must fall back to def")
	}
	t.Setenv("ADC_TEST_BOOL", "")
	if !getenvBool("ADC_TEST_BOOL", true) {
		t.Fatal("empty value must fall back to def")
	}
	t.Setenv("ADC_TEST_BOOL", "1")
	if !getenvBool("ADC_TEST_BOOL", false) {
		t.Fatal("ParseBool forms like 1 must work")
	}
}

func TestGatewayRateLimiterAllow(t *testing.T) {
	store := ratelimit.NewMemStore()
	lim, err := ratelimit.NewTokenBucket(store, nil)
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	g := gatewayRateLimiter{lim: lim}
	ctx := context.Background()

	ok, err := g.Allow(ctx, "tenant", "t1")
	if err != nil || !ok {
		t.Fatalf("tenant scope must allow: %v %v", ok, err)
	}
	ok, err = g.Allow(ctx, "agent", "a1")
	if err != nil || !ok {
		t.Fatalf("agent scope must allow: %v %v", ok, err)
	}
	ok, err = g.Allow(ctx, "device", "d1")
	if err != nil || !ok {
		t.Fatalf("device scope must allow: %v %v", ok, err)
	}
	// unknown scopes pass (defense-in-depth, the gate is not the identity check)
	ok, err = g.Allow(ctx, "unknown", "x")
	if err != nil || !ok {
		t.Fatalf("unknown scope must pass: %v %v", ok, err)
	}
}

func TestGatewayRateLimiterExhaustion(t *testing.T) {
	// burst 1, tiny rate: the second call in the same window is refused
	store := ratelimit.NewMemStore()
	lim, err := ratelimit.NewTokenBucket(store, map[ratelimit.Scope]ratelimit.Limit{
		ratelimit.ScopeTenant: {Rate: 0.01, Burst: 1},
		ratelimit.ScopeAgent:  {Rate: 0.01, Burst: 1},
		ratelimit.ScopeDevice: {Rate: 0.01, Burst: 1},
	})
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	g := gatewayRateLimiter{lim: lim}
	ctx := context.Background()
	if ok, err := g.Allow(ctx, "tenant", "burst"); err != nil || !ok {
		t.Fatalf("first call must pass: %v %v", ok, err)
	}
	if ok, _ := g.Allow(ctx, "tenant", "burst"); ok {
		t.Fatal("second call must be refused with burst 1")
	}
	time.Sleep(10 * time.Millisecond)
}
