package mapstructure

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/go-viper/mapstructure/v2/internal/errors"
)

// fieldMaskNode is a node in the trie compiled from DecoderConfig.FieldMask.
// The keys of children are canonical field names (the tag value when set,
// otherwise the field name passed through MapFieldName). A node with subtree
// set to true selects everything below it and has no children.
type fieldMaskNode struct {
	subtree  bool
	children map[string]*fieldMaskNode
}

func (n *fieldMaskNode) child(name string) *fieldMaskNode {
	if n.children == nil {
		n.children = make(map[string]*fieldMaskNode)
	}
	child, ok := n.children[name]
	if !ok {
		child = &fieldMaskNode{}
		n.children[name] = child
	}
	return child
}

// nodeFor returns the mask node governing the struct at the given canonical
// dot-separated decoder path (e.g. "address" or "servers[0]"). Ancestors
// selected as a whole (subtree) cover all their descendants, including
// struct values nested in slices, arrays and maps. It returns nil if the
// path is not covered by the mask at all.
func (n *fieldMaskNode) nodeFor(path string) *fieldMaskNode {
	node := n
	if path == "" {
		return node
	}
	for _, segment := range strings.Split(path, ".") {
		if node.subtree {
			return node
		}
		// Strip slice/array/map index suffixes ("items[0]" -> "items").
		if i := strings.IndexByte(segment, '['); i >= 0 {
			segment = segment[:i]
		}
		next, ok := node.children[segment]
		if !ok {
			return nil
		}
		node = next
	}
	return node
}

// allows reports whether the field with the given canonical name may be
// decoded at this node.
func (n *fieldMaskNode) allows(fieldName string) bool {
	if n.subtree {
		return true
	}
	_, ok := n.children[fieldName]
	return ok
}

// compileFieldMask validates the given mask paths against the result type
// and compiles them into a trie. Every invalid path is reported.
func compileFieldMask(config *DecoderConfig, paths []string, resultType reflect.Type) (*fieldMaskNode, error) {
	root := &fieldMaskNode{}

	var errs []error
	for _, path := range paths {
		if err := insertMaskPath(config, root, resultType, path); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return root, nil
}

// insertMaskPath resolves a single mask path against the given type and
// records it in the trie rooted at root.
func insertMaskPath(config *DecoderConfig, root *fieldMaskNode, resultType reflect.Type, path string) error {
	segments := strings.Split(path, ".")

	node := root
	typ := resultType
	for i, segment := range segments {
		if segment == "" {
			return fmt.Errorf("invalid field mask path %q: empty path segment", path)
		}
		last := i == len(segments)-1

		canonical, transparent, fieldType, err := resolveMaskSegment(config, typ, segment)
		if err != nil {
			return fmt.Errorf("invalid field mask path %q: %w", path, err)
		}

		if transparent {
			// A squashed field addressed by its own name contributes no
			// path segment of its own: its fields are promoted to the
			// parent level.
			if last {
				expandMaskSubtree(config, node, fieldType)
				return nil
			}
			typ = fieldType
			continue
		}

		node = node.child(canonical)
		if last {
			node.subtree = true
			return nil
		}

		if derefMaskType(fieldType).Kind() != reflect.Struct {
			return fmt.Errorf(
				"invalid field mask path %q: cannot select into field %q of type %s",
				path, segment, fieldType)
		}
		typ = fieldType
	}

	return nil
}

// resolveMaskSegment finds the field of typ (a struct, or a pointer to one)
// that the given mask path segment addresses. It returns the canonical name
// of the field (the trie key) and the field type. If the segment addresses
// a squashed field by its own name, transparent is true and the canonical
// name is empty: the field contributes no path segment of its own.
func resolveMaskSegment(config *DecoderConfig, typ reflect.Type, segment string) (canonical string, transparent bool, fieldType reflect.Type, err error) {
	typ = derefMaskType(typ)
	if typ.Kind() != reflect.Struct {
		return "", false, nil, fmt.Errorf("cannot select field %q of non-struct type %s", segment, typ)
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		tagValue, _ := getTagValue(field, config.TagName)
		if tagValue == "" && config.IgnoreUntaggedFields {
			continue
		}

		squash, _ := isSquashedField(config, field, tagValue)
		if field.PkgPath != "" && !squash {
			// Unexported fields are never decoded, unless they are
			// squashed embedded structs promoting exported fields.
			continue
		}

		name := strings.SplitN(tagValue, ",", 2)[0]
		if name == "" {
			name = config.MapFieldName(field.Name)
		}

		if squash {
			if derefMaskType(field.Type).Kind() != reflect.Struct {
				// Decoding would reject this field; don't match it.
				continue
			}
			if config.MatchName(segment, name) {
				return "", true, field.Type, nil
			}
			// Squashed fields promote their own fields to this level.
			if canonical, transparent, fieldType, err := resolveMaskSegment(config, field.Type, segment); err == nil {
				return canonical, transparent, fieldType, nil
			}
			continue
		}

		if config.MatchName(segment, name) {
			return name, false, field.Type, nil
		}
	}

	return "", false, nil, fmt.Errorf("no field matching %q in %s", segment, typ)
}

// expandMaskSubtree marks every field promoted from the (squashed) struct
// typ as fully selected at the given node.
func expandMaskSubtree(config *DecoderConfig, node *fieldMaskNode, typ reflect.Type) {
	typ = derefMaskType(typ)
	if typ.Kind() != reflect.Struct {
		node.subtree = true
		return
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		tagValue, _ := getTagValue(field, config.TagName)
		if tagValue == "" && config.IgnoreUntaggedFields {
			continue
		}

		squash, _ := isSquashedField(config, field, tagValue)
		if field.PkgPath != "" && !squash {
			continue
		}

		if squash {
			if derefMaskType(field.Type).Kind() == reflect.Struct {
				expandMaskSubtree(config, node, field.Type)
			}
			continue
		}

		name := strings.SplitN(tagValue, ",", 2)[0]
		if name == "" {
			name = config.MapFieldName(field.Name)
		}
		node.child(name).subtree = true
	}
}

// maskSelectsSquashed reports whether the mask node selects any of the
// fields promoted from the squashed struct typ.
func maskSelectsSquashed(config *DecoderConfig, node *fieldMaskNode, typ reflect.Type) bool {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		tagValue, _ := getTagValue(field, config.TagName)
		if tagValue == "" && config.IgnoreUntaggedFields {
			continue
		}

		squash, _ := isSquashedField(config, field, tagValue)
		if field.PkgPath != "" && !squash {
			continue
		}

		if squash {
			fieldType := derefMaskType(field.Type)
			if fieldType.Kind() == reflect.Struct && maskSelectsSquashed(config, node, fieldType) {
				return true
			}
			continue
		}

		name := strings.SplitN(tagValue, ",", 2)[0]
		if name == "" {
			name = config.MapFieldName(field.Name)
		}
		if node.allows(name) {
			return true
		}
	}

	return false
}

// isSquashedField mirrors the squash detection of decodeStructFromMap at
// the type level. The second return value reports whether the field carries
// the "remain" tag option.
func isSquashedField(config *DecoderConfig, field reflect.StructField, tagValue string) (squash, remain bool) {
	fieldType := field.Type
	if fieldType.Kind() == reflect.Ptr && fieldType.Elem().Kind() == reflect.Struct {
		fieldType = fieldType.Elem()
	}
	squash = config.Squash && fieldType.Kind() == reflect.Struct && field.Anonymous

	for _, opt := range strings.Split(tagValue, ",")[1:] {
		if opt == config.SquashTagOption {
			squash = true
		}
		if opt == "remain" {
			remain = true
		}
	}

	return squash, remain
}

// derefMaskType dereferences pointer types until a non-pointer type remains.
func derefMaskType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ
}

// maskStructPath converts a decoder field path (as built during decoding,
// prefixed with RootName) into a mask-relative path.
func (d *Decoder) maskStructPath(name string) string {
	if d.config.RootName == "" {
		return name
	}
	return strings.TrimPrefix(strings.TrimPrefix(name, d.config.RootName), ".")
}
