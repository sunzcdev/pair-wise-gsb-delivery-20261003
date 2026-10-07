package importmanifest

// Minimal WebAssembly binary builder used to synthesize type/import sections
// without a wat toolchain.

type wasmBuilder struct {
	types       []byte
	typeCount   uint32
	imports     []byte
	importCount uint32
}

func newBuilder() *wasmBuilder { return &wasmBuilder{} }

func (b *wasmBuilder) functype(params, results []byte) uint32 {
	idx := b.typeCount
	b.types = append(b.types, 0x60)
	b.types = appendU32(b.types, uint32(len(params)))
	b.types = append(b.types, params...)
	b.types = appendU32(b.types, uint32(len(results)))
	b.types = append(b.types, results...)
	b.typeCount++
	return idx
}

func (b *wasmBuilder) beginImport(module, name string) {
	b.imports = append(b.imports, encodeName(module)...)
	b.imports = append(b.imports, encodeName(name)...)
}

func (b *wasmBuilder) importFunc(module, name string, typeIdx uint32) {
	b.beginImport(module, name)
	b.imports = append(b.imports, KindFunc)
	b.imports = appendU32(b.imports, typeIdx)
	b.importCount++
}

func (b *wasmBuilder) importGlobal(module, name string, valType byte, mutable bool) {
	b.beginImport(module, name)
	b.imports = append(b.imports, KindGlobal, valType)
	if mutable {
		b.imports = append(b.imports, 0x01)
	} else {
		b.imports = append(b.imports, 0x00)
	}
	b.importCount++
}

func (b *wasmBuilder) importMemory(module, name string, min uint32, max *uint32, shared bool) {
	b.beginImport(module, name)
	b.imports = append(b.imports, KindMemory)
	b.imports = appendLimits(b.imports, min, max, shared)
	b.importCount++
}

func (b *wasmBuilder) importTable(module, name string, elem byte, min uint32, max *uint32) {
	b.beginImport(module, name)
	b.imports = append(b.imports, KindTable, elem)
	b.imports = appendLimits(b.imports, min, max, false)
	b.importCount++
}

// rawImport appends a hand-built import descriptor, used to inject malformed
// bytes that the normal typed helpers cannot produce.
func (b *wasmBuilder) rawImport(module, name string, desc ...byte) {
	b.beginImport(module, name)
	b.imports = append(b.imports, desc...)
	b.importCount++
}

func (b *wasmBuilder) build() []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if b.typeCount > 0 {
		body := appendU32(nil, b.typeCount)
		body = append(body, b.types...)
		out = appendSection(out, sectionIDType, body)
	}
	if b.importCount > 0 {
		body := appendU32(nil, b.importCount)
		body = append(body, b.imports...)
		out = appendSection(out, sectionIDImport, body)
	}
	return out
}

func appendU32(buf []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		buf = append(buf, c)
		if v == 0 {
			return buf
		}
	}
}

func appendS33(buf []byte, v int64) []byte {
	for {
		c := byte(v & 0x7f)
		sign := byte(v & 0x40)
		v >>= 7
		if (v != -1 || sign == 0) && (v != 0 || sign != 0) {
			c |= 0x80
		}
		buf = append(buf, c)
		if c&0x80 == 0 {
			return buf
		}
	}
}

func encodeName(s string) []byte {
	buf := appendU32(nil, uint32(len(s)))
	return append(buf, s...)
}

func appendLimits(buf []byte, min uint32, max *uint32, shared bool) []byte {
	switch {
	case max != nil && shared:
		buf = append(buf, 0x03)
	case max != nil:
		buf = append(buf, 0x01)
	case shared:
		buf = append(buf, 0x02)
	default:
		buf = append(buf, 0x00)
	}
	buf = appendU32(buf, min)
	if max != nil {
		buf = appendU32(buf, *max)
	}
	return buf
}

func appendSection(out []byte, id byte, body []byte) []byte {
	out = append(out, id)
	out = appendU32(out, uint32(len(body)))
	out = append(out, body...)
	return out
}
