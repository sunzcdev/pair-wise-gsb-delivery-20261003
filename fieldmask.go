package mapstructure

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/go-viper/mapstructure/v2/internal/errors"
)

// fieldMaskNode is a node in the compiled field mask tree. A node with
// subtree set to true selects everything below it (children are then
// irrelevant). Otherwise only the listed children are selected.
type fieldMaskNode struct {
	subtree  bool
	children map[string]*fieldMaskNode
}

// buildFieldMask compiles a list of dot-separated field paths into a
// mask tree. It returns nil (no filtering) when paths is empty.
func buildFieldMask(paths []string) (*fieldMaskNode, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	root := &fieldMaskNode{children: make(map[string]*fieldMaskNode)}
	for _, path := range paths {
		segments := strings.Split(path, ".")
		node := root
		for _, segment := range segments {
			if segment == "" {
				return nil, fmt.Errorf("field mask: invalid path %q: empty path segment", path)
			}
			if node.subtree {
				// A parent path already covers this whole subtree.
				break
			}
			child, ok := node.children[segment]
			if !ok {
				child = &fieldMaskNode{children: make(map[string]*fieldMaskNode)}
				node.children[segment] = child
			}
			node = child
		}
		node.subtree = true
	}

	return root, nil
}

// validateFieldMask checks every path in the mask against the target
// type and reports all invalid paths. It runs before any input is
// processed so that typos in the mask surface immediately.
func (d *Decoder) validateFieldMask() error {
	typ := reflect.TypeOf(d.config.Result)
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return fmt.Errorf("field mask: result must be a struct, got %s", typ)
	}

	var errs []error
	d.validateMaskNode(typ, d.fieldMask, "", &errs)

	return errors.Join(errs...)
}

// validateMaskNode validates the children of node against the struct
// type typ, appending errors for invalid paths to errs. path holds the
// dot-joined prefix of the current node for error messages.
func (d *Decoder) validateMaskNode(typ reflect.Type, node *fieldMaskNode, path string, errs *[]error) {
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	if typ.Kind() != reflect.Struct || d.isMaskLeaf(typ) {
		// Cannot descend into this type: any child path is invalid.
		for _, p := range maskPaths(node, path) {
			*errs = append(*errs, fmt.Errorf(
				"field mask: invalid path %q: cannot select fields of type %s", p, typ))
		}
		return
	}

	fields := d.maskFields(typ)

	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		child := node.children[name]
		childPath := name
		if path != "" {
			childPath = path + "." + name
		}

		fieldType, ok := fields[name]
		if !ok {
			*errs = append(*errs, fmt.Errorf(
				"field mask: invalid path %q: %s has no field %q", childPath, typ, name))
			continue
		}

		if child.subtree {
			// Selecting the field itself covers its whole subtree.
			continue
		}

		d.validateMaskNode(fieldType, child, childPath, errs)
	}
}

// isMaskLeaf reports whether values of the given struct type are decoded
// as a whole, making paths below the type meaningless. That is the case
// for types implementing Unmarshaler (unless disabled), since decoding
// is delegated to the type itself.
func (d *Decoder) isMaskLeaf(typ reflect.Type) bool {
	if d.config.DisableUnmarshaler {
		return false
	}

	unmarshalerType := reflect.TypeOf((*Unmarshaler)(nil)).Elem()
	return typ.Implements(unmarshalerType) || reflect.PtrTo(typ).Implements(unmarshalerType)
}

// maskFields returns the maskable fields of the given struct type as a
// map from the decoder-visible field name to the field type. Squashed
// (embedded) structs contribute their fields at this level, mirroring
// the decoding behavior.
func (d *Decoder) maskFields(typ reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	d.collectMaskFields(typ, fields)
	return fields
}

func (d *Decoder) collectMaskFields(typ reflect.Type, fields map[string]reflect.Type) {
	for i := 0; i < typ.NumField(); i++ {
		fieldType := typ.Field(i)

		// Unexported fields cannot be set by the decoder.
		if fieldType.PkgPath != "" {
			continue
		}

		ft := fieldType.Type
		squashable := ft.Kind() == reflect.Struct ||
			(ft.Kind() == reflect.Ptr && ft.Elem().Kind() == reflect.Struct)

		squash := d.config.Squash && squashable && fieldType.Anonymous
		remain := false

		tagParts := getTagParts(fieldType, d.config.TagName)
		if len(tagParts) == 0 {
			tagParts = []string{""}
		}
		for _, tag := range tagParts[1:] {
			if tag == d.config.SquashTagOption {
				squash = true
			}
			if tag == "remain" {
				remain = true
			}
		}

		if squash {
			if !squashable {
				// The decoder errors on squashing non-struct types;
				// such a field can never be decoded.
				continue
			}
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			d.collectMaskFields(ft, fields)
			continue
		}

		// Remain fields catch unused keys and cannot be selected by name.
		if remain {
			continue
		}

		tagValue := tagParts[0]
		if tagValue == "" && d.config.IgnoreUntaggedFields {
			continue
		}

		// Fields tagged with "-" are skipped by the decoder.
		if tagValue == "-" {
			continue
		}

		fieldName := tagValue
		if fieldName == "" {
			fieldName = d.config.MapFieldName(fieldType.Name)
		}

		fields[fieldName] = fieldType.Type
	}
}

// maskPaths returns the full dot-joined paths of all leaves selected by
// node, prefixed with path.
func maskPaths(node *fieldMaskNode, path string) []string {
	var paths []string
	for name, child := range node.children {
		childPath := name
		if path != "" {
			childPath = path + "." + name
		}
		if child.subtree || len(child.children) == 0 {
			paths = append(paths, childPath)
			continue
		}
		paths = append(paths, maskPaths(child, childPath)...)
	}
	sort.Strings(paths)
	return paths
}
