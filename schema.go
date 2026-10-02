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

	// Reference targets and index types can only be checked against the
	// complete set of tables.
	for name, table := range s.Tables {
		for _, ref := range table.References {
			if err := ref.validateCrossTable(s, name); err != nil {
				return fmt.Errorf("table %q reference %q: %s", name, ref.Name, err)
			}
		}
	}

	return nil
}

// TableSchema is the schema for a single table.
type TableSchema struct {
	// Name of the table. This must match the key in the Tables map in DBSchema.
	Name string

	// Indexes is the set of indexes for querying this table. The key
	// is a unique name for the index and must match the Name in the
	// IndexSchema.
	Indexes map[string]*IndexSchema

	// References declares inter-table references (foreign keys) from this
	// table (the child) to another table (the parent). Referential
	// integrity is checked when a write transaction commits, so the same
	// transaction may insert the child before the referenced parent.
	// Tables without references continue to behave exactly as before.
	References []*ReferenceSchema
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

	refNames := make(map[string]struct{}, len(s.References))
	for _, ref := range s.References {
		if ref == nil {
			return fmt.Errorf("nil reference in table '%s'", s.Name)
		}
		if _, ok := refNames[ref.Name]; ok {
			return fmt.Errorf("duplicate reference name '%s'", ref.Name)
		}
		refNames[ref.Name] = struct{}{}
		if err := ref.validate(s); err != nil {
			return fmt.Errorf("reference %q: %s", ref.Name, err)
		}
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

// Delete action policies for a reference.
const (
	// ReferenceActionRestrict prevents deleting a parent row while it is
	// still referenced by a surviving child row. This is the default when
	// no action is specified.
	ReferenceActionRestrict = "restrict"

	// ReferenceActionCascade deletes the referencing child rows when the
	// referenced parent row is deleted. Cascading deletes propagate
	// transitively and each distinct child row is deleted at most once.
	ReferenceActionCascade = "cascade"
)

// ReferenceSchema declares a reference (foreign key) from a column of the
// containing (child) table to a unique index of another (parent) table.
//
// The local column is identified by an index name in the child table; the
// same indexer is used both to build the child's key and to look the parent
// up, so reference columns must use a SingleIndexer. A child whose indexer
// reports no value (an AllowMissing index) is treated as having a NULL
// foreign key and is not required to reference any parent.
type ReferenceSchema struct {
	// Name uniquely identifies the reference within the child table.
	Name string

	// LocalIndex is the name of an index in the child table that holds
	// the foreign key value. It must use a SingleIndexer.
	LocalIndex string

	// ForeignTable is the name of the referenced (parent) table.
	ForeignTable string

	// ForeignIndex is the name of a unique SingleIndexer index in the
	// parent table. Defaults to "id" when empty.
	ForeignIndex string

	// OnDelete selects the behavior when a referenced parent is deleted:
	// ReferenceActionRestrict or ReferenceActionCascade. Defaults to
	// ReferenceActionRestrict when empty.
	OnDelete string
}

func (r *ReferenceSchema) validate(child *TableSchema) error {
	if r.Name == "" {
		return fmt.Errorf("missing reference name")
	}

	if r.LocalIndex == "" {
		return fmt.Errorf("missing local index")
	}
	local, ok := child.Indexes[r.LocalIndex]
	if !ok {
		return fmt.Errorf("local index '%s' does not exist", r.LocalIndex)
	}
	if _, ok := local.Indexer.(SingleIndexer); !ok {
		return fmt.Errorf("local index '%s' must use a SingleIndexer", r.LocalIndex)
	}

	if r.ForeignTable == "" {
		return fmt.Errorf("missing foreign table")
	}

	switch r.OnDelete {
	case "", ReferenceActionRestrict, ReferenceActionCascade:
	default:
		return fmt.Errorf("invalid on-delete action %q", r.OnDelete)
	}

	return nil
}

func (r *ReferenceSchema) action() string {
	if r.OnDelete == "" {
		return ReferenceActionRestrict
	}
	return r.OnDelete
}

func (r *ReferenceSchema) foreignIndexName() string {
	if r.ForeignIndex == "" {
		return id
	}
	return r.ForeignIndex
}

// validateCrossTable checks reference targets that can only be verified once
// the whole database schema is known. It is called by DBSchema.Validate.
func (r *ReferenceSchema) validateCrossTable(schema *DBSchema, childName string) error {
	parent, ok := schema.Tables[r.ForeignTable]
	if !ok {
		return fmt.Errorf("foreign table '%s' does not exist", r.ForeignTable)
	}

	fName := r.foreignIndexName()
	foreign, ok := parent.Indexes[fName]
	if !ok {
		return fmt.Errorf("foreign index '%s' does not exist on table '%s'", fName, r.ForeignTable)
	}
	if !foreign.Unique {
		return fmt.Errorf("foreign index '%s' on table '%s' must be unique", fName, r.ForeignTable)
	}

	child := schema.Tables[childName]
	local := child.Indexes[r.LocalIndex]
	localType := fmt.Sprintf("%T", local.Indexer)
	foreignType := fmt.Sprintf("%T", foreign.Indexer)
	if localType != foreignType {
		return fmt.Errorf(
			"local index '%s' (%s) and foreign index '%s.%s' (%s) must use the same indexer type",
			r.LocalIndex, localType, r.ForeignTable, fName, foreignType)
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
