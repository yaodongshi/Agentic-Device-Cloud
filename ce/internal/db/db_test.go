package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"adc.dev/ce/internal/config"
)

// TestConnectErrors covers the failure paths of the pool bootstrap: an
// unparseable DSN (invalid sslmode) and an unreachable endpoint. Both must
// return wrapped errors without leaking a half-open pool.
func TestConnectErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// An sslmode value outside the pgx set fails at parse time.
	_, err := Connect(ctx, config.DSN{Host: "localhost", Port: 5432, User: "u", Password: "p", DBName: "adc", SSLMode: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "db: parse dsn") {
		t.Fatalf("want parse error, got %v", err)
	}

	// Unreachable endpoint: connection refused -> ping error, pool closed.
	if _, err := Connect(ctx, config.DSN{Host: "127.0.0.1", Port: 1, User: "u", Password: "p", DBName: "adc", SSLMode: "disable"}); err == nil {
		t.Fatal("want connect error")
	} else if !strings.Contains(err.Error(), "db:") {
		t.Fatalf("want wrapped db error, got %v", err)
	}
}
