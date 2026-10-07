package importmanifest

import (
	"errors"
	"fmt"
	"io"

	"github.com/tetratelabs/wazero/api"
)

// WebAssembly binary constants used while parsing.
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#binary-format
const (
	sectionIDCustom byte = 0
	sectionIDType   byte = 1
	sectionIDImport byte = 2

	externTypeTag byte = 0x04

	limitsFlagNoMax   byte = 0x00
	limitsFlagHasMax  byte = 0x01
	limitsFlagShared  byte = 0x02
	limitsFlagSharedM byte = 0x03

	globalImmutable byte = 0x00
	globalMutable   byte = 0x01
)

// ErrInvalidBinary is returned when the bytes are not a well-formed WebAssembly
// module far enough to read an import manifest.
var ErrInvalidBinary = errors.New("invalid WebAssembly binary")

// ErrFeatureDisabled reports that the binary requests a WebAssembly feature
// that was not enabled for the read. Use [errors.Is] to detect it.
var ErrFeatureDisabled = errors.New("feature disabled")

// Read parses the import inventory from a WebAssembly binary without
// instantiating the module or running its start function.
//
// Only the type and import sections are decoded; every other section is
// skipped by its declared length, so no function bodies, data, globals or
// start function are evaluated or executed.
//
// enabledFeatures gates proposal-specific encoding, mirroring the rest of
// wazero: an import that relies on a disabled feature fails with an error
// wrapping [ErrFeatureDisabled], and any unknown encoding is rejected rather
// than guessed. Pass api.CoreFeaturesV2 (or a broader set) to allow reference
// types, SIMD value types, multi-value signatures, and similar.
func Read(bin []byte, enabledFeatures api.CoreFeatures) (*Manifest, error) {
	c := &cursor{buf: bin}

	if !c.take(0x00, 0x61, 0x73, 0x6d) {
		return nil, fmt.Errorf("%w: bad magic number", ErrInvalidBinary)
	}
	if !c.take(0x01, 0x00, 0x00, 0x00) {
		return nil, fmt.Errorf("%w: unsupported binary version", ErrInvalidBinary)
	}

	var types []FunctionSignature
	var imports []Import

	var lastSectionID byte
	for c.remaining() > 0 {
		sectionID, err := c.byte()
		if err != nil {
			return nil, err
		}
		sectionSize, err := c.u32()
		if err != nil {
			// Once the import section has been read, any trailing bytes that do
			// not form a complete later section are irrelevant to the manifest
			// and must never be executed, so stop rather than reject them.
			if imports != nil {
				break
			}
			return nil, fmt.Errorf("%w: reading section size: %v", ErrInvalidBinary, err)
		}

		if sectionID != sectionIDCustom {
			if sectionID <= lastSectionID {
				return nil, fmt.Errorf("%w: sections out of order (id %d after %d)",
					ErrInvalidBinary, sectionID, lastSectionID)
			}
			lastSectionID = sectionID
		}

		body, err := c.bytes(sectionSize)
		if err != nil {
			return nil, fmt.Errorf("%w: section %d length: %v", ErrInvalidBinary, sectionID, err)
		}
		sc := &cursor{buf: body}

		switch sectionID {
		case sectionIDType:
			types, err = decodeTypeSection(sc, enabledFeatures)
		case sectionIDImport:
			imports, err = decodeImportSection(sc, types, enabledFeatures)
		default:
			// Custom sections and every section we do not need are ignored.
			// Code/data/start are never inspected or executed.
		}
		if err != nil {
			return nil, err
		}

		// After the import section, no later section can add types or imports.
		if sectionID > sectionIDImport && sectionID != sectionIDCustom && imports != nil {
			break
		}
	}

	return buildManifest(imports), nil
}

func buildManifest(imports []Import) *Manifest {
	m := &Manifest{Imports: imports, PerModule: map[string][]Import{}}
	for i := range imports {
		imp := imports[i]
		m.PerModule[imp.Module] = append(m.PerModule[imp.Module], imp)
	}
	return m
}

// cursor is a bounds-checked, read-only view over a byte slice.
type cursor struct {
	buf []byte
	pos int
	err error
}

func (c *cursor) remaining() int { return len(c.buf) - c.pos }

func (c *cursor) byte() (byte, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.pos >= len(c.buf) {
		c.err = io.EOF
		return 0, c.err
	}
	b := c.buf[c.pos]
	c.pos++
	return b, nil
}

func (c *cursor) take(want ...byte) bool {
	for _, w := range want {
		b, err := c.byte()
		if err != nil || b != w {
			return false
		}
	}
	return true
}

func (c *cursor) bytes(n uint32) ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	if uint64(c.pos)+uint64(n) > uint64(len(c.buf)) {
		c.err = io.ErrUnexpectedEOF
		return nil, c.err
	}
	start := c.pos
	c.pos += int(n)
	return c.buf[start:c.pos], nil
}

// u32 decodes an unsigned LEB128 integer.
func (c *cursor) u32() (uint32, error) {
	var result uint32
	var shift uint
	for {
		b, err := c.byte()
		if err != nil {
			return 0, err
		}
		if shift == 28 && b&0xf0 != 0 {
			return 0, errors.New("LEB128 integer overflows 32 bits")
		}
		result |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return result, nil
}

// name decodes a UTF-8 length-prefixed string.
func (c *cursor) name(what string, idx uint32) (string, error) {
	n, err := c.u32()
	if err != nil {
		return "", fmt.Errorf("import[%d] %s name length: %w", idx, what, err)
	}
	raw, err := c.bytes(n)
	if err != nil {
		return "", fmt.Errorf("import[%d] %s name: %w", idx, what, err)
	}
	return string(raw), nil
}

// s33 decodes a signed 33-bit LEB128 integer (used for reference heap types).
func (c *cursor) s33() (int64, error) {
	var result int64
	var shift uint
	var b byte
	for {
		var err error
		b, err = c.byte()
		if err != nil {
			return 0, err
		}
		if shift >= 35 {
			return 0, errors.New("signed LEB128 integer too long")
		}
		result |= int64(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			break
		}
	}
	// Sign-extend when the final group's sign bit is set.
	if shift < 64 && b&0x40 != 0 {
		result |= -(1 << shift)
	}
	// Reject values that do not fit in a signed 33-bit range.
	if result < -(1<<32) || result > (1<<32)-1 {
		return 0, errors.New("signed LEB128 integer overflows 33 bits")
	}
	return result, nil
}
