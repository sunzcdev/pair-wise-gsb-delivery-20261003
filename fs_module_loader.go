package gojq

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

// NewFSModuleLoader creates a new [ModuleLoader] which loads modules from the
// given [fs.FS], such as an [embed.FS] or an in-memory [testing/fstest.MapFS].
//
// Module names and JSON data imports are resolved as slash-separated logical
// paths relative to the given file system; the host environment, the current
// working directory and user home directory are never consulted. Absolute
// paths and any ".." segment that would escape the root of the file system
// are rejected. The given roots are searched in order for top-level imports;
// a root of "" or "." denotes the root of the file system.
//
// Relative imports nested inside a module resolve against the logical
// directory of that module, while a "search" metadata entry only adjusts the
// lookup location within the file system. Errors refer to logical file names
// and the import chain and never expose host file system paths.
func NewFSModuleLoader(fsys fs.FS, roots ...string) ModuleLoader {
	rs := make([]string, 0, len(roots))
	for _, root := range roots {
		if root = cleanLogicalPath(root); root == "." {
			root = ""
		}
		if root == "" || isValidLogicalPath(root) {
			rs = append(rs, root)
		}
	}
	if len(rs) == 0 {
		rs = []string{""}
	}
	return &fsModuleLoader{fsys: fsys, roots: rs}
}

// fsModuleLoader loads jq modules and JSON data from an [fs.FS].
type fsModuleLoader struct {
	fsys  fs.FS
	roots []string
}

// LoadInitModules loads the init module ".jq" located at each root.
func (l *fsModuleLoader) LoadInitModules() ([]*Query, error) {
	entries, err := l.loadInitModulesWithContext()
	if err != nil {
		return nil, err
	}
	qs := make([]*Query, 0, len(entries))
	for _, entry := range entries {
		qs = append(qs, entry.query)
	}
	return qs, nil
}

// loadInitModulesWithContext returns each init query together with the logical
// directory of its source file, so imports nested in an init module resolve
// relative to that directory.
func (l *fsModuleLoader) loadInitModulesWithContext() ([]fsInitModule, error) {
	var result []fsInitModule
	for _, root := range l.roots {
		file := path.Join(root, ".jq")
		q, err := l.loadModuleFile(file, nil)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		result = append(result, fsInitModule{q, logicalDir(file)})
	}
	return result, nil
}

// fsInitModule pairs an init query with the logical directory of its source.
type fsInitModule struct {
	query *Query
	dir   string
}

// logicalDir returns the slash-separated directory portion of a logical file
// path; the file system root is the empty string.
func logicalDir(file string) string {
	if index := strings.LastIndex(file, "/"); index >= 0 {
		return file[:index]
	}
	return ""
}

// LoadModuleWithMeta loads a jq module by its logical name.
func (l *fsModuleLoader) LoadModuleWithMeta(name string, meta map[string]any) (*Query, error) {
	return l.loadModuleWithMetaFrom(name, meta, "", nil)
}

// LoadJSONWithMeta loads JSON data by its logical name.
func (l *fsModuleLoader) LoadJSONWithMeta(name string, meta map[string]any) (any, error) {
	return l.loadJSONWithMetaFrom(name, meta, "", nil)
}

// loadModuleWithContext keeps relative imports anchored to the importing
// module's logical directory and threads the import chain into errors. It
// returns the query, the resolved logical file path and its directory. It is
// invoked by the compiler when nested modules are compiled.
func (l *fsModuleLoader) loadModuleWithContext(name string, meta map[string]any, dir string, chain []string) (*Query, string, string, error) {
	file, err := l.lookupFile(name, ".jq", meta, dir)
	if err != nil {
		return nil, "", "", l.importError(name, chain, err)
	}
	q, err := l.loadModuleFile(file, chain)
	if err != nil {
		return nil, "", "", err
	}
	return q, file, logicalDir(file), nil
}

// loadJSONWithContext is the data-import counterpart of loadModuleWithContext.
func (l *fsModuleLoader) loadJSONWithContext(name string, meta map[string]any, dir string, chain []string) (any, error) {
	return l.loadJSONWithMetaFrom(name, meta, dir, chain)
}

// loadModuleWithMetaFrom resolves name relative to dir (the logical directory
// of the importing module, empty for top-level imports) and reads the module.
func (l *fsModuleLoader) loadModuleWithMetaFrom(
	name string, meta map[string]any, dir string, chain []string,
) (*Query, error) {
	file, err := l.lookupFile(name, ".jq", meta, dir)
	if err != nil {
		return nil, l.importError(name, chain, err)
	}
	return l.loadModuleFile(file, chain)
}

func (l *fsModuleLoader) loadModuleFile(file string, chain []string) (*Query, error) {
	cnt, err := fs.ReadFile(l.fsys, file)
	if err != nil {
		return nil, l.importError(file, chain, err)
	}
	q, err := Parse(string(cnt))
	if err != nil {
		return nil, &queryParseError{file, string(cnt), err}
	}
	return q, nil
}

// loadJSONWithMetaFrom resolves name relative to dir and reads JSON data.
func (l *fsModuleLoader) loadJSONWithMetaFrom(
	name string, meta map[string]any, dir string, chain []string,
) (any, error) {
	file, err := l.lookupFile(name, ".json", meta, dir)
	if err != nil {
		return nil, l.importError(name, chain, err)
	}
	f, err := l.fsys.Open(file)
	if err != nil {
		return nil, l.importError(file, chain, err)
	}
	defer f.Close()
	vals := []any{}
	dec := json.NewDecoder(f)
	dec.UseNumber()
	for {
		var val any
		if err := dec.Decode(&val); err != nil {
			if err == io.EOF {
				break
			}
			cnt, readErr := fs.ReadFile(l.fsys, file)
			if readErr != nil {
				return nil, l.importError(file, chain, err)
			}
			return nil, &jsonParseError{file, string(cnt), err}
		}
		vals = append(vals, val)
	}
	return vals, nil
}

// lookupBases returns the ordered logical directories to search for an import.
// A "search" metadata entry is honored first and is confined to the file
// system, followed by the importing module's directory and the configured
// roots.
func (l *fsModuleLoader) lookupBases(meta map[string]any, dir string) []string {
	var bases []string
	if search, ok := meta["search"].(string); ok {
		if base := cleanLogicalPath(search); base != "." && isValidLogicalPath(base) {
			bases = append(bases, base)
		}
	}
	if dir != "" {
		bases = append(bases, dir)
	}
	bases = append(bases, l.roots...)
	return dedupeStrings(bases)
}

// lookupFile finds an existing, non-directory file for name and extension
// under the candidate bases, mirroring the "<name><ext>" and
// "<name>/<base><ext>" layouts of the file system module loader.
func (l *fsModuleLoader) lookupFile(
	name, extension string, meta map[string]any, dir string,
) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("invalid module name: %q", name)
	}
	if err := validateLogicalName(name); err != nil {
		return "", err
	}
	for _, base := range l.lookupBases(meta, dir) {
		candidates, err := moduleCandidates(base, name, extension)
		if err != nil {
			return "", err
		}
		for _, candidate := range candidates {
			ok, err := fileExists(l.fsys, candidate)
			if err != nil {
				return "", err
			}
			if ok {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("module not found: %q: %w", name, fs.ErrNotExist)
}

// moduleCandidates returns the cleaned logical candidate paths for a name.
func moduleCandidates(base, name, extension string) ([]string, error) {
	first, err := joinLogicalPath(base, name+extension)
	if err != nil {
		return nil, err
	}
	second, err := joinLogicalPath(base, path.Join(name, path.Base(name)+extension))
	if err != nil {
		return nil, err
	}
	if first == second {
		return []string{first}, nil
	}
	return []string{first, second}, nil
}

// importError wraps an error with the requested logical name and the import
// chain so that failures never expose host file system paths.
func (l *fsModuleLoader) importError(name string, chain []string, err error) error {
	if err == nil {
		return nil
	}
	return &fsModuleError{name: name, chain: append([]string(nil), chain...), err: err}
}

// fsModuleError is an error resolving or reading a module inside an [fs.FS].
type fsModuleError struct {
	name  string
	chain []string
	err   error
}

func (err *fsModuleError) Unwrap() error { return err.err }

func (err *fsModuleError) Error() string {
	var b strings.Builder
	if len(err.chain) > 0 {
		b.WriteString("module import chain ")
		for i, name := range err.chain {
			if i > 0 {
				b.WriteString(" -> ")
			}
			b.WriteString(name)
		}
		b.WriteString(" -> ")
	}
	b.WriteString(err.name)
	b.WriteString(": ")
	b.WriteString(err.err.Error())
	return b.String()
}

// fileExists reports whether the logical path exists and is not a directory.
func fileExists(fsys fs.FS, name string) (bool, error) {
	fi, err := fs.Stat(fsys, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !fi.IsDir(), nil
}

func dedupeStrings(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
