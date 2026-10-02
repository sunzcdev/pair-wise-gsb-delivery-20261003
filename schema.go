// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import "fmt"

// DBSchema is the schema to use for the full database with a MemDB instance.
//
// MemDB will require a valid schema. Schema validation can be tested using
// the Validate function. Calling this function is recommended in unit tests.
type DBSchema struct {
	// Tables is the set of tables within this database. The key is the
	// table name and must match the Name in TableSchema.
	Tables map[string]*TableSchema
}

// Validate validates the schema.
func (s *DBSchema) Validate() error {
	if s == nil {
		return fmt.Errorf("schema is nil")
	}

	if len(s.Tables) == 0 {
		return fmt.Errorf("schema has no tables defined")
	}

	for name, table := range s.Tables {
		if name != table.Name {
			return fmt.Errorf("table name mis-match for '%s'", name)
		}

		if err := table.Validate(); err != nil {
			return fmt.Errorf("table %q: %s", name, err)
		}
	}

	return nil
}

// TableSchema is the schema for a single table.
type TableSchema struct {
	// Name of the table. This must match the key in the Tables map in DBSchema.
	Name string

	// ObjectCopy, if non-nil, is invoked to copy objects as they cross the
	// boundaries of the database: once on the way in (Insert) and once on
	// the way out (First, Last, Get, iterators, etc.).
	//
	// The copy on the way in means the database stores an independent
	// object, so callers may freely modify or reuse the object they passed
	// to Insert without corrupting the database. The copy on the way out
	// means callers own the object they receive and may modify it in-place
	// without affecting the stored object or its index entries.
	//
	// If ObjectCopy is nil, objects are stored and returned as-is and the
	// caller must not modify objects that have been inserted.
	ObjectCopy ObjectCopyFunc

	// Indexes is the set of indexes for querying this table. The key
	// is a unique name for the index and must match the Name in the
	// IndexSchema.
	Indexes map[string]*IndexSchema
}

// ObjectCopyFunc is a function that returns an independent copy of an
// object stored in a table. It is invoked by MemDB at the input and
// output boundaries of a table when TableSchema.ObjectCopy is set.
//
// The returned object must be independent of the input object: mutating
// either one must not affect the other. For objects containing reference
// types (slices, maps, pointers) this typically requires a deep copy of
// those fields.
//
// Returning an error or a nil object aborts the operation with an error
// that identifies the table and the boundary at which the copy failed.
type ObjectCopyFunc func(obj interface{}) (interface{}, error)

// copyObjectIn copies an object crossing into the database. If no copy
// function is configured the object is returned unchanged.
func (s *TableSchema) copyObjectIn(obj interface{}) (interface{}, error) {
	if s.ObjectCopy == nil {
		return obj, nil
	}
	copied, err := s.ObjectCopy(obj)
	if err != nil {
		return nil, fmt.Errorf("table %q: failed to copy object on insert: %w", s.Name, err)
	}
	if copied == nil {
		return nil, fmt.Errorf("table %q: object copy function returned nil on insert", s.Name)
	}
	return copied, nil
}

// copyObjectOut copies an object crossing out of the database. If no copy
// function is configured the object is returned unchanged.
func (s *TableSchema) copyObjectOut(obj interface{}) (interface{}, error) {
	if s.ObjectCopy == nil {
		return obj, nil
	}
	copied, err := s.ObjectCopy(obj)
	if err != nil {
		return nil, fmt.Errorf("table %q: failed to copy object on read: %w", s.Name, err)
	}
	if copied == nil {
		return nil, fmt.Errorf("table %q: object copy function returned nil on read", s.Name)
	}
	return copied, nil
}

// Validate is used to validate the table schema
func (s *TableSchema) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("missing table name")
	}

	if len(s.Indexes) == 0 {
		return fmt.Errorf("missing table indexes for '%s'", s.Name)
	}

	if _, ok := s.Indexes["id"]; !ok {
		return fmt.Errorf("must have id index")
	}

	if !s.Indexes["id"].Unique {
		return fmt.Errorf("id index must be unique")
	}

	if _, ok := s.Indexes["id"].Indexer.(SingleIndexer); !ok {
		return fmt.Errorf("id index must be a SingleIndexer")
	}

	for name, index := range s.Indexes {
		if name != index.Name {
			return fmt.Errorf("index name mis-match for '%s'", name)
		}

		if err := index.Validate(); err != nil {
			return fmt.Errorf("index %q: %s", name, err)
		}
	}

	return nil
}

// IndexSchema is the schema for an index. An index defines how a table is
// queried.
type IndexSchema struct {
	// Name of the index. This must be unique among a tables set of indexes.
	// This must match the key in the map of Indexes for a TableSchema.
	Name string

	// AllowMissing if true ignores this index if it doesn't produce a
	// value. For example, an index that extracts a field that doesn't
	// exist from a structure.
	AllowMissing bool

	Unique  bool
	Indexer Indexer
}

func (s *IndexSchema) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("missing index name")
	}
	if s.Indexer == nil {
		return fmt.Errorf("missing index function for '%s'", s.Name)
	}
	switch s.Indexer.(type) {
	case SingleIndexer:
	case MultiIndexer:
	default:
		return fmt.Errorf("indexer for '%s' must be a SingleIndexer or MultiIndexer", s.Name)
	}
	return nil
}
