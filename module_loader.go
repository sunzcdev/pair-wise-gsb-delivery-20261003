package gojq

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ModuleLoader is the interface for loading modules.
//
// Implement following optional methods. Use [NewModuleLoader] to load local modules.
//
//	LoadInitModules() ([]*Query, error)
//	LoadModule(string) (*Query, error)
//	LoadModuleWithMeta(string, map[string]any) (*Query, error)
//	LoadJSON(string) (any, error)
//	LoadJSONWithMeta(string, map[string]any) (any, error)
//
// When compiling with [WithDependencyManifest], implement the evidence
// methods instead, so that the compiler can record the real loaded source
// path and raw content and lock future compilations against them:
//
//	LoadInitModulesWithEvidence() ([]*ModuleInitEvidence, error)
//	LoadModuleWithEvidence(string, map[string]any) (*ModuleEvidence, error)
//	LoadJSONWithEvidence(string, map[string]any) (*JSONEvidence, error)
//
// Without the evidence methods, a compilation with a dependency manifest
// fails because the dependencies cannot be locked.
type ModuleLoader any

// ModuleInitEvidence is an initialization module loaded with its content,
// returned by LoadInitModulesWithEvidence of a [ModuleLoader].
type ModuleInitEvidence struct {
	// Query is the parsed initialization module.
	Query *Query
	// Source is the canonical source identifier of the module, such as an
	// absolute file path.
	Source string
	// Content is the raw module content used for parsing Query.
	Content []byte
}

// ModuleEvidence is a jq module loaded with its content, returned by
// LoadModuleWithEvidence of a [ModuleLoader].
type ModuleEvidence struct {
	// Query is the parsed module.
	Query *Query
	// Source is the canonical source identifier of the module, such as an
	// absolute file path.
	Source string
	// Content is the raw module content used for parsing Query.
	Content []byte
}

// JSONEvidence is a JSON data file loaded with its content, returned by
// LoadJSONWithEvidence of a [ModuleLoader].
type JSONEvidence struct {
	// Value is the decoded JSON value (a slice of the decoded values
	// when the file contains concatenated JSON values).
	Value any
	// Source is the canonical source identifier of the data, such as an
	// absolute file path.
	Source string
	// Content is the raw file content decoded into Value.
	Content []byte
}

// NewModuleLoader creates a new [ModuleLoader] loading local modules in the paths.
// Note that user can load modules outside the paths using "search" path of metadata.
// Empty paths are ignored, so specify "." for the current working directory.
func NewModuleLoader(paths []string) ModuleLoader {
	ps := make([]string, 0, len(paths))
	for _, path := range paths {
		if path = resolvePath(path, ""); path != "" {
			ps = append(ps, path)
		}
	}
	return &moduleLoader{ps}
}

type moduleLoader struct {
	paths []string
}

func (l *moduleLoader) LoadInitModules() ([]*Query, error) {
	es, err := l.LoadInitModulesWithEvidence()
	if err != nil {
		return nil, err
	}
	qs := make([]*Query, 0, len(es))
	for _, e := range es {
		qs = append(qs, e.Query)
	}
	return qs, nil
}

func (l *moduleLoader) LoadInitModulesWithEvidence() ([]*ModuleInitEvidence, error) {
	var es []*ModuleInitEvidence
	for _, path := range l.paths {
		if filepath.Base(path) != ".jq" {
			continue
		}
		source := path
		if abs, err := filepath.Abs(path); err == nil {
			source = abs
		}
		fi, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if fi.IsDir() {
			continue
		}
		cnt, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		q, err := parseModule(string(cnt), filepath.Dir(path))
		if err != nil {
			return nil, &queryParseError{path, string(cnt), err}
		}
		es = append(es, &ModuleInitEvidence{
			Query:   q,
			Source:  source,
			Content: cnt,
		})
	}
	return es, nil
}

func (l *moduleLoader) LoadModuleWithMeta(name string, meta map[string]any) (*Query, error) {
	e, err := l.LoadModuleWithEvidence(name, meta)
	if err != nil {
		return nil, err
	}
	return e.Query, nil
}

func (l *moduleLoader) LoadModuleWithEvidence(name string, meta map[string]any) (*ModuleEvidence, error) {
	path, err := l.lookupModule(name, ".jq", meta)
	if err != nil {
		return nil, err
	}
	source := path
	if abs, err := filepath.Abs(path); err == nil {
		source = abs
	}
	cnt, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	q, err := parseModule(string(cnt), filepath.Dir(path))
	if err != nil {
		return nil, &queryParseError{path, string(cnt), err}
	}
	return &ModuleEvidence{Query: q, Source: source, Content: cnt}, nil
}

func (l *moduleLoader) LoadJSONWithMeta(name string, meta map[string]any) (any, error) {
	e, err := l.LoadJSONWithEvidence(name, meta)
	if err != nil {
		return nil, err
	}
	return e.Value, nil
}

func (l *moduleLoader) LoadJSONWithEvidence(name string, meta map[string]any) (*JSONEvidence, error) {
	path, err := l.lookupModule(name, ".json", meta)
	if err != nil {
		return nil, err
	}
	source := path
	if abs, err := filepath.Abs(path); err == nil {
		source = abs
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cnt, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	vals := []any{}
	dec := json.NewDecoder(bytes.NewReader(cnt))
	dec.UseNumber()
	for {
		var val any
		if err := dec.Decode(&val); err != nil {
			if err == io.EOF {
				break
			}
			return nil, &jsonParseError{path, string(cnt), err}
		}
		vals = append(vals, val)
	}
	return &JSONEvidence{Value: vals, Source: source, Content: cnt}, nil
}

func (l *moduleLoader) lookupModule(name, extension string, meta map[string]any) (string, error) {
	paths := l.paths
	if path, ok := meta["search"].(string); ok {
		if path = resolvePath(path, ""); path != "" {
			paths = append([]string{path}, paths...)
		}
	}
	for _, base := range paths {
		path := filepath.Join(base, name+extension)
		if _, err := os.Stat(path); err == nil {
			return path, err
		}
		path = filepath.Join(base, name, filepath.Base(name)+extension)
		if _, err := os.Stat(path); err == nil {
			return path, err
		}
	}
	return "", fmt.Errorf("module not found: %q", name)
}

func parseModule(cnt, dir string) (*Query, error) {
	q, err := Parse(cnt)
	if err != nil {
		return nil, err
	}
	for _, i := range q.Imports {
		if i.Meta != nil {
			for _, e := range i.Meta.KeyVals {
				if e.Key == "search" || e.KeyString == "search" {
					if path, ok := e.Val.toString(); ok {
						if path = resolvePath(path, dir); path != "" {
							e.Val.Str = path
						} else {
							e.Val.Null = true
						}
					}
				}
			}
		}
	}
	return q, nil
}

func resolvePath(path, dir string) string {
	switch {
	case filepath.IsAbs(path):
		return path
	case strings.HasPrefix(path, "~/"):
		dir, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(dir, path[2:])
	case strings.HasPrefix(path, "$ORIGIN/"):
		exe, err := os.Executable()
		if err != nil {
			return ""
		}
		exe, err = filepath.EvalSymlinks(exe)
		if err != nil {
			return ""
		}
		return filepath.Join(filepath.Dir(exe), path[8:])
	default:
		return filepath.Join(dir, path)
	}
}
