package importmanifest

import (
	"fmt"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// Short-form value type bytes.
const (
	valI32       byte = 0x7f
	valI64       byte = 0x7e
	valF32       byte = 0x7d
	valF64       byte = 0x7c
	valV128      byte = 0x7b
	valFuncref   byte = 0x70
	valExternref byte = 0x6f
	valExnref    byte = 0x69

	refNullable    byte = 0x63
	refNonNullable byte = 0x64

	tableWithInit byte = 0x40
)

// Abstract heap type indices (signed LEB33).
const (
	heapFunc   int64 = -0x10
	heapExtern int64 = -0x11
	heapExn    int64 = -0x17
)

func decodeTypeSection(c *cursor, features api.CoreFeatures) ([]FunctionSignature, error) {
	count, err := c.u32()
	if err != nil {
		return nil, fmt.Errorf("%w: type count: %v", ErrInvalidBinary, err)
	}
	types := make([]FunctionSignature, 0, count)
	for i := uint32(0); i < count; i++ {
		lead, err := c.byte()
		if err != nil {
			return nil, fmt.Errorf("%w: type[%d] marker: %v", ErrInvalidBinary, i, err)
		}
		if lead != 0x60 {
			return nil, fmt.Errorf("%w: type[%d]: expected functype marker 0x60, got %#x",
				ErrInvalidBinary, i, lead)
		}

		paramCount, err := c.u32()
		if err != nil {
			return nil, fmt.Errorf("%w: type[%d] param count: %v", ErrInvalidBinary, i, err)
		}
		params := make([]api.ValueType, 0, paramCount)
		for j := uint32(0); j < paramCount; j++ {
			vt, err := decodeValType(c, features)
			if err != nil {
				return nil, fmt.Errorf("type[%d] param[%d]: %w", i, j, err)
			}
			params = append(params, vt)
		}

		resultCount, err := c.u32()
		if err != nil {
			return nil, fmt.Errorf("%w: type[%d] result count: %v", ErrInvalidBinary, i, err)
		}
		if resultCount > 1 {
			if err := require(features, api.CoreFeatureMultiValue, "multi-value"); err != nil {
				return nil, fmt.Errorf("type[%d] has %d results: %w", i, resultCount, err)
			}
		}
		results := make([]api.ValueType, 0, resultCount)
		for j := uint32(0); j < resultCount; j++ {
			vt, err := decodeValType(c, features)
			if err != nil {
				return nil, fmt.Errorf("type[%d] result[%d]: %w", i, j, err)
			}
			results = append(results, vt)
		}

		types = append(types, FunctionSignature{Params: params, Results: results})
	}
	if c.remaining() != 0 {
		return nil, fmt.Errorf("%w: %d trailing byte(s) in type section", ErrInvalidBinary, c.remaining())
	}
	return types, nil
}

func decodeImportSection(c *cursor, types []FunctionSignature, features api.CoreFeatures) ([]Import, error) {
	count, err := c.u32()
	if err != nil {
		return nil, fmt.Errorf("%w: import count: %v", ErrInvalidBinary, err)
	}

	imports := make([]Import, 0, count)
	var perKind [4]uint32
	for i := uint32(0); i < count; i++ {
		imp := Import{Index: i}

		if imp.Module, err = c.name("module", i); err != nil {
			return nil, err
		}
		if imp.Name, err = c.name("member", i); err != nil {
			return nil, err
		}

		kind, err := c.byte()
		if err != nil {
			return nil, fmt.Errorf("%w: import[%d] kind: %v", ErrInvalidBinary, i, err)
		}

		switch kind {
		case KindFunc:
			imp.Kind = KindFunc
			typeIdx, err := c.u32()
			if err != nil {
				return nil, fmt.Errorf("%w: import[%d] func type index: %v", ErrInvalidBinary, i, err)
			}
			if int(typeIdx) >= len(types) {
				return nil, fmt.Errorf("%w: import[%d] %s.%s: function type index %d out of range (type section has %d)",
					ErrInvalidBinary, i, imp.Module, imp.Name, typeIdx, len(types))
			}
			imp.FuncType = typeIdx
			sig := types[typeIdx]
			imp.Signature = &sig
		case KindTable:
			imp.Kind = KindTable
			tab, err := decodeTableType(c, features)
			if err != nil {
				return nil, wrapImport(i, imp.Module, imp.Name, "table", err)
			}
			imp.Table = tab
		case KindMemory:
			imp.Kind = KindMemory
			mem, err := decodeLimits(c)
			if err != nil {
				return nil, wrapImport(i, imp.Module, imp.Name, "memory", err)
			}
			if mem.Shared {
				if err := require(features, experimental.CoreFeaturesThreads, "threads"); err != nil {
					return nil, fmt.Errorf("import[%d] %s.%s shared memory: %w", i, imp.Module, imp.Name, err)
				}
				if mem.Max == nil {
					return nil, fmt.Errorf("%w: import[%d] %s.%s shared memory must declare a maximum",
						ErrInvalidBinary, i, imp.Module, imp.Name)
				}
			}
			imp.Memory = mem
		case KindGlobal:
			imp.Kind = KindGlobal
			gt, err := decodeGlobalType(c, features)
			if err != nil {
				return nil, wrapImport(i, imp.Module, imp.Name, "global", err)
			}
			if gt.Mutable {
				if err := require(features, api.CoreFeatureMutableGlobal, "mutable-global"); err != nil {
					return nil, fmt.Errorf("import[%d] %s.%s mutable global: %w", i, imp.Module, imp.Name, err)
				}
			}
			imp.GlobalType = gt
		case externTypeTag:
			// Tags are an exception-handling construct and are not part of the
			// func/table/memory/global capability model, so reject explicitly
			// rather than silently ignoring a requested capability.
			if err := require(features, experimental.CoreFeaturesExceptionHandling, "exception-handling"); err != nil {
				return nil, fmt.Errorf("import[%d] %s.%s tag: %w", i, imp.Module, imp.Name, err)
			}
			return nil, fmt.Errorf("%w: import[%d] %s.%s: tag imports are not supported by the import manifest",
				ErrInvalidBinary, i, imp.Module, imp.Name)
		default:
			return nil, fmt.Errorf("%w: import[%d] %s.%s: unknown import kind %#x",
				ErrInvalidBinary, i, imp.Module, imp.Name, kind)
		}

		imp.IndexPerKind = perKind[kind]
		perKind[kind]++
		imports = append(imports, imp)
	}

	if c.remaining() != 0 {
		return nil, fmt.Errorf("%w: %d trailing byte(s) in import section", ErrInvalidBinary, c.remaining())
	}
	return imports, nil
}

func decodeGlobalType(c *cursor, features api.CoreFeatures) (*GlobalType, error) {
	vt, err := decodeValType(c, features)
	if err != nil {
		return nil, err
	}
	mut, err := c.byte()
	if err != nil {
		return nil, fmt.Errorf("read mutability: %w", err)
	}
	switch mut {
	case globalImmutable:
		return &GlobalType{ValueType: vt, Mutable: false}, nil
	case globalMutable:
		return &GlobalType{ValueType: vt, Mutable: true}, nil
	default:
		return nil, fmt.Errorf("%w: invalid mutability byte %#x", ErrInvalidBinary, mut)
	}
}

func decodeTableType(c *cursor, features api.CoreFeatures) (*Table, error) {
	b, err := c.byte()
	if err != nil {
		return nil, fmt.Errorf("read element type: %w", err)
	}
	if b == tableWithInit {
		// An init-expression table only exists in the table (definition)
		// section; an import must carry a plain tabletype.
		return nil, fmt.Errorf("%w: table import must not declare an init expression", ErrInvalidBinary)
	}

	var elem api.ValueType
	switch b {
	case refNullable, refNonNullable:
		elem, err = decodeRefHeapType(c, features, b == refNullable)
		if err != nil {
			return nil, err
		}
	default:
		elem = api.ValueType(b)
	}

	if elem != api.ValueType(valFuncref) {
		if err := require(features, api.CoreFeatureReferenceTypes, "reference-types"); err != nil {
			return nil, fmt.Errorf("table element %s: %w", api.ValueTypeName(elem), err)
		}
	}

	lim, err := decodeLimits(c)
	if err != nil {
		return nil, err
	}
	return &Table{ElementType: elem, Limits: *lim}, nil
}

// decodeValType reads one value type used in function signatures and globals.
func decodeValType(c *cursor, features api.CoreFeatures) (api.ValueType, error) {
	b, err := c.byte()
	if err != nil {
		return 0, fmt.Errorf("read value type: %w", err)
	}
	switch b {
	case valI32, valI64, valF32, valF64, valFuncref:
		return api.ValueType(b), nil
	case valV128:
		if err := require(features, api.CoreFeatureSIMD, "simd"); err != nil {
			return 0, err
		}
		return api.ValueType(b), nil
	case valExternref:
		if err := require(features, api.CoreFeatureReferenceTypes, "reference-types"); err != nil {
			return 0, err
		}
		return api.ValueType(b), nil
	case valExnref:
		if err := require(features, experimental.CoreFeaturesExceptionHandling, "exception-handling"); err != nil {
			return 0, err
		}
		return api.ValueType(b), nil
	case refNullable, refNonNullable:
		return decodeRefHeapType(c, features, b == refNullable)
	default:
		return 0, fmt.Errorf("%w: unknown value type %#x", ErrInvalidBinary, b)
	}
}

// decodeRefHeapType reads the heap type following a 0x63/0x64 reference prefix
// and reduces abstract nullable references to their short value-type form.
func decodeRefHeapType(c *cursor, features api.CoreFeatures, nullable bool) (api.ValueType, error) {
	if !nullable {
		if err := require(features, experimental.CoreFeaturesTypedFunctionReferences, "typed-function-references"); err != nil {
			return 0, err
		}
		// The byte-based manifest cannot carry nullability, so do not pretend a
		// non-nullable reference matched the nullable short form.
		return 0, fmt.Errorf("%w: non-nullable reference types are not supported in the import manifest",
			ErrInvalidBinary)
	}

	heap, err := c.s33()
	if err != nil {
		return 0, fmt.Errorf("read heap type: %w", err)
	}
	switch heap {
	case heapFunc:
		return api.ValueType(valFuncref), nil
	case heapExtern:
		if err := require(features, api.CoreFeatureReferenceTypes, "reference-types"); err != nil {
			return 0, err
		}
		return api.ValueTypeExternref, nil
	case heapExn:
		if err := require(features, experimental.CoreFeaturesExceptionHandling, "exception-handling"); err != nil {
			return 0, err
		}
		return 0x69, nil
	default:
		if heap < 0 {
			return 0, fmt.Errorf("%w: unknown abstract heap type %d", ErrInvalidBinary, heap)
		}
		if err := require(features, experimental.CoreFeaturesTypedFunctionReferences, "typed-function-references"); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w: concrete reference types are not supported in the import manifest",
			ErrInvalidBinary)
	}
}

func decodeLimits(c *cursor) (*Limits, error) {
	flag, err := c.byte()
	if err != nil {
		return nil, fmt.Errorf("read limits flag: %w", err)
	}

	var shared bool
	switch flag {
	case limitsFlagNoMax, limitsFlagHasMax:
	case limitsFlagShared, limitsFlagSharedM:
		shared = true
	default:
		return nil, fmt.Errorf("%w: invalid limits flag %#x", ErrInvalidBinary, flag)
	}

	min, err := c.u32()
	if err != nil {
		return nil, fmt.Errorf("read min: %w", err)
	}
	lim := &Limits{Min: min, Shared: shared}

	if flag == limitsFlagHasMax || flag == limitsFlagSharedM {
		max, err := c.u32()
		if err != nil {
			return nil, fmt.Errorf("read max: %w", err)
		}
		if max < min {
			return nil, fmt.Errorf("%w: size minimum %d exceeds maximum %d", ErrInvalidBinary, min, max)
		}
		lim.Max = &max
	}
	return lim, nil
}

func require(features api.CoreFeatures, feature api.CoreFeatures, name string) error {
	if features.IsEnabled(feature) {
		return nil
	}
	return fmt.Errorf("%w: %s is not enabled", ErrFeatureDisabled, name)
}

// wrapImport annotates an import error while preserving error identity so
// errors.Is(err, ErrFeatureDisabled) still succeeds for gated encodings.
func wrapImport(idx uint32, module, name, what string, err error) error {
	return fmt.Errorf("import[%d] %s.%s %s: %w", idx, module, name, what, err)
}
