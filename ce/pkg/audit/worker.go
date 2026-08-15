package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Inserter persists a batch of events into adc_audit_logs. The pgx
// implementation relies on the request_id unique index (ON CONFLICT DO
// NOTHING) for idempotency, so redelivered events are harmless
// (design/32 6.3).
type Inserter interface {
	InsertBatch(ctx context.Context, evs []*AuditEvent) error
}

const (
	defaultBatchSize   = 100
	defaultMaxRetries  = 3
	defaultPollTimeout = time.Second
)

// Worker drains QueueKey into adc_audit_logs (design/31 4.2 consumer):
// BRPOPLPUSH moves an event to ProcessingKey as a crash guard, a batch of
// events is inserted into PG, and on success each payload is LREM'd from
// the processing list. Transient insert failures are retried maxRetries
// times with backoff; after that the payloads are pushed back to the
// source queue (requeue) and picked up again later.
type Worker struct {
	q           Queue
	ins         Inserter
	srcKey      string
	procKey     string
	pollTimeout time.Duration
	batchSize   int
	maxRetries  int
	backoff     func(attempt int) time.Duration
	logger      *slog.Logger
}

// WorkerConfig configures a Worker; zero values select the defaults.
type WorkerConfig struct {
	Queue       Queue
	Inserter    Inserter
	BatchSize   int
	MaxRetries  int
	PollTimeout time.Duration
	Backoff     func(attempt int) time.Duration
	Logger      *slog.Logger
}

// NewWorker builds a Worker over an arbitrary Queue, which is what lets
// tests run the full consume path against an in-memory fake.
func NewWorker(cfg WorkerConfig) *Worker {
	w := &Worker{
		q:           cfg.Queue,
		ins:         cfg.Inserter,
		srcKey:      QueueKey,
		procKey:     ProcessingKey,
		pollTimeout: cfg.PollTimeout,
		batchSize:   cfg.BatchSize,
		maxRetries:  cfg.MaxRetries,
		backoff:     cfg.Backoff,
		logger:      cfg.Logger,
	}
	if w.pollTimeout <= 0 {
		w.pollTimeout = defaultPollTimeout
	}
	if w.batchSize <= 0 {
		w.batchSize = defaultBatchSize
	}
	if w.maxRetries <= 0 {
		w.maxRetries = defaultMaxRetries
	}
	if w.backoff == nil {
		w.backoff = defaultBackoff
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	return w
}

// NewValkeyWorker builds the production worker backed by go-redis/v9 and
// pgx/v5.
func NewValkeyWorker(rdb *redis.Client, pool *pgxpool.Pool) *Worker {
	return NewWorker(WorkerConfig{
		Queue:    redisQueue{rdb: rdb},
		Inserter: &pgInserter{pool: pool},
	})
}

// queuedEvent pairs a raw queue payload with its decoded event; the raw
// payload is needed for the LREM acknowledgement.
type queuedEvent struct {
	payload []byte
	ev      *AuditEvent
}

// Run drains the queue until ctx is cancelled. It returns nil on
// cancellation (graceful shutdown, design/31 1.3.7); a non-nil error
// means the loop cannot continue. Run itself is single-goroutine; start
// multiple workers to scale out.
func (w *Worker) Run(ctx context.Context) error {
	w.recoverProcessing(ctx)
	for {
		if ctx.Err() != nil {
			return nil
		}
		items, err := w.nextBatch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Warn("audit: dequeue failed, will retry", "err", err)
			sleepCtx(ctx, time.Second) // avoid a hot loop while Valkey is down
			continue
		}
		if len(items) == 0 {
			continue
		}
		evs := make([]*AuditEvent, len(items))
		for i, it := range items {
			evs[i] = it.ev
		}
		if err := w.insertWithRetry(ctx, evs); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Error("audit: insert failed after retries, requeueing",
				"count", len(items), "err", err)
			w.requeue(ctx, items)
			continue
		}
		for _, it := range items {
			w.logger.Debug("audit: event persisted",
				"request_id", it.ev.EventID, "trace_id", it.ev.TraceID)
			w.ack(ctx, it.payload)
		}
	}
}

// nextBatch BRPOPLPUSHes one event (blocking up to pollTimeout) and then
// drains up to batchSize-1 more without blocking, so inserts stay batched
// while an idle queue keeps latency low. Malformed or invalid payloads
// are dropped from the processing list so poison messages cannot wedge
// the queue.
func (w *Worker) nextBatch(ctx context.Context) ([]queuedEvent, error) {
	p, err := w.q.BRPopLPush(ctx, w.srcKey, w.procKey, w.pollTimeout)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	payloads := [][]byte{p}
	for len(payloads) < w.batchSize {
		p, err := w.q.BRPopLPush(ctx, w.srcKey, w.procKey, 0)
		if err != nil {
			w.logger.Warn("audit: batch drain failed", "err", err)
			break
		}
		if p == nil {
			break
		}
		payloads = append(payloads, p)
	}
	items := make([]queuedEvent, 0, len(payloads))
	for _, p := range payloads {
		var ev AuditEvent
		if err := json.Unmarshal(p, &ev); err != nil {
			w.logger.Error("audit: dropping malformed event", "err", err)
			w.ack(ctx, p)
			continue
		}
		if err := ev.Validate(); err != nil {
			w.logger.Error("audit: dropping invalid event", "err", err)
			w.ack(ctx, p)
			continue
		}
		items = append(items, queuedEvent{payload: p, ev: &ev})
	}
	return items, nil
}

// ack removes one payload from the processing list; the commit point
// after the PG insert succeeds.
func (w *Worker) ack(ctx context.Context, payload []byte) {
	if err := w.q.LRem(ctx, w.procKey, 1, payload); err != nil {
		w.logger.Error("audit: lrem processing failed", "err", err)
	}
}

// requeue pushes failed payloads back to the source queue and then
// removes them from the processing list. RPUSH precedes LREM: a crash
// between the two leaves a duplicate, which the request_id unique index
// makes harmless (at-least-once, design/32 6.3).
func (w *Worker) requeue(ctx context.Context, items []queuedEvent) {
	for _, it := range items {
		if err := w.q.RPush(ctx, w.srcKey, it.payload); err != nil {
			w.logger.Error("audit: requeue rpush failed", "err", err)
		}
		w.ack(ctx, it.payload)
	}
}

// recoverProcessing returns events stranded in the processing list (a
// crash between BRPOPLPUSH and LRem) to the source queue so they are not
// lost (SEC-07 at-least-once). Order is preserved: each tail pop becomes
// a head push, so the sequence is reversed twice.
func (w *Worker) recoverProcessing(ctx context.Context) {
	for {
		p, err := w.q.BRPopLPush(ctx, w.procKey, w.srcKey, 0)
		if err != nil {
			w.logger.Warn("audit: processing recovery failed", "err", err)
			return
		}
		if p == nil {
			return
		}
		w.logger.Info("audit: requeued stranded event")
	}
}

// insertWithRetry attempts the batch insert up to maxRetries+1 times.
// Only retryable errors (design/31 1.3.4 taxonomy) enter the backoff
// loop; permanent failures skip straight to requeue.
func (w *Worker) insertWithRetry(ctx context.Context, evs []*AuditEvent) error {
	err := w.ins.InsertBatch(ctx, evs)
	for retry := 0; err != nil && isRetryable(err) && retry < w.maxRetries; retry++ {
		w.logger.Warn("audit: batch insert failed, retrying",
			"retry", retry+1, "max_retries", w.maxRetries, "err", err)
		if !sleepCtx(ctx, w.backoff(retry+1)) {
			return ctx.Err()
		}
		err = w.ins.InsertBatch(ctx, evs)
	}
	return err
}

// retryable marks transient errors that deserve the backoff retry loop
// (network blips, Valkey flapping, PG deadlocks).
type retryable interface {
	Retryable() bool
}

func isRetryable(err error) bool {
	var r retryable
	return errors.As(err, &r) && r.Retryable()
}

// transientError wraps errors worth retrying (design/31 1.3.4).
type transientError struct{ err error }

func (e *transientError) Error() string   { return e.err.Error() }
func (e *transientError) Unwrap() error   { return e.err }
func (e *transientError) Retryable() bool { return true }

// sleepCtx waits d or until ctx is done; false means ctx fired.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// defaultBackoff mirrors the notification retry cadence (design/31
// 3.3.5): 1s, 2s, 4s.
func defaultBackoff(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// insertSQL maps AuditEvent fields onto adc_audit_logs columns
// (migration 0001_init.up.sql section 3.10). The ON CONFLICT clause
// targets the uq_audit_request_id unique index so redelivered events are
// skipped (design/32 6.3). Column-list form is used because the migration
// defines a unique index (not a constraint); both columns must be listed
// since the index is composite (request_id, created_at).
const insertSQL = `
INSERT INTO adc_audit_logs
    (id, tenant_id, event_type, actor_type, actor_id, device_id, tool_name,
     risk_level, request_params, response_payload, status, hitl_approver,
     request_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (request_id, created_at) DO NOTHING`

// pgInserter batch-inserts events into adc_audit_logs through a pgx pool.
type pgInserter struct {
	pool *pgxpool.Pool
}

func (p *pgInserter) InsertBatch(ctx context.Context, evs []*AuditEvent) error {
	if p.pool == nil {
		return errors.New("audit: nil pgx pool")
	}
	b := &pgx.Batch{}
	for _, ev := range evs {
		paramsJSON, err := marshalJSONB(ev.Params)
		if err != nil {
			return fmt.Errorf("audit: marshal params: %w", err)
		}
		resultJSON, err := marshalJSONB(ev.Result)
		if err != nil {
			return fmt.Errorf("audit: marshal result: %w", err)
		}
		createdAt := ev.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}
		b.Queue(insertSQL,
			ev.EventID,
			ev.TenantID,
			EventTypeToolCall,
			ActorTypeAgent,
			nullIfEmpty(ev.AgentID),
			nullIfEmpty(ev.DeviceID),
			nullIfEmpty(ev.ToolName),
			ev.RiskLevel,
			paramsJSON,
			resultJSON,
			ev.Status,
			nullIfEmpty(ev.Approver),
			ev.EventID,
			createdAt,
		)
	}
	br := p.pool.SendBatch(ctx, b)
	defer br.Close()
	for range evs {
		if _, err := br.Exec(); err != nil {
			return classifyPgErr(fmt.Errorf("audit: insert adc_audit_logs: %w", err))
		}
	}
	return nil
}

// marshalJSONB returns the JSON bytes of v, or nil (SQL NULL) when v is
// nil.
func marshalJSONB(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// classifyPgErr marks SQLSTATE classes 22 (data exception, e.g. invalid
// uuid) and 23 (integrity constraint violation) as permanent; everything
// else (connection failures, deadlocks, timeouts) is transient and worth
// retrying (design/31 1.3.4).
func classifyPgErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 {
		switch pgErr.Code[:2] {
		case "22", "23":
			return err // permanent: skip the backoff retry loop
		}
	}
	return &transientError{err: err}
}

// nullIfEmpty maps empty strings to nil so optional uuid/varchar columns
// get SQL NULL instead of "" (which PG would reject for uuid).
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
