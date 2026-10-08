package avro

import (
	"errors"
	"fmt"
	"math"
)

// ---------- enum ----------

// compileEnum builds the writer-symbol -> reader-ordinal table, identity
// included, exactly as resolveEnum does.
func (c *xcCompiler) compileEnum(r, w *schemaNode) xcFn {
	readerIdx := make(map[string]int, len(r.symbols))
	for i, s := range r.symbols {
		readerIdx[s] = i
	}

	identity := len(r.symbols) == len(w.symbols)
	mapping := make([]int, len(w.symbols))
	for i, ws := range w.symbols {
		if ri, ok := readerIdx[ws]; ok {
			mapping[i] = ri
			if ri != i {
				identity = false
			}
			continue
		}
		identity = false
		defIdx, ok := readerIdx[r.enumDef]
		if !ok {
			// Mirror resolveEnum: an enum default must itself be a reader
			// symbol. Return a compiler xcFn that fails at run time with
			// the same text; this pair cannot produce a value for the
			// missing symbol, so the error surfaces when that branch is
			// reached.
			msg := fmt.Sprintf("enum default %q not found in reader symbols", truncForError(r.enumDef))
			return xcRewrite(func(_ *xcState, _ *xcSink, _ []byte) ([]byte, error) {
				return nil, errors.New(msg)
			})
		}
		mapping[i] = defIdx
	}

	if identity {
		return c.verbatim(w)
	}

	return xcRewrite(func(_ *xcState, sink *xcSink, src []byte) ([]byte, error) {
		idx, rest, err := readVarint(src)
		if err != nil {
			return nil, err
		}
		if idx < 0 || int(idx) >= len(mapping) {
			return nil, fmt.Errorf("enum index %d out of range [0, %d)", idx, len(mapping))
		}
		var buf [5]byte
		out := appendVarint(buf[:0], int32(mapping[idx]))
		if err := sink.emitBytes(out); err != nil {
			return nil, err
		}
		return rest, nil
	})
}

// ---------- numeric / bytes promotions ----------

// promotion returns the transcoder for a cross-kind pair resolution allows,
// or nil. int>long and string<->bytes are wire-identical and go verbatim;
// the float widenings are rewrites.
func (c *xcCompiler) promotion(r, w *schemaNode) xcFn {
	switch w.kind + ">" + r.kind {
	case "int>long", "string>bytes", "bytes>string":
		return c.verbatim(w)
	case "int>float":
		return xcRewrite(promoteIntToFloat32)
	case "int>double":
		return xcRewrite(promoteIntToFloat64)
	case "long>float":
		return xcRewrite(promoteLongToFloat32)
	case "long>double":
		return xcRewrite(promoteLongToFloat64)
	case "float>double":
		return xcRewrite(promoteFloat32ToFloat64)
	}
	return nil
}

func promoteIntToFloat32(_ *xcState, sink *xcSink, src []byte) ([]byte, error) {
	n, rest, err := readVarint(src)
	if err != nil {
		return nil, err
	}
	var buf [4]byte
	out := appendUint32(buf[:0], math.Float32bits(float32(n)))
	return rest, sink.emitBytes(out)
}

func promoteIntToFloat64(_ *xcState, sink *xcSink, src []byte) ([]byte, error) {
	n, rest, err := readVarint(src)
	if err != nil {
		return nil, err
	}
	var buf [8]byte
	out := appendUint64(buf[:0], math.Float64bits(float64(n)))
	return rest, sink.emitBytes(out)
}

func promoteLongToFloat32(_ *xcState, sink *xcSink, src []byte) ([]byte, error) {
	n, rest, err := readVarlong(src)
	if err != nil {
		return nil, err
	}
	var buf [4]byte
	out := appendUint32(buf[:0], math.Float32bits(float32(n)))
	return rest, sink.emitBytes(out)
}

func promoteLongToFloat64(_ *xcState, sink *xcSink, src []byte) ([]byte, error) {
	n, rest, err := readVarlong(src)
	if err != nil {
		return nil, err
	}
	var buf [8]byte
	out := appendUint64(buf[:0], math.Float64bits(float64(n)))
	return rest, sink.emitBytes(out)
}

func promoteFloat32ToFloat64(_ *xcState, sink *xcSink, src []byte) ([]byte, error) {
	u, rest, err := readUint32(src)
	if err != nil {
		return nil, err
	}
	var buf [8]byte
	out := appendUint64(buf[:0], math.Float64bits(float64(math.Float32frombits(u))))
	return rest, sink.emitBytes(out)
}

// ---------- arrays ----------

func (c *xcCompiler) compileArray(r, w *schemaNode) (xcFn, error) {
	item, err := c.compile(r.items, w.items)
	if err != nil {
		return nil, err
	}
	// Identical item encoding: block framing and every item pass through.
	if _, ok := item.(*xcVerbatim); ok {
		return c.verbatim(w), nil
	}
	minItemBytes := c.mbw.minBytesOf(w.items)

	return xcRewrite(func(st *xcState, sink *xcSink, src []byte) (_ []byte, err error) {
		if st.slab.depth >= maxDepth {
			return nil, errTooDeep
		}
		st.slab.depth++
		defer func() { st.slab.depth-- }()

		var total int64
		var hdr [10]byte
		for {
			count, byteSize, rest, end, err := readBlockHeader(src, "array", true)
			if err != nil {
				return nil, xcFail("[]", err)
			}
			src = rest
			if end {
				if err := sink.emitView([]byte{0}); err != nil {
					return nil, err
				}
				return src, nil
			}
			total += count

			// Negative-count/byte-size framing names a contiguous body the
			// header already bounded (readBlockHeader verified byteSize <=
			// remaining); convert against the body, then jump past it.
			// Such a frame is not subject to the per-item remaining-buffer
			// count guard, which exists to bound plain-count frames.
			if byteSize > 0 {
				if err := checkArrayBlockBounds(count, total, int(byteSize), minItemBytes); err != nil {
					return nil, xcFail("[]", err)
				}
				if err := sink.emitBytes(appendVarlong(hdr[:0], count)); err != nil {
					return nil, err
				}
				body := src[:byteSize]
				brest := src[byteSize:]
				itemSrc := body
				for i := int64(0); i < count; i++ {
					itemSrc, err = item.xcRun(st, sink, itemSrc)
					if err != nil {
						return nil, xcFailKeep(fmt.Sprintf("[%d]", total-count+i), err)
					}
				}
				src = brest
				continue
			}
			if err := checkArrayBlockBounds(count, total, len(src), minItemBytes); err != nil {
				return nil, xcFail("[]", err)
			}
			if err := sink.emitBytes(appendVarlong(hdr[:0], count)); err != nil {
				return nil, err
			}
			for i := int64(0); i < count; i++ {
				src, err = item.xcRun(st, sink, src)
				if err != nil {
					return nil, xcFailKeep(fmt.Sprintf("[%d]", total-count+i), err)
				}
			}
		}
	}), nil
}

// ---------- maps ----------

func (c *xcCompiler) compileMap(r, w *schemaNode) (xcFn, error) {
	value, err := c.compile(r.values, w.values)
	if err != nil {
		return nil, err
	}
	if _, ok := value.(*xcVerbatim); ok {
		return c.verbatim(w), nil
	}
	minEntryBytes := mapEntryMinBytes(c.mbw.minBytesOf(w.values))

	return xcRewrite(func(st *xcState, sink *xcSink, src []byte) (_ []byte, err error) {
		if st.slab.depth >= maxDepth {
			return nil, errTooDeep
		}
		st.slab.depth++
		defer func() { st.slab.depth-- }()

		var total int64
		var hdr [10]byte
		for {
			count, byteSize, rest, end, err := readBlockHeader(src, "map", true)
			src = rest
			if err != nil {
				return nil, xcFail("{}", err)
			}
			if end {
				if err := sink.emitView([]byte{0}); err != nil {
					return nil, err
				}
				return src, nil
			}
			total += count

			// Negative-count/byte-size framing names a contiguous body the
			// header already bounded; check and convert against that body,
			// then jump past it.
			boundsSrc := len(src)
			if byteSize > 0 {
				boundsSrc = int(byteSize)
			}
			if err := checkMapBlockBounds(count, boundsSrc, minEntryBytes); err != nil {
				return nil, xcFail("{}", err)
			}

			if err := sink.emitBytes(appendVarlong(hdr[:0], count)); err != nil {
				return nil, err
			}
			if byteSize > 0 {
				itemSrc := src[:byteSize]
				for i := int64(0); i < count; i++ {
					keyLen, krest, kerr := readLength(itemSrc, "string")
					if kerr != nil {
						return nil, xcFail("{}", kerr)
					}
					key := krest[:keyLen]
					keySpanLen := len(itemSrc) - len(krest) + keyLen
					if err := sink.emitView(itemSrc[:keySpanLen]); err != nil {
						return nil, err
					}
					itemSrc = krest[keyLen:]
					itemSrc, kerr = value.xcRun(st, sink, itemSrc)
					if kerr != nil {
						return nil, xcFailKeep(fmt.Sprintf("[%q]", truncForError(string(key))), kerr)
					}
				}
				src = src[byteSize:]
				continue
			}
			for i := int64(0); i < count; i++ {
				keyLen, krest, err := readLength(src, "string")
				if err != nil {
					return nil, err
				}
				key := krest[:keyLen]
				// readLength stops after the length header; forward the
				// header plus the run, then position src after the run
				// before converting the value.
				keySpanLen := len(src) - len(krest) + keyLen
				if err := sink.emitView(src[:keySpanLen]); err != nil {
					return nil, err
				}
				src = krest[keyLen:]
				src, err = value.xcRun(st, sink, src)
				if err != nil {
					return nil, xcFailKeep(fmt.Sprintf("[%q]", truncForError(string(key))), err)
				}
			}
		}
	}), nil
}

// ---------- unions ----------

// compileWriterUnion: writer union, reader non-union. Every branch resolves
// against the reader; the branch index is consumed and the branch value
// converted.
func (c *xcCompiler) compileWriterUnion(r, w *schemaNode) (xcFn, error) {
	branches := make([]xcFn, len(w.branches))
	for i, wb := range w.branches {
		fn, err := c.compile(r, wb)
		if err != nil {
			return nil, err
		}
		branches[i] = fn
	}
	return xcRewrite(func(st *xcState, sink *xcSink, src []byte) ([]byte, error) {
		idx, rest, err := readVarint(src)
		if err != nil {
			return nil, err
		}
		if idx < 0 || int(idx) >= len(branches) {
			return nil, fmt.Errorf("union index %d out of range [0, %d)", idx, len(branches))
		}
		src2, err := branches[idx].xcRun(st, sink, rest)
		return src2, xcFailKeep(fmt.Sprintf("union[%d]", idx), err)
	}), nil
}

// compileReaderUnion: writer non-union, reader union. The writer selects one
// reader branch up front, and every value is emitted with that branch's tag.
func (c *xcCompiler) compileReaderUnion(r, w *schemaNode) (xcFn, error) {
	rb := newReaderBranchLookup(r).match(w)
	if rb == nil {
		return nil, &CompatibilityError{
			Path:       "(root)",
			ReaderType: "union",
			WriterType: w.kind,
			Detail:     detailWriterTypeNoReaderBranch,
		}
	}
	inner, err := c.compile(rb, w)
	if err != nil {
		return nil, err
	}
	tag := indexOfReaderBranch(r, rb)
	var tagBuf [5]byte
	tagBytes := appendVarint(tagBuf[:0], int32(tag))

	return xcRewrite(func(st *xcState, sink *xcSink, src []byte) ([]byte, error) {
		if err := sink.emitBytes(tagBytes); err != nil {
			return nil, err
		}
		return inner.xcRun(st, sink, src)
	}), nil
}

// compileUnionUnion maps each writer branch to its best matching reader
// branch and rewrites the tag.
func (c *xcCompiler) compileUnionUnion(r, w *schemaNode) (xcFn, error) {
	lookup := newReaderBranchLookup(r)
	branches := make([]xcFn, len(w.branches))
	tags := make([]int, len(w.branches))
	for i, wb := range w.branches {
		rb := lookup.match(wb)
		if rb == nil {
			return nil, &CompatibilityError{
				Path:       "(root)",
				ReaderType: "union",
				WriterType: fmt.Sprintf("union[%d]:%s", i, wb.kind),
				Detail:     detailWriterBranchNoReaderBranch,
			}
		}
		fn, err := c.compile(rb, wb)
		if err != nil {
			return nil, err
		}
		branches[i] = fn
		tags[i] = indexOfReaderBranch(r, rb)
	}

	return xcRewrite(func(st *xcState, sink *xcSink, src []byte) ([]byte, error) {
		idx, rest, err := readVarint(src)
		if err != nil {
			return nil, err
		}
		if idx < 0 || int(idx) >= len(branches) {
			return nil, fmt.Errorf("union index %d out of range [0, %d)", idx, len(branches))
		}
		var tagBuf [5]byte
		newTag := tags[idx]
		out, err2 := func() ([]byte, error) {
			if err := sink.emitBytes(appendVarint(tagBuf[:0], int32(newTag))); err != nil {
				return nil, err
			}
			return branches[idx].xcRun(st, sink, rest)
		}()
		return out, xcFailKeep(fmt.Sprintf("union[%d]", idx), err2)
	}), nil
}

// indexOfReaderBranch returns rb's position in r.branches. The lookup tables
// the parser builds index names, so equality on the branch pointer gives the
// ordinal the tag needs.
func indexOfReaderBranch(r, rb *schemaNode) int {
	for i := range r.branches {
		if r.branches[i] == rb {
			return i
		}
	}
	return 0
}
