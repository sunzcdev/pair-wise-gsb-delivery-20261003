package renewal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Plan is the persisted schedule: one entry per managed certificate.
type Plan struct {
	Entries []*Entry `json:"entries"`
}

// Sorted returns the entries ordered by scheduled time, then CA, then cert ID.
// It does not modify the underlying slice order.
func (p Plan) Sorted() []*Entry {
	out := append([]*Entry(nil), p.Entries...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.ScheduledAt.Equal(b.ScheduledAt) {
			return a.ScheduledAt.Before(b.ScheduledAt)
		}
		if a.CA != b.CA {
			return a.CA < b.CA
		}
		return a.CertID < b.CertID
	})
	return out
}

// Store persists a [Plan]. Implementations must be safe for the planner's use;
// the planner serializes its own mutations.
type Store interface {
	Load() (*Plan, error)
	Save(*Plan) error
}

// ErrPlanNotFound is returned by [FileStore.Load] when no plan exists yet.
var ErrPlanNotFound = errors.New("renewal: saved plan not found")

// FileStore keeps the plan as a single JSON file written atomically.
type FileStore struct {
	Path string
}

// Load reads the plan file. It returns [ErrPlanNotFound] if it does not exist.
func (s FileStore) Load() (*Plan, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("renewal: decoding plan: %w", err)
	}
	return &p, nil
}

// Save writes the plan atomically by staging a temp file in the same
// directory and renaming it into place.
func (s FileStore) Save(p *Plan) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "\t")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".plan-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.Path)
}

// MemoryStore keeps the plan in memory; it is useful for tests and the
// simulator. A nil plan reports [ErrPlanNotFound].
type MemoryStore struct {
	plan *Plan
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Load implements [Store].
func (s *MemoryStore) Load() (*Plan, error) {
	if s.plan == nil {
		return nil, ErrPlanNotFound
	}
	return s.plan, nil
}

// Save implements [Store].
func (s *MemoryStore) Save(p *Plan) error {
	s.plan = p
	return nil
}
