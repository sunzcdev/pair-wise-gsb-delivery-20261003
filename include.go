package aconfig

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// IncludeDecoder is an optional extension of FileDecoder.
// Decoders that implement it can report files referenced (included)
// by the decoded file. Includes are resolved relative to the referencing
// file and are loaded before it, so the referencing file overrides them.
type IncludeDecoder interface {
	// DecodeFileIncludes decodes a file and returns its fields together
	// with the list of files it includes.
	DecodeFileIncludes(filename string) (map[string]any, []string, error)
}

// resolvedFile is a decoded config file ready to be applied.
type resolvedFile struct {
	path    string
	decoder FileDecoder
	fields  map[string]any
}

// includeResolver decodes a file and all of its includes exactly once per
// load, producing a deterministic application order: dependencies first
// (in the order they are listed), the referencing file last.
type includeResolver struct {
	l       *Loader
	loaded  map[string]bool // canonical paths fully resolved in this load
	onStack map[string]int  // canonical path -> index in stack
	stack   []string        // canonical paths of the current resolution chain
	files   []resolvedFile  // post-order: dependencies before referrers
}

func newIncludeResolver(l *Loader) *includeResolver {
	return &includeResolver{
		l:       l,
		loaded:  map[string]bool{},
		onStack: map[string]int{},
	}
}

// resolve decodes the given file and all of its includes and returns the
// newly resolved files in the order they must be applied.
func (r *includeResolver) resolve(file string) ([]resolvedFile, error) {
	before := len(r.files)
	if err := r.resolveFile(cleanPath(file)); err != nil {
		return nil, err
	}
	return r.files[before:], nil
}

func (r *includeResolver) resolveFile(canon string) error {
	if r.loaded[canon] {
		return nil
	}
	if idx, ok := r.onStack[canon]; ok {
		chain := append(append([]string{}, r.stack[idx:]...), canon)
		return fmt.Errorf("include cycle detected: %s", strings.Join(chain, " -> "))
	}

	ext := strings.ToLower(filepath.Ext(canon))
	decoder, ok := r.l.config.FileDecoders[ext]
	if !ok {
		return fmt.Errorf("file format %q is not supported", ext)
	}

	var (
		fields   map[string]any
		includes []string
		err      error
	)
	if dec, ok := decoder.(IncludeDecoder); ok {
		fields, includes, err = dec.DecodeFileIncludes(canon)
	} else {
		fields, err = decoder.DecodeFile(canon)
	}
	if err != nil {
		return fmt.Errorf("load %q: %w", canon, err)
	}

	r.onStack[canon] = len(r.stack)
	r.stack = append(r.stack, canon)
	defer func() {
		delete(r.onStack, canon)
		r.stack = r.stack[:len(r.stack)-1]
	}()

	dir := path.Dir(canon)
	for _, include := range includes {
		sub, err := resolveIncludePath(dir, include)
		if err != nil {
			return fmt.Errorf("in %q: %w", canon, err)
		}
		if err := r.resolveFile(sub); err != nil {
			return err
		}
	}

	r.loaded[canon] = true
	r.files = append(r.files, resolvedFile{path: canon, decoder: decoder, fields: fields})
	return nil
}

// resolveIncludePath resolves an include path relative to the directory of
// the referencing file and ensures it stays inside the filesystem root.
func resolveIncludePath(dir, include string) (string, error) {
	if include == "" {
		return "", fmt.Errorf("include path must not be empty")
	}
	if path.IsAbs(include) {
		return "", fmt.Errorf("include path %q escapes the config filesystem root", include)
	}
	sub := path.Join(dir, filepath.ToSlash(include))
	if sub == ".." || strings.HasPrefix(sub, "../") {
		return "", fmt.Errorf("include path %q escapes the config filesystem root", include)
	}
	return sub, nil
}

func cleanPath(name string) string {
	return path.Clean(filepath.ToSlash(name))
}
