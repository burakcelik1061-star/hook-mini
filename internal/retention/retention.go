// Package retention deletes data that has outlived its usefulness: finished
// deliveries (with their attempts) and the events nothing refers to any more.
//
// Deliveries that are pending or in flight are never touched, and an event
// stays as long as any delivery of it does. The work is done in small batches
// so no single statement holds locks for long, and any number of workers may
// run a Cleaner at once: the queries skip rows another one has locked.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// BatchSize is how many rows one DELETE removes at most.
const BatchSize = 1000

// Config tunes a Cleaner.
type Config struct {
	// Period is how long finished data is kept. Zero keeps it forever, which
	// turns the cleaner off.
	Period time.Duration
	// Interval is how often a cleaning pass runs.
	Interval time.Duration
	// BatchSize overrides the number of rows per DELETE; zero means BatchSize.
	BatchSize int
}

func (c Config) validate() error {
	switch {
	case c.Period < 0:
		return errors.New("retention period must not be negative")
	case c.Interval <= 0:
		return errors.New("cleanup interval must be positive")
	case c.BatchSize < 0:
		return errors.New("batch size must not be negative")
	}
	return nil
}

// Result counts what one pass deleted.
type Result struct {
	Deliveries int64
	Events     int64
}

// Cleaner deletes expired data.
type Cleaner struct {
	q   *store.Queries
	log *slog.Logger
	cfg Config
}

// New validates cfg and returns a Cleaner on db (a pool, or a transaction in
// tests).
func New(db store.DBTX, log *slog.Logger, cfg Config) (*Cleaner, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid retention config: %w", err)
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = BatchSize
	}
	return &Cleaner{q: store.New(db), log: log.With("component", "retention"), cfg: cfg}, nil
}

// Enabled reports whether the cleaner deletes anything.
func (c *Cleaner) Enabled() bool { return c.cfg.Period > 0 }

// Once runs one pass: deliveries first, then the events they leave without
// any delivery. It keeps going batch by batch until nothing expired is left,
// or ctx ends. With retention off it does nothing.
func (c *Cleaner) Once(ctx context.Context) (Result, error) {
	var res Result
	if !c.Enabled() {
		return res, nil
	}
	retentionMs := c.cfg.Period.Milliseconds()
	batch := int32(c.cfg.BatchSize)

	for ctx.Err() == nil {
		n, err := c.q.DeleteExpiredDeliveries(ctx, store.DeleteExpiredDeliveriesParams{RetentionMs: retentionMs, BatchSize: batch})
		res.Deliveries += n
		if err != nil {
			return res, fmt.Errorf("delete expired deliveries: %w", err)
		}
		if n < int64(batch) {
			break
		}
	}
	for ctx.Err() == nil {
		n, err := c.q.DeleteExpiredEvents(ctx, store.DeleteExpiredEventsParams{RetentionMs: retentionMs, BatchSize: batch})
		res.Events += n
		if err != nil {
			return res, fmt.Errorf("delete expired events: %w", err)
		}
		if n < int64(batch) {
			break
		}
	}
	return res, ctx.Err()
}

// Run cleans on start and then every Interval until ctx is cancelled. A
// failed pass is logged and retried at the next tick.
func (c *Cleaner) Run(ctx context.Context) {
	if !c.Enabled() {
		c.log.Info("retention is off: finished deliveries and events are kept forever")
		return
	}
	c.log.Info("retention enabled", "period", c.cfg.Period, "interval", c.cfg.Interval)

	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		res, err := c.Once(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			c.log.Error("retention pass failed", "error", err, "deleted_deliveries", res.Deliveries, "deleted_events", res.Events)
		case res.Deliveries > 0 || res.Events > 0:
			c.log.Info("retention pass finished", "deleted_deliveries", res.Deliveries, "deleted_events", res.Events)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
