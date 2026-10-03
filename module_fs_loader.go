package gojq

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

// NewModuleFSLoader creates a new [ModuleLoader] loading modules from fsys,
// which is useful for embedded file systems ([embed.FS]) or in-memory file
// systems (such as [testing/fstest.MapFS]).
//
// Module names and JSON data imports are resolved as slash-separated logical
// paths relative to the given paths, or the root of fsys if paths is empty.
// Absolute paths and paths escaping the root with ".." are rejected, and the
// loader never reads from the host environment nor the current working
// directory. The "search" path of import metadata only adjusts the lookup
// position relative to the importing module within fsys, and nested imports
// retain the source directory of the importing module. Errors report logical
// file names with the import chain, and never leak host paths.
func NewModuleFSLoader(fsys fs.FS, paths []string) ModuleLoader {
	ps := make([]string, 0, len(paths))
	for _, p := range paths {
		if p = cleanFSPath(p); p != "" {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		ps = []string{"."}
	}
	return &moduleFSLoader{fsys, ps}
}

type moduleFSLoader struct {
	fsys  fs.FS
	paths []string
}

func (l *moduleFSLoader) LoadInitModules() ([]*Query, error) {
	var qs []*Query
	for _, p := range l.paths {
		if path.Base(p) != ".jq" {
			continue
		}
		cnt, err := fs.ReadFile(l.fsys, p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		q, err := parseModuleFS(string(cnt), path.Dir(p))
		if err != nil {
			return nil, &queryParseError{p, string(cnt), err}
		}
		qs = append(qs, q)
	}
	return qs, nil
}

func (l *moduleFSLoader) LoadModuleWithMeta(name string, meta map[string]any) (*Query, error) {
	p, err := l.lookupModule(name, ".jq", meta)
	if err != nil {
		return nil, err
	}
	cnt, err := fs.ReadFile(l.fsys, p)
	if err != nil {
		return nil, err
	}
	q, err := parseModuleFS(string(cnt), path.Dir(p))
	if err != nil {
		return nil, &queryParseError{p, string(cnt), err}
	}
	return q, nil
}

func (l *moduleFSLoader) LoadJSONWithMeta(name string, meta map[string]any) (any, error) {
	p, err := l.lookupModule(name, ".json", meta)
	if err != nil {
		return nil, err
	}
	cnt, err := fs.ReadFile(l.fsys, p)
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
			return nil, &jsonParseError{p, string(cnt), err}
		}
		vals = append(vals, val)
	}
	return vals, nil
}

func (l *moduleFSLoader) lookupModule(name, extension string, meta map[string]any) (string, error) {
	p := cleanFSPath(name)
	if p == "" {
		return "", fmt.Errorf("invalid module name: %q", name)
	}
	name = p
	paths := l.paths
	if search, ok := meta["search"].(string); ok {
		if search = cleanFSPath(search); search != "" {
			paths = append([]string{search}, paths...)
		}
	}
	for _, base := range paths {
		p = path.Join(base, name+extension)
		if _, err := fs.Stat(l.fsys, p); err == nil {
			return p, nil
		}
		p = path.Join(base, name, path.Base(name)+extension)
		if _, err := fs.Stat(l.fsys, p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("module not found: %q", name)
}

func parseModuleFS(cnt, dir string) (*Query, error) {
	q, err := Parse(cnt)
	if err != nil {
		return nil, err
	}
	for _, i := range q.Imports {
		if i.Meta != nil {
			for _, e := range i.Meta.KeyVals {
				if e.Key == "search" || e.KeyString == "search" {
					if p, ok := e.Val.toString(); ok {
						if p = resolveFSPath(p, dir); p != "" {
							e.Val.Str = p
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

func resolveFSPath(name, dir string) string {
	if path.IsAbs(name) {
		return ""
	}
	return cleanFSPath(path.Join(dir, name))
}

func cleanFSPath(name string) string {
	if name == "" || path.IsAbs(name) {
		return ""
	}
	name = path.Clean(name)
	if name == ".." || strings.HasPrefix(name, "../") {
		return ""
	}
	return name
}
