package gojq

import "fmt"

// DependencyKind is the kind of a dependency loaded during compilation.
type DependencyKind string

const (
	// DependencyKindModule is a jq module dependency.
	DependencyKindModule DependencyKind = "module"
	// DependencyKindJSON is a JSON data dependency.
	DependencyKindJSON DependencyKind = "json"
)

// Dependency is a dependency actually loaded during compilation. Name is the
// module or JSON name as requested by the query (the file path for init
// modules), Source is the canonical source identifier, and Digest is the
// digest of the loaded content.
type Dependency struct {
	Kind   DependencyKind `json:"kind"`
	Name   string         `json:"name"`
	Source string         `json:"source"`
	Digest string         `json:"digest"`
}

// DependencyManifest is the set of dependencies recorded during compilation.
// Marshal it to JSON to keep it as a lock file, and pass it to
// [WithDependencyLock] to make later compilations fail clearly when a
// dependency source or content changes.
type DependencyManifest struct {
	Dependencies []Dependency `json:"dependencies"`
}

func (m *DependencyManifest) verify(dep Dependency) error {
	for _, locked := range m.Dependencies {
		if locked.Kind == dep.Kind && locked.Name == dep.Name {
			switch {
			case locked.Source != dep.Source:
				return &DependencyLockError{dep.Kind, dep.Name,
					fmt.Sprintf("source changed from %q to %q", locked.Source, dep.Source)}
			case locked.Digest != dep.Digest:
				return &DependencyLockError{dep.Kind, dep.Name,
					fmt.Sprintf("content digest changed from %s to %s", locked.Digest, dep.Digest)}
			}
			return nil
		}
	}
	return &DependencyLockError{dep.Kind, dep.Name, "not found in the dependency lock manifest"}
}

// DependencyEvidence identifies the origin and the content of a loaded
// dependency. Module loaders may optionally provide evidence for their loads;
// see [ModuleLoader] for the optional evidence methods. Name identifies the
// dependency (used for init modules, which have no requested name), Source is
// the canonical source identifier, and Digest is the digest of the loaded
// content.
type DependencyEvidence struct {
	Name   string
	Source string
	Digest string
}

// DependencyRecorder records the dependencies actually loaded during
// compilation, including init modules and nested imports. Use it with
// [WithDependencyRecorder], and call [DependencyRecorder.Manifest] after a
// successful compilation to obtain the lock input for later compilations.
type DependencyRecorder struct {
	dependencies []Dependency
}

func (r *DependencyRecorder) record(dep Dependency) {
	for _, d := range r.dependencies {
		if d.Kind == dep.Kind && d.Name == dep.Name {
			return
		}
	}
	r.dependencies = append(r.dependencies, dep)
}

// Manifest returns the recorded dependencies as a manifest. The manifest can
// be marshaled to JSON and passed to [WithDependencyLock] to lock later
// compilations to the recorded sources and content.
func (r *DependencyRecorder) Manifest() *DependencyManifest {
	dependencies := make([]Dependency, len(r.dependencies))
	copy(dependencies, r.dependencies)
	return &DependencyManifest{Dependencies: dependencies}
}

// DependencyEvidenceError is returned when dependency recording or locking is
// enabled but the module loader does not provide evidence for a loaded
// dependency, so the dependency cannot be locked.
type DependencyEvidenceError struct {
	Kind DependencyKind
	Name string
}

func (err *DependencyEvidenceError) Error() string {
	if err.Name == "" {
		return "cannot lock init modules: module loader does not provide dependency evidence"
	}
	return fmt.Sprintf("cannot lock %s %q: module loader does not provide dependency evidence", err.Kind, err.Name)
}

// DependencyLockError is returned when a dependency loaded during compilation
// does not match the manifest given by [WithDependencyLock].
type DependencyLockError struct {
	Kind   DependencyKind
	Name   string
	Reason string
}

func (err *DependencyLockError) Error() string {
	return fmt.Sprintf("dependency lock failed for %s %q: %s", err.Kind, err.Name, err.Reason)
}

func (c *compiler) dependencyRecording() bool {
	return c.dependencyRecorder != nil || c.dependencyLock != nil
}

func (c *compiler) recordDependency(kind DependencyKind, name string, meta map[string]any) error {
	if !c.dependencyRecording() {
		return nil
	}
	moduleLoader, ok := c.moduleLoader.(interface {
		LoadDependencyEvidence(DependencyKind, string, map[string]any) (*DependencyEvidence, error)
	})
	if !ok {
		return &DependencyEvidenceError{Kind: kind, Name: name}
	}
	evidence, err := moduleLoader.LoadDependencyEvidence(kind, name, meta)
	if err != nil {
		return err
	}
	if evidence == nil {
		return &DependencyEvidenceError{Kind: kind, Name: name}
	}
	return c.checkDependency(Dependency{kind, name, evidence.Source, evidence.Digest})
}

func (c *compiler) recordInitDependencies(count int) error {
	if !c.dependencyRecording() || count == 0 {
		return nil
	}
	moduleLoader, ok := c.moduleLoader.(interface {
		LoadInitModulesEvidence() ([]DependencyEvidence, error)
	})
	if !ok {
		return &DependencyEvidenceError{Kind: DependencyKindModule}
	}
	evidences, err := moduleLoader.LoadInitModulesEvidence()
	if err != nil {
		return err
	}
	if len(evidences) != count {
		return fmt.Errorf("module loader returned %d init module evidences for %d init modules",
			len(evidences), count)
	}
	for _, evidence := range evidences {
		dep := Dependency{DependencyKindModule, evidence.Name, evidence.Source, evidence.Digest}
		if err := c.checkDependency(dep); err != nil {
			return err
		}
	}
	return nil
}

func (c *compiler) checkDependency(dep Dependency) error {
	if c.dependencyRecorder != nil {
		c.dependencyRecorder.record(dep)
	}
	if c.dependencyLock != nil {
		return c.dependencyLock.verify(dep)
	}
	return nil
}
