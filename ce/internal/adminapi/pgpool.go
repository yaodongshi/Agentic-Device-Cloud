package adminapi

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// pgxPooler is the minimal pgx pool surface the PostgreSQL repositories
// use. *pgxpool.Pool satisfies it in production; unit tests inject
// pgxmock-backed fakes. It carries no production behavior.
type pgxPooler interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
