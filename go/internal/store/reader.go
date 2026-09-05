package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Reader exposes only historian queries. Opening it never initializes a writer,
// migrates schema, prunes replay keys, or changes retention/compression policies.
type Reader struct{ pool *pgxpool.Pool }

func OpenReader(ctx context.Context, dsn string) (*Reader, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("reader configuration: %w", err)
	}
	// Defense in depth, including when the caller supplied writer credentials.
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(cctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("reader connection: %w", err)
	}
	// The probe is the replay query's own SELECT list plus its ORDER BY keys,
	// so a schema missing any column the query touches fails here, with the
	// actionable message, rather than mid-replay.
	rows, err := pool.Query(cctx, navFrameSelect+" ORDER BY receipt_order NULLS FIRST, received_at, ts LIMIT 0")
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("historian replay schema unavailable: require SELECT on nav_frames and its replay columns; ask the collector administrator to apply compatible writer migrations: %w", err)
	}
	rows.Close()
	return &Reader{pool: pool}, nil
}

func (r *Reader) Close() { r.pool.Close() }

func (r *Reader) QueryNavFrames(ctx context.Context, q NavFrameQuery, fn func(StoredNavFrame) error) error {
	return queryNavFrames(ctx, r.pool, q, fn)
}
