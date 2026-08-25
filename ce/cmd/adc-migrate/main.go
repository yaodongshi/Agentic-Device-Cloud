// Command adc-migrate upgrades the ADC PostgreSQL schema and exits.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"adc.dev/ce/internal/config"
	"adc.dev/ce/migrations"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		cfg, err := config.LoadPostgres()
		if err != nil {
			fatal(err)
		}
		dsn = cfg.String()
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fatal(fmt.Errorf("adc-migrate: connect: %w", err))
	}
	defer conn.Close(context.Background())
	if err := migrations.Run(ctx, conn); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
