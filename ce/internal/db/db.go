// Package db 封装 PostgreSQL 连接池（pgx），提供迁移路径与统一查询入口。
package db

import (
	"context"
	"fmt"
	"time"

	"adc.dev/ce/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Pool struct {
	*pgxpool.Pool
}

// Connect 建立连接池；连接参数来自环境注入的 config.DSN。
func Connect(ctx context.Context, cfg config.DSN) (*Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.String())
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 15 * time.Minute

	p, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &Pool{Pool: p}, nil
}

// Close 关闭连接池。
func (p *Pool) Close() { p.Pool.Close() }
