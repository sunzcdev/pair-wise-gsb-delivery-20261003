package gojq

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// DependencyKind is the kind of a dependency recorded in [DependencyManifest].
type DependencyKind int

const (
	// DependencyInitModule is an initialization module loaded via
	// LoadInitModules of the module loader.
	DependencyInitModule DependencyKind = iota + 1
	// DependencyModule is a jq module loaded by an "import" or "include"
	// declaration (or modulemeta).
	DependencyModule
	// DependencyJSON is a JSON data file loaded by an
	// "import ... as $name" declaration.
	DependencyJSON
)

func (k DependencyKind) String() string {
	switch k {
	case DependencyInitModule:
		return "init"
	case DependencyModule:
		return "module"
	case DependencyJSON:
		return "json"
	default:
		return "unknown"
	}
}

func (k DependencyKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

func (k *DependencyKind) UnmarshalJSON(cnt []byte) error {
	var s string
	if err := json.Unmarshal(cnt, &s); err != nil {
		return err
	}
	switch s {
	case "init":
		*k = DependencyInitModule
	case "module":
		*k = DependencyModule
	case "json":
		*k = DependencyJSON
	default:
		return fmt.Errorf("unknown dependency kind: %q", s)
	}
	return nil
}

// Dependency is a jq module or JSON data file actually loaded during
// compilation. The same jq dependency must resolve to the same Source and
// Digest across compilations for a lock to stay valid.
type Dependency struct {
	// Kind is the kind of the dependency.
	Kind DependencyKind
	// Name is the module or data name used in the import declaration;
	// it is empty for initialization modules (identify them by Source).
	Name string
	// Source is the canonical identifier of where the content was loaded
	// from, such as an absolute file path reported by the module loader.
	Source string
	// Size is the length of the loaded content in bytes.
	Size int
	// Digest is the hexadecimal SHA-256 digest of the loaded content,
	// prefixed with "sha256:".
	Digest string
}

type dependencyJSON struct {
	Kind   DependencyKind `json:"kind"`
	Name   string         `json:"name,omitempty"`
	Source string         `json:"source"`
	Size   int            `json:"size"`
	Digest string         `json:"digest"`
}

// MarshalJSON marshals the dependency as JSON.
func (d *Dependency) MarshalJSON() ([]byte, error) {
	return json.Marshal(dependencyJSON{
		Kind:   d.Kind,
		Name:   d.Name,
		Source: d.Source,
		Size:   d.Size,
		Digest: d.Digest,
	})
}

// UnmarshalJSON unmarshals the dependency from JSON.
func (d *Dependency) UnmarshalJSON(cnt []byte) error {
	var x dependencyJSON
	if err := json.Unmarshal(cnt, &x); err != nil {
		return err
	}
	*d = Dependency{
		Kind:   x.Kind,
		Name:   x.Name,
		Source: x.Source,
		Size:   x.Size,
		Digest: x.Digest,
	}
	return nil
}

// DependencyManifest records the jq modules and JSON data actually loaded
// during a compilation. Create an empty manifest with [NewDependencyManifest]
// and pass it to [Compile] with [WithDependencyManifest] to record
// dependencies; serialize it (for example with json.Marshal) and reuse the
// deserialized manifest in a later compilation to lock the dependencies.
// The compilation fails when a dependency is missing from the lock, changed
// (different source or content digest), or left unused by the query.
type DependencyManifest struct {
	locked       bool
	dependencies []Dependency
	byKey        map[dependencyKey]int
}

// NewDependencyManifest creates an empty dependency manifest. Pass it to
// [Compile] with [WithDependencyManifest] to record dependencies.
func NewDependencyManifest() *DependencyManifest {
	return &DependencyManifest{}
}

type dependencyKey struct {
	kind   DependencyKind
	name   string
	source string
}

// Dependencies returns the recorded dependencies in the order they were
// loaded (deduplicated by kind, name and source).
func (m *DependencyManifest) Dependencies() []Dependency {
	if m == nil {
		return nil
	}
	return append([]Dependency(nil), m.dependencies...)
}

// MarshalJSON marshals the manifest as JSON.
func (m *DependencyManifest) MarshalJSON() ([]byte, error) {
	ds := make([]Dependency, len(m.dependencies))
	copy(ds, m.dependencies)
	return json.Marshal(struct {
		Version      int          `json:"version"`
		Dependencies []Dependency `json:"dependencies"`
	}{1, ds})
}

// UnmarshalJSON unmarshals the manifest from JSON. The result is a locked
// manifest: compilations with it verify that every dependency is used and
// that sources and contents do not change.
func (m *DependencyManifest) UnmarshalJSON(cnt []byte) error {
	var x struct {
		Version      int          `json:"version"`
		Dependencies []Dependency `json:"dependencies"`
	}
	if err := json.Unmarshal(cnt, &x); err != nil {
		return err
	}
	if x.Version != 1 {
		return fmt.Errorf("unsupported dependency manifest version: %d", x.Version)
	}
	nm := NewDependencyManifest()
	nm.locked = true
	for i, d := range x.Dependencies {
		if err := d.validate(); err != nil {
			return fmt.Errorf("invalid dependency at index %d: %w", i, err)
		}
		key := dependencyKey{d.Kind, d.Name, d.Source}
		if j, ok := nm.byKey[key]; ok {
			return &dependencyLockError{
				reason: dependencyLockDuplicate,
				dep:    d,
				msg: fmt.Sprintf(
					"duplicate locked dependency %s (also at index %d)",
					d.identity(), j),
			}
		}
		if nm.byKey == nil {
			nm.byKey = map[dependencyKey]int{}
		}
		nm.byKey[key] = len(nm.dependencies)
		nm.dependencies = append(nm.dependencies, d)
	}
	*m = *nm
	return nil
}

func (d *Dependency) validate() error {
	if d.Kind != DependencyInitModule &&
		d.Kind != DependencyModule &&
		d.Kind != DependencyJSON {
		return fmt.Errorf("invalid kind: %d", d.Kind)
	}
	if d.Source == "" {
		return fmt.Errorf("empty source")
	}
	if d.Size < 0 {
		return fmt.Errorf("negative size: %d", d.Size)
	}
	const prefix = "sha256:"
	if len(d.Digest) != len(prefix)+hex.EncodedLen(sha256.Size) ||
		d.Digest[:len(prefix)] != prefix {
		return fmt.Errorf("invalid digest: %q", d.Digest)
	}
	if _, err := hex.DecodeString(d.Digest[len(prefix):]); err != nil {
		return fmt.Errorf("invalid digest: %q", d.Digest)
	}
	return nil
}

func (d Dependency) identity() string {
	if d.Kind == DependencyInitModule {
		return "init module " + quoteString(d.Source)
	}
	return d.Kind.String() + " " + quoteString(d.Name) +
		" (" + d.Source + ")"
}

func quoteString(s string) string {
	cnt, err := json.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return string(cnt)
}

func dependencyDigest(cnt []byte) (int, string) {
	sum := sha256.Sum256(cnt)
	return len(cnt), "sha256:" + hex.EncodeToString(sum[:])
}

type dependencyLockReason int

const (
	dependencyLockChanged dependencyLockReason = iota
	dependencyLockMissing
	dependencyLockUnused
	dependencyLockDuplicate
)

type dependencyLockError struct {
	reason dependencyLockReason
	dep    Dependency
	want   Dependency
	msg    string
}

func (err *dependencyLockError) Error() string {
	if err.msg != "" {
		return "dependency lock: " + err.msg
	}
	switch err.reason {
	case dependencyLockMissing:
		return "dependency lock: " + err.dep.identity() +
			" is not locked; record the dependencies with an empty manifest first"
	case dependencyLockChanged:
		if err.dep.Source != err.want.Source {
			return "dependency lock: " + err.want.identity() +
				" source changed: " + quoteString(err.want.Source) +
				" (locked as " + quoteString(err.dep.Source) + ")"
		}
		return "dependency lock: " + err.dep.identity() +
			" content changed: got size " + fmt.Sprint(err.want.Size) +
			" digest " + err.want.Digest +
			" (locked size " + fmt.Sprint(err.dep.Size) +
			" digest " + err.dep.Digest + ")"
	case dependencyLockUnused:
		return "dependency lock: " + err.dep.identity() +
			" is locked but not loaded by the query"
	case dependencyLockDuplicate:
		return "dependency lock: " + err.msg
	default:
		return "dependency lock: mismatch"
	}
}

type dependencyEvidenceError struct {
	loader string
	dep    Dependency
}

func (err *dependencyEvidenceError) Error() string {
	if err.dep.Kind == DependencyInitModule {
		return "dependency lock: module loader " + err.loader +
			" does not implement LoadInitModulesWithEvidence;" +
			" cannot lock initialization modules"
	}
	return "dependency lock: module loader " + err.loader +
		" does not implement the evidence interface" +
		" (LoadModuleWithEvidence or LoadJSONWithEvidence); cannot lock " +
		err.dep.identity()
}

func loaderTypeName(loader ModuleLoader) string {
	t := fmt.Sprintf("%T", loader)
	if t == "" {
		return "module loader"
	}
	return t
}

// recordDependency appends a loaded dependency in record mode or verifies
// it against the locked entries in lock mode. It returns an error when the
// dependency cannot be locked by the loader or does not match the lock.
func (c *compiler) recordDependency(dep Dependency, cnt []byte) error {
	m := c.dependencyManifest
	if m == nil {
		return nil
	}
	if dep.Size == 0 && len(cnt) > 0 {
		dep.Size = len(cnt)
	}
	if dep.Digest == "" && cnt != nil {
		dep.Size, dep.Digest = dependencyDigest(cnt)
	}
	if err := dep.validate(); err != nil {
		return err
	}
	key := dependencyKey{dep.Kind, dep.Name, dep.Source}
	if m.locked {
		i, ok := m.byKey[key]
		if !ok {
			// A dependency with the same name but a different source is
			// still a missing lock, not a content change.
			return &dependencyLockError{reason: dependencyLockMissing, dep: dep}
		}
		locked := m.dependencies[i]
		if locked.Digest != dep.Digest || locked.Size != dep.Size {
			return &dependencyLockError{
				reason: dependencyLockChanged, dep: locked, want: dep,
			}
		}
		m.byKey[key] = ^i
		return nil
	}
	if i, ok := m.byKey[key]; ok {
		got := m.dependencies[i]
		if got.Digest != dep.Digest || got.Size != dep.Size {
			return &dependencyLockError{
				reason: dependencyLockChanged, dep: got, want: dep,
				msg: got.identity() + " was loaded with different contents",
			}
		}
		return nil
	}
	if m.byKey == nil {
		m.byKey = map[dependencyKey]int{}
	}
	m.byKey[key] = len(m.dependencies)
	m.dependencies = append(m.dependencies, dep)
	return nil
}

// finishDependencyManifest ensures that every locked dependency was loaded.
func (c *compiler) finishDependencyManifest() error {
	m := c.dependencyManifest
	if m == nil || !m.locked {
		return nil
	}
	var unused []int
	for _, i := range m.byKey {
		if i >= 0 {
			unused = append(unused, i)
		}
	}
	if len(unused) == 0 {
		return nil
	}
	sort.Ints(unused)
	return &dependencyLockError{
		reason: dependencyLockUnused,
		dep:    m.dependencies[unused[0]],
	}
}
