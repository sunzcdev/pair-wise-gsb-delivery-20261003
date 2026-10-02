package versioned

import "fmt"

// SchemaNotFoundError indicates that the schema id embedded in the data is
// unknown to the Registry.
type SchemaNotFoundError struct {
	Schema string
}

func (e *SchemaNotFoundError) Error() string {
	return "versioned: schema not registered: " + e.Schema
}

// VersionNotFoundError indicates that a schema id is known but the version
// embedded in the data is not registered.
type VersionNotFoundError struct {
	Schema  string
	Version uint32
}

func (e *VersionNotFoundError) Error() string {
	return fmt.Sprintf("versioned: version %d of schema %q not registered", e.Version, e.Schema)
}

// MigrationNotFoundError indicates that the caller did not register a
// conversion path between two versions.
//
// The library never guesses migrations (for example from similar field names);
// the caller must register every required conversion.
type MigrationNotFoundError struct {
	Schema string
	From   uint32
	To     uint32
}

func (e *MigrationNotFoundError) Error() string {
	return fmt.Sprintf(
		"versioned: no registered migration for schema %q from version %d to %d",
		e.Schema, e.From, e.To)
}

// FormatError marks a payload that is not a valid versioned record envelope.
type FormatError struct {
	Reason string
}

func (e *FormatError) Error() string {
	return "versioned: invalid versioned record: " + e.Reason
}
