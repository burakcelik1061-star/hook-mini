// Package service holds Hook Mini's business rules. It sits between the HTTP
// layer (which only translates requests and responses) and the generated
// store (which only runs SQL), so each rule lives in exactly one place and can
// be tested without HTTP.
package service

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/internal/safedial"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// Service implements the use cases. It is safe for concurrent use.
type Service struct {
	pool   *pgxpool.Pool
	q      *store.Queries
	log    *slog.Logger
	egress safedial.Policy
}

// Option configures a Service.
type Option func(*Service)

// WithEgressPolicy sets which destinations endpoint URLs may point at. The
// default refuses private, loopback and link-local addresses.
func WithEgressPolicy(p safedial.Policy) Option {
	return func(s *Service) { s.egress = p }
}

// New returns a Service backed by pool.
func New(pool *pgxpool.Pool, log *slog.Logger, opts ...Option) *Service {
	s := &Service{pool: pool, q: store.New(pool), log: log}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Sentinel errors. The HTTP layer maps each one to a status code; anything
// else is an unexpected failure and becomes a 500.
var (
	// ErrNotFound means the resource does not exist for this tenant. It is
	// deliberately the same for "no such id" and "someone else's id", so
	// responses never reveal that another tenant's resource exists.
	ErrNotFound = errors.New("not found")

	// ErrUnauthorized means the API key is missing, malformed, unknown or revoked.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrIdempotencyMismatch means an idempotency key was reused with a
	// different request body.
	ErrIdempotencyMismatch = errors.New("idempotency key was already used with a different request")
)

// ValidationError describes input the caller can fix. Field names the JSON
// field (or header) at fault.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + " " + e.Message }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// pgViolation reports whether err is a Postgres error with the given SQLSTATE
// code and, when constraint is not empty, that exact constraint.
func pgViolation(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
)

// dataError turns a database rejection of the caller's data into the
// ValidationError it really is. Validators run first and catch the common
// cases with clear messages; this is the backstop for everything they cannot
// know, because the database's idea of valid text and JSON (NUL, lone
// surrogates, numeric range, locale whitespace) is the one that counts.
//
//   - SQLSTATE class 22 (data exception): the value cannot be stored.
//   - 23514 (check violation): a validator and a CHECK constraint disagree.
//     The caller still gets a 400, and the drift is logged so it is fixed.
//
// It returns nil for any other error, which stays a genuine server failure.
func (s *Service) dataError(err error, field string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	switch {
	case strings.HasPrefix(pgErr.Code, "22"):
		return invalid(field, "contains a value the database cannot store (for example a \\u0000 escape, an invalid Unicode sequence or a number that is too large)")
	case pgErr.Code == pgCheckViolation:
		s.log.Warn("the database rejected input that validation accepted: a validator and a CHECK constraint disagree",
			"constraint", pgErr.ConstraintName, "field", field)
		return invalid(field, "is not acceptable")
	}
	return nil
}
