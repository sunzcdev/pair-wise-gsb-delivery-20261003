// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DefaultIncarnationReserveBatch is the number of incarnation numbers that
// are reserved with the IncarnationStore in a single write when
// Config.IncarnationReserveBatch is not set.
const DefaultIncarnationReserveBatch = 1000

// ErrIncarnationExhausted is returned when allocating a new incarnation
// number would overflow the uint32 incarnation space. It is only returned
// when an IncarnationStore is configured; the pure in-memory mode keeps its
// historical wrap-around behavior.
var ErrIncarnationExhausted = errors.New("memberlist: incarnation number space exhausted")

// IncarnationStore is an optional persistence provider for the local node's
// incarnation number. When a node restarts quickly with the same name, the
// network may still hold alive, suspect and dead messages published by the
// previous process. By durably reserving incarnation numbers before they are
// used, a restarted process guarantees that every incarnation it publishes is
// strictly greater than anything the previous process could have published,
// so stale packets can never override the new process's metadata or state.
//
// Implementations must be safe for concurrent use and must be scoped to a
// single node: an incarnation counter is per-node state and must never be
// shared by the whole cluster.
type IncarnationStore interface {
	// Load returns the highest incarnation number that has been durably
	// reserved by a previous process of this node, or 0 if nothing has
	// been reserved yet.
	Load() (uint32, error)

	// Reserve durably records that all incarnation numbers up to and
	// including high have been reserved for this node. It must only return
	// nil once the reservation is durable; any failure must be reported as
	// an error so the caller does not publish unpersisted incarnations.
	Reserve(high uint32) error
}

// FileIncarnationStore is an IncarnationStore backed by a small file on
// local disk. Each node gets its own file inside a shared directory, named
// after the node, so the counter is never shared with other nodes in the
// cluster. The directory should be on storage local to the node.
type FileIncarnationStore struct {
	path string

	mu   sync.Mutex
	high uint32 // highest incarnation durably reserved by this process
}

// NewFileIncarnationStore creates a FileIncarnationStore for the given node
// inside dir, creating dir if necessary. The node name must be non-empty and
// must not contain path separators, so that every node maps to exactly one
// file of its own.
func NewFileIncarnationStore(dir, nodeName string) (*FileIncarnationStore, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("memberlist: node name is required for an incarnation store")
	}
	if nodeName != filepath.Base(nodeName) {
		return nil, fmt.Errorf("memberlist: node name %q cannot be used as an incarnation file name", nodeName)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memberlist: failed to create incarnation store directory: %w", err)
	}
	s := &FileIncarnationStore{
		path: filepath.Join(dir, nodeName+".incarnation"),
	}
	high, err := s.Load()
	if err != nil {
		return nil, err
	}
	s.high = high
	return s, nil
}

// Load implements IncarnationStore.
func (s *FileIncarnationStore) Load() (uint32, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("memberlist: failed to read incarnation file %s: %w", s.path, err)
	}
	high, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("memberlist: failed to parse incarnation file %s: %w", s.path, err)
	}
	return uint32(high), nil
}

// Reserve implements IncarnationStore. Reservations are monotonic: reserving
// a value at or below an already durably reserved value is a no-op.
func (s *FileIncarnationStore) Reserve(high uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if high <= s.high {
		return nil
	}

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("memberlist: failed to write incarnation file %s: %w", s.path, err)
	}
	if _, err := f.WriteString(strconv.FormatUint(uint64(high), 10)); err != nil {
		_ = f.Close()
		return fmt.Errorf("memberlist: failed to write incarnation file %s: %w", s.path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("memberlist: failed to sync incarnation file %s: %w", s.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("memberlist: failed to write incarnation file %s: %w", s.path, err)
	}
	// Rename is not atomic on Windows when the destination exists, so remove
	// the old file first. The reservation is only committed to s.high after
	// the rename succeeds, so a crash in between simply loses the reservation.
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memberlist: failed to replace incarnation file %s: %w", s.path, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("memberlist: failed to replace incarnation file %s: %w", s.path, err)
	}
	s.high = high
	return nil
}
