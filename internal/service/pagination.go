package service

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Page limits for every list endpoint.
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// Page is a request for one page of a list.
type Page struct {
	// Limit is the page size; 0 means DefaultPageSize.
	Limit int
	// Cursor is the opaque value returned as NextCursor by the previous page.
	Cursor string
}

// ListResult is one page of results.
type ListResult[T any] struct {
	Items []T
	// NextCursor is empty on the last page.
	NextCursor string
}

// maxCursorMicros is the start of year 10000 in Unix microseconds.
var maxCursorMicros = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro()

// keyset is the decoded form of a cursor: the sort key of the last row seen.
type keyset struct {
	at *time.Time
	id *uuid.UUID
}

// cursorPayload is what a cursor carries. Microseconds are exactly what
// Postgres stores for timestamptz, so a round trip loses nothing and the
// comparison "(created_at, id) < cursor" cannot skip or repeat a row.
type cursorPayload struct {
	Micros int64     `json:"t"`
	ID     uuid.UUID `json:"i"`
}

// EncodeCursor builds the opaque cursor that resumes after the row (t, id).
func EncodeCursor(t time.Time, id uuid.UUID) string {
	b, _ := json.Marshal(cursorPayload{Micros: t.UnixMicro(), ID: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodePage validates a Page and returns the SQL limit (one more than asked,
// to learn whether another page exists) and the keyset to resume from.
func decodePage(p Page) (limit int32, ks keyset, pageSize int, err error) {
	pageSize = p.Limit
	switch {
	case pageSize == 0:
		pageSize = DefaultPageSize
	case pageSize < 0 || pageSize > MaxPageSize:
		return 0, keyset{}, 0, invalid("limit", "must be between 1 and %d", MaxPageSize)
	}

	if p.Cursor != "" {
		raw, derr := base64.RawURLEncoding.DecodeString(p.Cursor)
		var c cursorPayload
		// Reject absurd timestamps up front: they would otherwise reach
		// Postgres and surface as an out-of-range error (a 500) instead of a 400.
		if derr != nil || json.Unmarshal(raw, &c) != nil || c.ID == uuid.Nil ||
			c.Micros < 0 || c.Micros > maxCursorMicros {
			return 0, keyset{}, 0, invalid("cursor", "is not valid")
		}
		at := time.UnixMicro(c.Micros)
		ks = keyset{at: &at, id: &c.ID}
	}
	return int32(pageSize + 1), ks, pageSize, nil
}

// finishPage trims the extra row fetched to detect a next page and computes
// the cursor for it.
func finishPage[T any](rows []T, pageSize int, key func(T) (time.Time, uuid.UUID)) ListResult[T] {
	if len(rows) <= pageSize {
		return ListResult[T]{Items: rows}
	}
	rows = rows[:pageSize]
	at, id := key(rows[pageSize-1])
	return ListResult[T]{Items: rows, NextCursor: EncodeCursor(at, id)}
}
