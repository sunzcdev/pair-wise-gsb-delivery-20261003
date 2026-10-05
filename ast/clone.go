package ast

import (
	"reflect"
	"unsafe"

	"github.com/yuin/goldmark/v2/text"
)

var nodeType = reflect.TypeFor[Node]()

// CloneNode returns a deep, independent copy of n and its descendants.
// Source positions in the copy still refer to the same immutable source byte
// slice used by n; callers must keep that source alive and must not modify it.
func CloneNode(n Node) Node {
	if n == nil {
		panic("node cannot be nil")
	}

	value := reflect.ValueOf(n).Elem()
	cloneValue := reflect.New(value.Type()).Elem()
	cloneValue.Set(value)
	clone := cloneValue.Addr().Interface().(Node)

	resetNodeLinks(cloneValue)
	setNodeSelf(cloneValue, reflect.ValueOf(clone))
	cloneNodeData(cloneValue)

	if block, ok := clone.(BlockNode); ok {
		block.SetSource(cloneSegments(block.Source()))
	}

	if attrs := n.Attributes(); attrs != nil {
		clonedAttrs := make([]Attribute, len(attrs))
		for i, attr := range attrs {
			clonedAttrs[i] = Attribute{
				Name:  attr.Name,
				Value: text.CloneMultiLineValue(attr.Value),
			}
		}
		clone.RemoveAttributes()
		for _, attr := range clonedAttrs {
			clone.SetAttribute(attr.Name, attr.Value)
		}
	}

	for child := range n.Children() {
		clone.AppendChild(CloneNode(child))
	}

	return clone
}

func cloneSegments(segments []text.Segment) []text.Segment {
	if segments == nil {
		return nil
	}
	cloned := make([]text.Segment, len(segments))
	copy(cloned, segments)
	return cloned
}

func resetNodeLinks(value reflect.Value) {
	if value.Kind() != reflect.Struct {
		return
	}

	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldValue := value.Field(i)
		if field.Anonymous && fieldValue.Kind() == reflect.Struct {
			resetNodeLinks(fieldValue)
			continue
		}

		switch field.Name {
		case "parent", "prev", "next", "self", "firstChild", "lastChild", "attributes":
			setReflectValue(fieldValue, reflect.Zero(fieldValue.Type()))
		}
	}
}

func setNodeSelf(value, self reflect.Value) {
	if value.Kind() != reflect.Struct {
		return
	}

	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldValue := value.Field(i)
		if field.Anonymous && fieldValue.Kind() == reflect.Struct {
			setNodeSelf(fieldValue, self)
			continue
		}
		if field.Name == "self" {
			setReflectValue(fieldValue, self)
		}
	}
}

func cloneNodeData(value reflect.Value) {
	if value.Kind() != reflect.Struct {
		return
	}

	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldValue := value.Field(i)
		if field.Anonymous && fieldValue.Kind() == reflect.Struct {
			cloneNodeData(fieldValue)
			continue
		}
		if !fieldValue.CanSet() {
			continue
		}

		switch field.Type {
		case reflect.TypeFor[text.SingleLineValue]():
			fieldValue.Set(reflect.ValueOf(text.CloneSingleLineValue(fieldValue.Interface().(text.SingleLineValue))))
		case reflect.TypeFor[text.MultiLineValue]():
			fieldValue.Set(reflect.ValueOf(text.CloneMultiLineValue(fieldValue.Interface().(text.MultiLineValue))))
		case reflect.TypeFor[text.Lines]():
			fieldValue.Set(reflect.ValueOf(text.CloneLines(fieldValue.Interface().(text.Lines))))
		default:
			cloneValueData(fieldValue)
		}
	}
}

func cloneValueData(value reflect.Value) {
	if !value.CanSet() {
		return
	}

	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return
		}
		if value.Type().Implements(nodeType) {
			value.Set(reflect.ValueOf(CloneNode(value.Interface().(Node))))
			return
		}
		if node, ok := value.Interface().(Node); ok {
			value.Set(reflect.ValueOf(CloneNode(node)))
			return
		}
		if value.Elem().Kind() != reflect.Struct {
			return
		}
		cloned := reflect.New(value.Elem().Type())
		cloned.Elem().Set(value.Elem())
		cloneNodeData(cloned.Elem())
		value.Set(cloned)
	case reflect.Slice:
		if value.IsNil() {
			return
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(value.Index(i))
			cloneValueData(cloned.Index(i))
		}
		value.Set(cloned)
	case reflect.Map:
		if value.IsNil() {
			return
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		for _, key := range value.MapKeys() {
			mapValue := value.MapIndex(key)
			copied := reflect.New(mapValue.Type()).Elem()
			copied.Set(mapValue)
			cloneValueData(copied)
			cloned.SetMapIndex(key, copied)
		}
		value.Set(cloned)
	case reflect.Interface:
		if value.IsNil() {
			return
		}
		if node, ok := value.Interface().(Node); ok {
			value.Set(reflect.ValueOf(CloneNode(node)))
		}
	case reflect.Struct:
		cloneNodeData(value)
	}
}

func setReflectValue(value, newValue reflect.Value) {
	if value.CanSet() {
		value.Set(newValue)
		return
	}
	reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem().Set(newValue)
}
