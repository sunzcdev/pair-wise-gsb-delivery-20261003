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

	// Validate cross-table foreign key references.
	for name, table := range s.Tables {
		for _, fk := range table.ForeignKeys {
			parent, ok := s.Tables[fk.Table]
			if !ok {
				return fmt.Errorf("table %q: foreign key references unknown table %q", name, fk.Table)
			}
			refIndex := fk.RefIndex
			if refIndex == "" {
				refIndex = "id"
			}
			refSchema, ok := parent.Indexes[refIndex]
			if !ok {
				return fmt.Errorf("table %q: foreign key references unknown index %q on table %q", name, refIndex, fk.Table)
			}
			if !refSchema.Unique {
				return fmt.Errorf("table %q: foreign key referenced index %q on table %q must be unique", name, refIndex, fk.Table)
			}
		}
	}

	return nil
}

// RefAction is the action taken when a referenced (parent) object is
// deleted while child objects still reference it.
type RefAction int

const (
	// Restrict prevents the deletion of a parent object while child
	// objects still reference it. The violation is reported when the
	// transaction is committed via CommitWithError.
	Restrict RefAction = iota

	// Cascade deletes child objects that reference a deleted parent
	// object. Cascading is applied recursively and each object is
	// processed at most once, so reference cycles and diamonds are safe.
	Cascade
)

// ForeignKeySchema declares a reference from the table that declares it
// (the child) to another table (the parent). Referential integrity is
// validated when a write transaction is committed, so within a single
// transaction child objects may be inserted before their parents.
//
// The child index and the referenced parent index must encode values
// identically (for example, both using StringFieldIndex), since the
// encoded child value is used to look up the parent object.
type ForeignKeySchema struct {
	// Table is the name of the referenced (parent) table.
	Table string

	// Index is the name of the index in the child table that holds the
	// reference value. It must be a SingleIndexer.
	Index string

	// RefIndex is the name of the index in the parent table that is
	// referenced. It must be unique. If empty, the "id" index is used.
	RefIndex string

	// OnDelete is the action taken when a referenced parent object is
	// deleted, either Restrict or Cascade.
	OnDelete RefAction
}

// TableSchema is the schema for a single table.
type TableSchema struct {
	// Name of the table. This must match the key in the Tables map in DBSchema.
	Name string

	// Indexes is the set of indexes for querying this table. The key
	// is a unique name for the index and must match the Name in the
	// IndexSchema.
	Indexes map[string]*IndexSchema

	// ForeignKeys is the set of references from this table to parent
	// tables. Tables without foreign keys are unaffected by referential
	// integrity validation.
	ForeignKeys []*ForeignKeySchema
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

	for _, fk := range s.ForeignKeys {
		if fk.Table == "" {
			return fmt.Errorf("foreign key missing referenced table")
		}
		if fk.Index == "" {
			return fmt.Errorf("foreign key missing index")
		}
		index, ok := s.Indexes[fk.Index]
		if !ok {
			return fmt.Errorf("foreign key references unknown index %q", fk.Index)
		}
		if _, ok := index.Indexer.(SingleIndexer); !ok {
			return fmt.Errorf("foreign key index %q must be a SingleIndexer", fk.Index)
		}
		if fk.OnDelete != Restrict && fk.OnDelete != Cascade {
			return fmt.Errorf("foreign key on index %q has invalid delete action", fk.Index)
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
