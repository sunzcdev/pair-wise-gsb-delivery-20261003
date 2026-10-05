package section

import (
	"maps"
	"reflect"
	"unsafe"

	"github.com/yuin/goldmark/v2/ast"
	extast "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/text"
)

func copyDocumentMetadata(result *ast.Document, original *ast.Document) {
	field := reflect.ValueOf(original).Elem().FieldByName("metadata")
	metadataPointer := (*map[string]any)(unsafe.Pointer(field.UnsafeAddr()))
	if metadata := maps.Clone(*metadataPointer); metadata != nil {
		result.SetMetadata(metadata)
	}
}

func cloneNode(original ast.Node) ast.Node {
	value := reflect.ValueOf(original).Elem()
	clonePointer := reflect.New(value.Type())
	cloneValue := clonePointer.Elem()
	cloneValue.Set(value)
	clone := clonePointer.Interface().(ast.Node)

	resetNodeLinks(cloneValue, clone)

	for child := original.FirstChild(); child != nil; child = child.NextSibling() {
		if !isDefinition(child) {
			clone.AppendChild(cloneNode(child))
		}
	}

	copyAttributes(original, clone)
	copyNodeState(original, clone)
	return clone
}

func resetNodeLinks(value reflect.Value, owner ast.Node) {
	if value.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if field.Kind() != reflect.Struct {
			continue
		}

		switch value.Type().Field(i).Name {
		case "node":
			clearNode(field)
		case "BaseNode":
			clearBaseNode(field, owner)
		case "BaseBlock", "BaseInline", "terminalBlockNode":
			resetNodeLinks(field, owner)
		}
	}
}

func clearBaseNode(value reflect.Value, owner ast.Node) {
	clearNode(value.Field(0))
	setNodeField(value.Field(1), owner)
	setNodeField(value.Field(2), nil)
	setNodeField(value.Field(3), nil)
}

func clearNode(value reflect.Value) {
	setNodeField(value.Field(0), nil)
	setNodeField(value.Field(1), nil)
	setNodeField(value.Field(2), nil)
	setAttributePointerField(value.Field(3), nil)
}

func setNodeField(target reflect.Value, node ast.Node) {
	pointer := unsafe.Pointer(target.UnsafeAddr())
	field := reflect.NewAt(reflect.TypeOf((*ast.Node)(nil)).Elem(), pointer).Elem()
	if node == nil {
		field.Set(reflect.Zero(field.Type()))
	} else {
		field.Set(reflect.ValueOf(node))
	}
}

func setUnexported(target reflect.Value, value any) {
	pointer := unsafe.Pointer(target.UnsafeAddr())
	targetType := reflect.TypeOf(&value).Elem()
	field := reflect.NewAt(targetType, pointer).Elem()
	if value == nil {
		field.Set(reflect.Zero(targetType))
	} else {
		field.Set(reflect.ValueOf(value))
	}
}

func setAttributePointerField(target reflect.Value, value *[]ast.Attribute) {
	pointer := unsafe.Pointer(target.UnsafeAddr())
	field := reflect.NewAt(reflect.TypeOf((**[]ast.Attribute)(nil)).Elem(), pointer).Elem()
	if value == nil {
		field.Set(reflect.Zero(field.Type()))
	} else {
		field.Set(reflect.ValueOf(value))
	}
}

func copyAttributes(original ast.Node, clone ast.Node) {
	attributes := original.Attributes()
	for _, attribute := range attributes {
		clone.SetAttribute(attribute.Name, copyMultiLineValue(attribute.Value))
	}
}

func copyNodeState(original ast.Node, clone ast.Node) {
	switch node := clone.(type) {
	case *ast.CodeBlock:
		node.Value = copyLines(node.Value)
	case *ast.HTMLBlock:
		node.Value = copyLines(node.Value)
	case *ast.CodeSpan:
		node.Value = copyTextValue(original.(*ast.CodeSpan).Value)
	case *ast.Link:
		copyLinkState(original.(*ast.Link), node)
	case *ast.Image:
		copyImageState(original.(*ast.Image), node)
	case *ast.RawHTML:
		node.Value = copyMultiLineValue(node.Value)
	case *ast.LinkReferenceDefinition:
		node.Label = copyMultiLineValue(node.Label)
		node.Title = copyMultiLineValue(node.Title)
	case *extast.DefinitionList:
		node.SetTemporaryParagraph(nil)
	}

	if source, ok := original.(ast.BlockNode); ok {
		if segments := source.Source(); len(segments) > 0 {
			clone.(ast.BlockNode).SetSource(append([]text.Segment(nil), segments...))
		}
	}
}

func copyLinkState(original *ast.Link, clone *ast.Link) {
	clone.Title = copyMultiLineValue(original.Title)
	if original.Reference != nil {
		reference := *original.Reference
		reference.Value = copyMultiLineValue(reference.Value)
		clone.Reference = &reference
	}
}

func copyImageState(original *ast.Image, clone *ast.Image) {
	clone.Title = copyMultiLineValue(original.Title)
	if original.Reference != nil {
		reference := *original.Reference
		reference.Value = copyMultiLineValue(reference.Value)
		clone.Reference = &reference
	}
}

func copyTextValue(value text.Value) text.Value {
	switch value := value.(type) {
	case text.MultiLineValue:
		return copyMultiLineValue(value)
	default:
		original := reflect.ValueOf(value)
		if original.Kind() == reflect.Pointer {
			clone := reflect.New(original.Elem().Type())
			clone.Elem().Set(original.Elem())
			copyMutableSlices(clone.Elem())
			return clone.Interface().(text.Value)
		}
		if original.Kind() == reflect.Struct {
			clone := reflect.New(original.Type()).Elem()
			clone.Set(original)
			copyMutableSlices(clone)
			return clone.Interface().(text.Value)
		}
		return value
	}
}

func copyMutableSlices(value reflect.Value) {
	if value.Kind() == reflect.Pointer {
		if !value.IsNil() {
			copyMutableSlices(value.Elem())
		}
		return
	}
	if value.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		switch field.Kind() {
		case reflect.Slice:
			if !field.IsNil() {
				clone := reflect.MakeSlice(field.Type(), field.Len(), field.Cap())
				reflect.Copy(clone, field)
				setUnexported(field, clone.Interface())
			}
		case reflect.Struct:
			copyMutableSlices(field)
		case reflect.Pointer:
			copyMutableSlices(field)
		}
	}
}

func copyMultiLineValue(value text.MultiLineValue) text.MultiLineValue {
	clone := value
	indices := value.Indices()
	if indices != nil {
		copied := append([]text.Index(nil), indices...)
		field := reflect.ValueOf(&clone).Elem().FieldByName("indices")
		setUnexported(field, copied)
	}
	return clone
}

func copyLines(value text.Lines) text.Lines {
	clone := value
	segments := value.Segments()
	if segments != nil {
		copied := append([]text.Segment(nil), segments...)
		field := reflect.ValueOf(&clone).Elem().FieldByName("segs")
		setUnexported(field, copied)
	}
	return clone
}
