package runtime

import "reflect"

// VariantInfo describes a concrete variant payload type used by the OpVariant
// opcode. A value matches when its dynamic type (after dereferencing) equals
// the payload type; pointer payloads are accepted as well.
type VariantInfo struct {
	Type    reflect.Type // Concrete value payload type.
	PtrType reflect.Type // Pointer-to-payload type.
	Name    string
}

// IsVariant reports whether value matches the variant described by info.
func IsVariant(value any, info *VariantInfo) bool {
	if value == nil || info == nil {
		return false
	}
	t := reflect.TypeOf(value)
	if t == info.Type || t == info.PtrType {
		return true
	}
	return false
}
