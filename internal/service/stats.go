package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// StatusCounts counts deliveries by status.
type StatusCounts struct {
	Succeeded, Dead, Pending, InFlight int64
}

// EndpointStats is one endpoint's share of the statistics.
type EndpointStats struct {
	ID                  uuid.UUID
	URL                 string
	Status              string
	ConsecutiveFailures int32
	Last24h             StatusCounts
}

// Stats is what the console's overview shows for a tenant.
type Stats struct {
	// Last24h counts the deliveries created in the last 24 hours by the
	// status they have now.
	Last24h StatusCounts
	// Backlog counts unfinished deliveries, however old.
	Backlog struct{ Pending, InFlight int64 }
	// Endpoints lists every endpoint with its 24-hour counts.
	Endpoints []EndpointStats
}

// Stats gathers the tenant's statistics. The three queries run in one
// read-only, repeatable-read transaction, so the figures describe the same
// moment and add up.
func (s *Service) Stats(ctx context.Context, tenantID uuid.UUID) (Stats, error) {
	var out Stats
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		counts, err := q.DeliveryCountsLast24h(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("delivery counts: %w", err)
		}
		for _, c := range counts {
			add(&out.Last24h, c.Status, c.Deliveries)
		}
		backlog, err := q.DeliveryBacklog(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("backlog: %w", err)
		}
		out.Backlog.Pending, out.Backlog.InFlight = backlog.Pending, backlog.InFlight
		eps, err := q.EndpointStatsLast24h(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("endpoint stats: %w", err)
		}
		out.Endpoints = make([]EndpointStats, 0, len(eps))
		for _, e := range eps {
			out.Endpoints = append(out.Endpoints, endpointStats(e))
		}
		return nil
	})
	return out, err
}

func endpointStats(e store.EndpointStatsLast24hRow) EndpointStats {
	return EndpointStats{
		ID: e.ID, URL: e.Url, Status: e.Status, ConsecutiveFailures: e.ConsecutiveFailures,
		Last24h: StatusCounts{Succeeded: e.Succeeded, Dead: e.Dead, Pending: e.Pending, InFlight: e.InFlight},
	}
}

func add(c *StatusCounts, status string, n int64) {
	switch status {
	case "succeeded":
		c.Succeeded += n
	case "dead":
		c.Dead += n
	case "pending":
		c.Pending += n
	case "in_flight":
		c.InFlight += n
	}
}
