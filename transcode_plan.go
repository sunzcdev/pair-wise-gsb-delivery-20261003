package avro

import (
	"errors"
	"fmt"
)

// xcFn transcodes one value of a compiled (reader, writer) pair from src,
// emitting through sink, and returns the unconsumed remainder. The
// concrete value is always one of:
//   - *xcVerbatim for an unchanged subtree (copy bytes straight through);
//   - a plain func literal for every rewriting op.
//
// The tagged value is how array/map compilers recognize an unchanged child
// where func values cannot be compared.
type xcFn interface {
	xcRun(st *xcState, sink *xcSink, src []byte) ([]byte, error)
}

// xcEntry is the seen-table value. open marks a pair whose compilation is
// in progress (a recursive call reached it); tramp is the slot the outer
// frame binds once the real function exists. fn is the resolved function.
type xcEntry struct {
	open  bool
	fn    xcFn
	tramp *xcFn
}

// xcVerbatim is the compiled "copy one value unchanged" op, tagged so array
// and map compilers can recognize an unchanged child schema where function
// values cannot be compared for equality.
type xcVerbatim struct {
	skip skipfn
}

func (v *xcVerbatim) xcRun(st *xcState, sink *xcSink, src []byte) ([]byte, error) {
	return xcSpan(v.skip, st, sink, src)
}

// xcRewrite is a rewriting conversion function.
type xcRewrite func(st *xcState, sink *xcSink, src []byte) ([]byte, error)

func (f xcRewrite) xcRun(st *xcState, sink *xcSink, src []byte) ([]byte, error) {
	return f(st, sink, src)
}

// xcCompiler holds the shared state of one plan compilation. seen mirrors
// resolveNode's three states keyed by (reader, writer): absent, in-progress
// (open with a trampoline), and resolved. Recursive compilation terminates
// through the trampoline closure the outer frame binds once the graph
// exists; NewTranscoder verifies every cycle bound before returning.
type xcCompiler struct {
	seen       map[nodePair]*xcEntry
	verbatimOf map[*schemaNode]*xcVerbatim
	mbw        *minBytesWalk
}

func (c *xcCompiler) verbatim(w *schemaNode) *xcVerbatim {
	if c.verbatimOf == nil {
		c.verbatimOf = make(map[*schemaNode]*xcVerbatim)
	}
	if v, ok := c.verbatimOf[w]; ok {
		return v
	}
	skip := buildSkip(w, c.mbw)
	v := &xcVerbatim{skip: skip}
	c.verbatimOf[w] = v
	return v
}

func (c *xcCompiler) compile(r, w *schemaNode) (xcFn, error) {
	// The same compiled node on both sides is one encoding: copy the whole
	// subtree verbatim. This also breaks self-referential cycles at
	// compile time; the verbatim skipper bounds the runtime recursion.
	if r == w {
		return c.verbatim(r), nil
	}
	pair := nodePair{r, w}
	if e, ok := c.seen[pair]; ok {
		if !e.open {
			return e.fn, nil
		}
		// Recursive pair: install a trampoline keyed to a slot the outer
		// frame fills once the real function exists.
		var real xcFn
		trampoline := xcRewrite(func(st *xcState, sink *xcSink, src []byte) ([]byte, error) {
			return real.xcRun(st, sink, src)
		})
		// Share one trampoline across repeated recursive visits: keep the
		// first entry and the slot it closed over.
		if e.tramp == nil {
			e.fn = trampoline
			e.tramp = &real
		}
		return e.fn, nil
	}

	e := &xcEntry{open: true}
	c.seen[pair] = e

	fn, err := c.compileNode(r, w)
	if err != nil {
		return nil, err
	}

	e.open = false
	if e.tramp != nil {
		// A recursive call replaced this entry's fn with a trampoline and
		// opened a slot. Bind the slot and keep the trampoline so every
		// holder reaches the real implementation.
		*e.tramp = fn
		if e.fn == nil {
			return nil, errors.New("avro: internal: transcoder cycle lost its trampoline")
		}
		return e.fn, nil
	}
	e.fn = fn
	return fn, nil
}

// compileNode mirrors doResolve's dispatch.
func (c *xcCompiler) compileNode(r, w *schemaNode) (xcFn, error) {
	if w.kind == "union" && r.kind != "union" {
		return c.compileWriterUnion(r, w)
	}
	if r.kind == "union" && w.kind != "union" {
		return c.compileReaderUnion(r, w)
	}
	if r.kind == "union" && w.kind == "union" {
		return c.compileUnionUnion(r, w)
	}

	if r.kind == w.kind {
		switch r.kind {
		case "record":
			return c.compileRecord(r, w)
		case "enum":
			return c.compileEnum(r, w), nil
		case "array":
			return c.compileArray(r, w)
		case "map":
			return c.compileMap(r, w)
		default:
			// null, boolean, int, long, float, double, bytes, string,
			// fixed: compatibility required equal fixed names and sizes,
			// and same-kind scalars share one wire encoding, so writer
			// bytes are reader bytes verbatim. Logical types never change
			// the underlying encoding at this level.
			return c.verbatim(w), nil
		}
	}

	if fn := c.promotion(r, w); fn != nil {
		return fn, nil
	}
	return nil, &CompatibilityError{
		Path:       "(root)",
		ReaderType: r.kind,
		WriterType: w.kind,
		Detail:     "incompatible types",
	}
}

// xcSpan runs skip over src, forwarding the spanned bytes, and returns the
// remainder.
func xcSpan(skip skipfn, st *xcState, sink *xcSink, src []byte) ([]byte, error) {
	end, err := skip(src, &st.slab)
	if err != nil {
		return nil, err
	}
	if err := sink.emitView(src[:len(src)-len(end)]); err != nil {
		return nil, err
	}
	return end, nil
}

// ---------- records ----------

type xcFieldOp struct {
	readerIdx  int // reader field index, or -1 for a dropped writer field
	readerName string
	convert    xcFn
	skip       skipfn
	writerName string
}

type xcDefault struct {
	readerIdx int
	encoded   []byte
}

func (c *xcCompiler) compileRecord(r, w *schemaNode) (xcFn, error) {
	readerByName := newReaderFieldLookup(r)

	var ops []xcFieldOp
	matched := make([]bool, len(r.fields))
	matchedBy := make([]string, len(r.fields))

	for _, wf := range w.fields {
		ri := readerByName.index(wf.name)
		if ri < 0 {
			ops = append(ops, xcFieldOp{
				readerIdx:  -1,
				skip:       buildSkip(wf.node, c.mbw),
				writerName: wf.name,
			})
			continue
		}
		if matched[ri] {
			return nil, &CompatibilityError{
				Path:       r.name,
				ReaderType: "record",
				WriterType: "record",
				Detail: fmt.Sprintf("writer fields %q and %q both resolve to reader field %q (via name + alias collision); rename the writer or drop the alias to disambiguate",
					truncForError(matchedBy[ri]), truncForError(wf.name), truncForError(r.fields[ri].name)),
			}
		}
		matched[ri] = true
		matchedBy[ri] = wf.name
		fn, err := c.compile(r.fields[ri].node, wf.node)
		if err != nil {
			return nil, err
		}
		ops = append(ops, xcFieldOp{
			readerIdx:  ri,
			readerName: r.fields[ri].name,
			convert:    fn,
			writerName: wf.name,
		})
	}

	var defaults []xcDefault
	for i, rf := range r.fields {
		if matched[i] {
			continue
		}
		encoded, err := encodeDefault(nil, rf.defaultVal, rf.node)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", truncForError(rf.name), err)
		}
		defaults = append(defaults, xcDefault{readerIdx: i, encoded: encoded})
	}

	// Streaming holds exactly when matched reader indices arrive in
	// non-decreasing writer order: dropped fields are skipped, and defaults
	// then fill prefix gaps and the trailing tail. A reversal forces
	// capture and reassembly.
	streaming := true
	prev := -1
	for _, op := range ops {
		if op.readerIdx < 0 {
			continue
		}
		if op.readerIdx < prev {
			streaming = false
			break
		}
		prev = op.readerIdx
	}

	if streaming {
		return streamingRecord(ops, defaults), nil
	}
	return bufferedRecord(ops, defaults), nil
}

func streamingRecord(ops []xcFieldOp, defaults []xcDefault) xcFn {
	return xcRewrite(func(st *xcState, sink *xcSink, src []byte) (_ []byte, err error) {
		if st.slab.depth >= maxDepth {
			return nil, errTooDeep
		}
		st.slab.depth++
		defer func() { st.slab.depth-- }()

		di := 0
		for _, op := range ops {
			if op.readerIdx < 0 {
				src, err = op.skip(src, &st.slab)
				if err != nil {
					return nil, xcFail(op.writerName, err)
				}
				continue
			}
			// Emit defaults for reader fields preceding this one.
			for di < len(defaults) && defaults[di].readerIdx < op.readerIdx {
				if err = sink.emitView(defaults[di].encoded); err != nil {
					return nil, err
				}
				di++
			}
			src, err = op.convert.xcRun(st, sink, src)
			if err != nil {
				return nil, xcFailField(op.readerName, err)
			}
		}
		for ; di < len(defaults); di++ {
			if err = sink.emitView(defaults[di].encoded); err != nil {
				return nil, err
			}
		}
		return src, nil
	})
}

// getCapture borrows a capture sink that writes assembled records into
// parent. Captures are heap objects pooled per call so a nested buffered
// record pushing its own capture cannot invalidate the parent's pointer.
func (st *xcState) getCapture(nFields int, parent *xcSink) *xcSink {
	var cap_ *xcSink
	if n := len(st.freeCaps); n > 0 {
		cap_ = st.freeCaps[n-1]
		st.freeCaps = st.freeCaps[:n-1]
		if cap(cap_.fields) < nFields {
			cap_.fields = make([]xcFrag, nFields)
		} else {
			clear(cap_.fields[:nFields])
		}
		cap_.scratch = cap_.scratch[:0]
	} else {
		cap_ = &xcSink{fields: make([]xcFrag, nFields)}
	}
	cap_.mode = sinkCapture
	cap_.parent = parent
	cap_.cur = nil
	cap_.err = nil
	cap_.maxBuf = st.maxBuf
	return cap_
}

// putCapture returns a capture to the per-call pool.
func (st *xcState) putCapture(cap_ *xcSink) {
	st.freeCaps = append(st.freeCaps, cap_)
}

func bufferedRecord(ops []xcFieldOp, defaults []xcDefault) xcFn {
	nFields := 0
	for _, op := range ops {
		if op.readerIdx >= nFields {
			nFields = op.readerIdx + 1
		}
	}
	if len(defaults) > 0 {
		if last := defaults[len(defaults)-1].readerIdx + 1; last > nFields {
			nFields = last
		}
	}

	return xcRewrite(func(st *xcState, sink *xcSink, src []byte) (_ []byte, err error) {
		if st.slab.depth >= maxDepth {
			return nil, errTooDeep
		}
		st.slab.depth++
		defer func() { st.slab.depth-- }()

		cap_ := st.getCapture(nFields, sink)
		defer st.putCapture(cap_)

		for _, op := range ops {
			if op.readerIdx < 0 {
				src, err = op.skip(src, &st.slab)
				if err != nil {
					return nil, xcFail(op.writerName, err)
				}
				continue
			}
			cap_.cur = &cap_.fields[op.readerIdx]
			cap_.cur.present = true
			src, err = op.convert.xcRun(st, cap_, src)
			cap_.cur = nil
			if err != nil {
				return nil, xcFailField(op.readerName, err)
			}
		}

		di := 0
		for ri := 0; ri < nFields; ri++ {
			frag := &cap_.fields[ri]
			if frag.present {
				for _, rn := range frag.ranges {
					if rn.view != nil {
						err = sink.emitView(rn.view)
					} else {
						err = sink.emitBytes(cap_.scratch[rn.start:rn.end])
					}
					if err != nil {
						return nil, err
					}
				}
				continue
			}
			for di < len(defaults) && defaults[di].readerIdx < ri {
				di++
			}
			if di < len(defaults) && defaults[di].readerIdx == ri {
				if err = sink.emitView(defaults[di].encoded); err != nil {
					return nil, err
				}
				di++
			}
		}
		return src, nil
	})
}
