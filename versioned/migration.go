package versioned

import (
	"fmt"
)

// MigrateFunc converts one version's record into the next version's record.
//
// in is the record at version from; the function returns the record at version
// to. A MigrateFunc may return an error to signal that a particular value
// cannot be converted losslessly (for example a fractional number into an
// integer field).
//
// Conversions are explicit: the function receives and produces stable field
// ids only. Go field names are not consulted, so renaming fields never causes
// a migration to happen silently.
type MigrateFunc func(in Record) (Record, error)

// RegisterMigration registers a single explicit conversion step from one
// version of schema to another. Both versions must already be registered via
// Register. Steps may go forward or backward; Unmarshal composes a shortest
// chain of registered steps when the stored version differs from the target.
func (r *Registry) RegisterMigration(schema string, from, to uint32, fn MigrateFunc) error {
	if fn == nil {
		return fmt.Errorf("versioned: RegisterMigration(%s %d->%d): fn is nil", schema, from, to)
	}
	if from == to {
		return fmt.Errorf("versioned: RegisterMigration(%s): from and to must differ, got %d", schema, from)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.migrations == nil {
		r.migrations = make(map[string]map[uint32][]migration)
	}

	if _, ok := r.versions[schema]; !ok {
		return &SchemaNotFoundError{Schema: schema}
	}
	if r.versions[schema][from] == nil {
		return &VersionNotFoundError{Schema: schema, Version: from}
	}
	if r.versions[schema][to] == nil {
		return &VersionNotFoundError{Schema: schema, Version: to}
	}

	edges := r.migrations[schema]
	if edges == nil {
		edges = make(map[uint32][]migration)
		r.migrations[schema] = edges
	}
	for _, e := range edges[from] {
		if e.to == to {
			return fmt.Errorf("versioned: migration %s %d->%d already registered", schema, from, to)
		}
	}
	edges[from] = append(edges[from], migration{from: from, to: to, fn: fn})

	return nil
}

// MustRegisterMigration is like RegisterMigration but panics on error.
func (r *Registry) MustRegisterMigration(schema string, from, to uint32, fn MigrateFunc) {
	if err := r.RegisterMigration(schema, from, to, fn); err != nil {
		panic(err)
	}
}

// migrationPath returns the shortest chain of steps from->to using a
// breadth-first search over explicitly registered steps.
func (r *Registry) migrationPath(schema string, from, to uint32) ([]migration, error) {
	if from == to {
		return nil, nil
	}

	r.mu.RLock()
	edges := r.migrations[schema]
	r.mu.RUnlock()

	if len(edges) == 0 {
		return nil, &MigrationNotFoundError{Schema: schema, From: from, To: to}
	}

	type node struct {
		version uint32
		parent  *node
		step    migration
	}

	visited := map[uint32]bool{from: true}
	queue := []*node{{version: from}}
	var goal *node

	for len(queue) > 0 && goal == nil {
		cur := queue[0]
		queue = queue[1:]
		for _, edge := range edges[cur.version] {
			if visited[edge.to] {
				continue
			}
			visited[edge.to] = true
			next := &node{version: edge.to, parent: cur, step: edge}
			if edge.to == to {
				goal = next
				break
			}
			queue = append(queue, next)
		}
	}

	if goal == nil {
		return nil, &MigrationNotFoundError{Schema: schema, From: from, To: to}
	}

	// Walk goal back to the source and reverse.
	var path []migration
	for n := goal; n.parent != nil; n = n.parent {
		path = append(path, n.step)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}
