package gojq

import (
	"fmt"
	"path"
	"strings"
)

// errFSEscape is returned when a logical path resolves outside the root of
// the file system.
var errFSEscape = fmt.Errorf("path escapes the file system root")

// validateLogicalName rejects names that are syntactically invalid for a
// slash-separated logical path: empty, absolute, or containing a backslash.
// Whether a ".." segment leaves the file system root is decided while the
// name is resolved against its base directory.
func validateLogicalName(name string) error {
	if name == "" {
		return fmt.Errorf("invalid module name: %q", name)
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return fmt.Errorf("absolute module path is not allowed: %q", name)
	}
	if strings.ContainsRune(name, '\\') {
		return fmt.Errorf("backslashes are not allowed in module path: %q", name)
	}
	return nil
}

// isValidLogicalPath reports whether a cleaned path stays within the root.
func isValidLogicalPath(p string) bool {
	_, err := joinLogicalPath("", p)
	return err == nil
}

// cleanLogicalPath normalizes a slash-separated path. The root is reported as
// "." (matching [path.Clean]); ".." at the root is collapsed into ".".
func cleanLogicalPath(p string) string {
	if p == "" {
		return "."
	}
	return path.Clean(p)
}

// joinLogicalPath joins a base directory (empty means the file system root)
// with a slash-separated target and verifies that the result never escapes
// the root. It returns the cleaned logical path without a leading slash.
func joinLogicalPath(base, target string) (string, error) {
	if strings.ContainsRune(target, '\\') || strings.ContainsRune(base, '\\') {
		return "", fmt.Errorf("backslashes are not allowed in module path: %q", target)
	}
	if strings.HasPrefix(target, "/") {
		return "", fmt.Errorf("absolute module path is not allowed: %q", target)
	}
	var stack []string
	push := func(segment string) error {
		switch segment {
		case "", ".":
		case "..":
			if len(stack) == 0 {
				return fmt.Errorf("%w: %q", errFSEscape, target)
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, segment)
		}
		return nil
	}
	for _, seg := range strings.Split(base, "/") {
		if err := push(seg); err != nil {
			return "", err
		}
	}
	for _, seg := range strings.Split(target, "/") {
		if err := push(seg); err != nil {
			return "", err
		}
	}
	return strings.Join(stack, "/"), nil
}
