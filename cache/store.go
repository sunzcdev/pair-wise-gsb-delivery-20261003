package cache

import (
	"net/http"
	"time"
)

// Entry is one cached URL, holding every Vary variant selected for it.
// A variant owns its metadata; the response body lives in the Store and
// is referenced by BodyID so identical bodies can be shared and a 304 can
// refresh metadata without touching the body.
type Entry struct {
	PrimaryKey string
	Variants   []*Variant
}

// Variant is a single negotiated representation of an Entry.
type Variant struct {
	// PrimaryKey is the cache entry this variant belongs to.
	PrimaryKey string
	// VaryNames are the request header names the response varies on.
	VaryNames []string
	// VaryKey is the concrete selection (request header values) for this
	// representation.
	VaryKey string

	// BodyID identifies the stored response body.
	BodyID string

	// Header is the stored response header block. It is owned by the
	// variant and must not be mutated in place.
	Header     http.Header
	StatusCode int

	// Freshness bookkeeping.
	Date         time.Time // origin Date (or responseTime fallback)
	RequestTime  time.Time // request left the cache
	ResponseTime time.Time // response arrived at the cache
	HeaderAge    time.Duration
	FreshnessTTL time.Duration
	MustValidate bool
	// ExpiresAt is the absolute time the representation becomes stale.
	ExpiresAt time.Time
}

// Clone returns a deep copy suitable for handing to a caller.
func (v *Variant) Clone() *Variant {
	cp := *v
	cp.VaryNames = append([]string(nil), v.VaryNames...)
	cp.Header = v.Header.Clone()
	return &cp
}

// age returns the current age of the representation per RFC 9111 §4.2.3.
func (v *Variant) age(now time.Time) time.Duration {
	return currentAge(now, v.Date, v.RequestTime, v.ResponseTime, v.HeaderAge)
}

// fresh reports whether the representation is fresh at now.
func (v *Variant) fresh(now time.Time) bool {
	return now.Before(v.ExpiresAt)
}

// Store is the replaceable persistence layer for the cache. Implementations
// are expected to be safe for concurrent use and to enforce their own
// capacity bounds, evicting older entries as needed.
//
// Variants are stored whole inside an Entry; bodies are stored separately
// and referenced by ID, allowing bodies to be shared across variants and
// metadata to be refreshed without rewriting the body.
type Store interface {
	// GetEntry returns the entry for a primary key and whether it exists.
	GetEntry(key string) (*Entry, bool)
	// PutEntry stores (or replaces) an entry, refreshing its recency.
	PutEntry(entry *Entry)
	// DeleteEntry removes an entry and all of its variants.
	DeleteEntry(key string)

	// PutBody stores a body under id, replacing any previous value.
	PutBody(id string, body []byte)
	// GetBody returns a stored body. The returned slice must not be
	// mutated by callers.
	GetBody(id string) ([]byte, bool)
	// DeleteBody releases a stored body.
	DeleteBody(id string)
}
