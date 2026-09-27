// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// DefaultIncarnationReserveBatch is the number of incarnation numbers that
// are reserved (and persisted) at a time when IncarnationReserveBatch is
// not explicitly configured. Reserving in batches amortizes disk writes at
// the cost of skipping up to this many incarnation numbers on each restart.
const DefaultIncarnationReserveBatch = 100

// ErrIncarnationExhausted is returned when the local incarnation number is
// about to overflow the uint32 range and no further incarnation can be
// reserved or allocated.
var ErrIncarnationExhausted = errors.New("incarnation space exhausted")

// IncarnationStore persists the local node's incarnation number so that a
// restarted process never publishes an incarnation lower than one it (or a
// previous process with the same node name) has already used. Without a
// store, a fast restart of a same-named node can be shadowed by stale
// alive, suspect or dead messages from its previous incarnation that are
// still circulating in the network.
//
// Implementations must be safe for concurrent use and must make Store
// durable before returning: after Store(inc) succeeds, any future Load
// (including from a later process) must return a value >= inc.
type IncarnationStore interface {
	// Load returns the highest incarnation ever reserved by this node,
	// or 0 if none has been reserved yet.
	Load() (uint32, error)

	// Store durably records that all incarnations up to and including
	// inc have been reserved for this node. inc is monotonically
	// increasing across calls.
	Store(inc uint32) error
}

// fileIncarnationRecord is the on-disk format of a FileIncarnationStore.
type fileIncarnationRecord struct {
	Node        string `json:"node"`
	Incarnation uint32 `json:"incarnation"`
}

// FileIncarnationStore is an IncarnationStore backed by a small file on
// local disk. The file records the node name it belongs to and refuses to
// load when opened for a different node, which prevents the whole cluster
// from accidentally sharing a single counter file (for example on a shared
// filesystem). Every node must use its own file on its own local disk.
type FileIncarnationStore struct {
	path string
	node string

	mu sync.Mutex
}

// NewFileIncarnationStore returns an IncarnationStore that keeps the
// reserved incarnation for the named node in the file at path. The node
// name must match Config.Name. The path must be unique per node; it must
// not be shared with other nodes in the cluster.
func NewFileIncarnationStore(path, nodeName string) (*FileIncarnationStore, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("node name is required for an incarnation store")
	}
	if path == "" {
		return nil, fmt.Errorf("path is required for an incarnation store")
	}
	return &FileIncarnationStore{path: path, node: nodeName}, nil
}

// Load implements IncarnationStore.
func (s *FileIncarnationStore) Load() (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	var rec fileIncarnationRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return 0, fmt.Errorf("failed to parse incarnation file %s: %w", s.path, err)
	}
	if rec.Node != s.node {
		return 0, fmt.Errorf("incarnation file %s belongs to node %q, not %q: "+
			"each node must use its own incarnation file", s.path, rec.Node, s.node)
	}
	return rec.Incarnation, nil
}

// Store implements IncarnationStore.
func (s *FileIncarnationStore) Store(inc uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := json.Marshal(fileIncarnationRecord{Node: s.node, Incarnation: inc})
	if err != nil {
		return err
	}

	// Write to a temporary file in the same directory and rename it into
	// place so a crash mid-write cannot leave a corrupt counter behind.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".incarnation-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	return nil
}

// initIncarnation restores the local incarnation from the configured
// IncarnationStore (if any) and reserves the first batch of incarnations.
// It must be called before the node starts announcing itself.
func (m *Memberlist) initIncarnation() error {
	if m.config.IncarnationStore == nil {
		return nil
	}

	persisted, err := m.config.IncarnationStore.Load()
	if err != nil {
		return fmt.Errorf("failed to load persisted incarnation: %w", err)
	}

	m.incarnationLock.Lock()
	defer m.incarnationLock.Unlock()
	m.incarnation.Store(persisted)
	m.incarnationReserved = persisted
	return m.reserveIncarnationsLocked(persisted)
}

// reserveIncarnationsLocked persists a new reservation covering at least
// minimum. m.incarnationLock must be held.
func (m *Memberlist) reserveIncarnationsLocked(minimum uint32) error {
	batch := m.config.IncarnationReserveBatch
	if batch == 0 {
		batch = DefaultIncarnationReserveBatch
	}

	current := m.incarnation.Load()
	target := uint64(current) + uint64(batch)
	if target < uint64(minimum) {
		target = uint64(minimum)
	}
	if target > math.MaxUint32 {
		return fmt.Errorf("cannot reserve incarnation %d beyond the uint32 range: %w",
			target, ErrIncarnationExhausted)
	}

	if err := m.config.IncarnationStore.Store(uint32(target)); err != nil {
		return fmt.Errorf("failed to persist incarnation reservation: %w", err)
	}
	m.incarnationReserved = uint32(target)
	return nil
}
