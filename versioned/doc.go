// Package versioned adds an optional, version-aware encoding layer on top of
// MessagePack.
//
// The classic msgpack.Marshal / msgpack.Unmarshal functions are completely
// unaffected by this package: a type only becomes versioned when its caller
// registers it with a Registry and marshals it through that Registry.
//
// # Wire format
//
// A versioned message is encoded as a single MessagePack map with integer
// keys:
//
//   - key -1 holds the schema id (a string),
//   - key -2 holds the schema version (a uint >= 1),
//   - every other key (>= 1) is a stable field identity declared by the caller
//     with a `versioned:"<id>"` struct tag.
//
// Because the schema id and version are bytes in the record itself, they travel
// with the data and are never inferred from deployment configuration. Field
// identities are integers assigned by the caller, not Go field names, so a Go
// field can be renamed freely as long as it keeps its id.
//
// # Evolving a schema
//
// Each structural version is registered separately with Register. Converting
// between versions is explicit: the caller registers MigrateFunc steps with
// RegisterMigration, and Unmarshal composes the shortest chain of registered
// steps to reach the target version. There is deliberately no name-similarity
// matching or any other automatic migration guess; if no path is registered,
// decoding fails with MigrationNotFoundError. A MigrateFunc may also fail when
// a value cannot be converted losslessly (for example a fractional float into
// an integer field).
package versioned
