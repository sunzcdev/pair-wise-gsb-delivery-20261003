package httphead

import (
	"bytes"
	"testing"
)

// incEvent is a recorded OptionScanner callback invocation.
type incEvent struct {
	index                    int
	option, attribute, value []byte
}

// refResult is the outcome of the one-shot reference scan, used as the oracle
// for incremental scans.
type refResult struct {
	events   []incEvent
	ok       bool
	errOff   int
	consumed int
}

// referenceScan mirrors ScanOptions over data, additionally recording the
// byte offset at which a lexical error occurred and the number of bytes fully
// consumed. Control decisions are provided by control.
func referenceScan(data []byte, control func(incEvent) Control) refResult {
	lexer := &Scanner{data: data}

	var res refResult
	res.errOff = -1

	var state int
	const (
		stateKey = iota
		stateParamBeforeName
		stateParamName
		stateParamBeforeValue
		stateParamValue
	)

	var (
		index             int
		key, param, value []byte
		mustCall          bool
	)

	for lexer.Next() {
		res.consumed = lexer.pos
		var (
			call      bool
			growIndex int
		)
		t := lexer.Type()
		v := lexer.Bytes()

		switch t {
		case ItemToken:
			switch state {
			case stateKey, stateParamBeforeName:
				key = v
				state = stateParamBeforeName
				mustCall = true
			case stateParamName:
				param = v
				state = stateParamBeforeValue
				mustCall = true
			case stateParamValue:
				value = v
				state = stateParamBeforeName
				call = true
			default:
				res.errOff = lexer.pos
				goto done
			}

		case ItemString:
			if state != stateParamValue {
				res.errOff = lexer.pos
				goto done
			}
			value = v
			state = stateParamBeforeName
			call = true

		case ItemSeparator:
			switch {
			case isComma(v) && state == stateKey:
			case isComma(v) && state == stateParamBeforeName:
				state = stateKey
				call = mustCall
				if !call {
					index++
				} else {
					growIndex = 1
				}
			case isComma(v) && state == stateParamBeforeValue:
				state = stateKey
				growIndex = 1
				call = true
			case isSemicolon(v) && state == stateParamBeforeName:
				state = stateParamName
			case isSemicolon(v) && state == stateParamBeforeValue:
				state = stateParamName
				call = true
			case isEquality(v) && state == stateParamBeforeValue:
				state = stateParamValue
			default:
				res.errOff = lexer.pos
				goto done
			}

		default:
			// ItemComment or ItemOctet reaching the grammar default.
			res.errOff = lexer.pos
			goto done
		}

		if call {
			ev := incEvent{index, key, param, value}
			res.events = append(res.events, copyEvent(ev))
			switch control(ev) {
			case ControlBreak:
				res.consumed = lexer.pos
				res.ok = true
				res.errOff = -2
				return res
			case ControlSkip:
				state = stateKey
				lexer.SkipEscaped(',')
			case ControlContinue:
			}
			res.consumed = lexer.pos
			res.ok = true
			param = nil
			value = nil
			mustCall = false
			index += growIndex
		}
	}
	if lexer.err {
		// For an illegal starting octet or a dangling CR the lexer keeps
		// its position at the offending byte; the error is reported one
		// byte past it. For an unterminated string or comment the fetch
		// helpers already advanced past the opening byte.
		res.errOff = lexer.pos + 1
		if under := lexer.data[:lexer.pos]; len(under) > 0 {
			if last := under[len(under)-1]; last == '"' || last == '(' {
				res.errOff = lexer.pos
			}
		}
		res.consumed = lexer.pos
	}
	if mustCall {
		ev := incEvent{index, key, param, value}
		res.events = append(res.events, copyEvent(ev))
		// ScanOptions ignores the trailing callback's control value.
		_ = control(ev)
		res.ok = true
	}

done:
	if res.errOff >= 0 {
		res.consumed = lexer.pos
		res.ok = false
	}
	return res
}

func copyEvent(e incEvent) incEvent {
	cp := func(b []byte) []byte {
		if b == nil {
			return nil
		}
		out := make([]byte, len(b))
		copy(out, b)
		return out
	}
	return incEvent{
		index:     e.index,
		option:    cp(e.option),
		attribute: cp(e.attribute),
		value:     cp(e.value),
	}
}

var incrementalCases = [][]byte{
	// Valid samples reused from parametersCases, plus boundary-heavy ones.
	[]byte(`a,b,c`),
	[]byte(`a,b,c;foo=1;bar=2`),
	[]byte(`c;foo;bar=2`),
	[]byte(`foo;bar=1;baz`),
	[]byte(`c;bar="2"`),
	[]byte(`c;bar=1,c;bar=2`),
	[]byte(`foo;a=1;b=2;c=3,bar;z,baz`),
	[]byte(`foo;a=1,, , ,bar;b=2`),
	[]byte(`c;bar="a,b;c=1"`),
	[]byte(`c;bar="a\"b"`),
	[]byte(`c;bar="a\"b\",baz"`),
	[]byte(` c ; bar = "x" , y `),
	[]byte(`a;b="\""`),
	[]byte(`a;b="\\"`),
	[]byte(`a;b="\"x\""`),
	[]byte(`foo;a=1\r\nb`),

	// Malformed samples.
	[]byte(`a,b;`),
	[]byte(`a;=1`),
	[]byte(`;a`),
	[]byte(`a;b==1`),
	[]byte(`a;"b"`),
	[]byte(`a;(b)`),
	[]byte(`a;b=(x)`),
	[]byte(`"abc"`),
	[]byte(`a;b="abc`),
	[]byte(`a\b`),
	[]byte(`a)`),
	[]byte(`a;;b`),
	[]byte(`a;b="unclosed`),
	[]byte("a;b=1\rc"),
	[]byte("a\r\nb"),
	[]byte("a\r\nb;c=1"),
	[]byte("a;b=1\r\nx"),
	[]byte("a;b=1\r"),
	[]byte("a;b=1\r\n"),
	[]byte("a "),
	[]byte(","),
	[]byte(``),
	[]byte(" \t "),
	[]byte("a;b=1;c"),
	[]byte(`a;b="a\"`),
}

// splitPoints returns every consecutive two-chunk split of data, including
// the 0 and len(data) boundaries.
func splitPoints(data []byte) [][2][]byte {
	var out [][2][]byte
	for i := 0; i <= len(data); i++ {
		out = append(out, [2][]byte{data[:i], data[i:]})
	}
	return out
}

func alwaysContinue(incEvent) Control { return ControlContinue }

func runIncremental(t *testing.T, chunks [][]byte, control func(incEvent) Control) (refResult, error) {
	var res refResult
	res.errOff = -1

	sc := NewOptionScanner(0, 0, func(index int, name, attr, val []byte) Control {
		ev := incEvent{index, name, attr, val}
		res.events = append(res.events, copyEvent(ev))
		return control(ev)
	})

	for _, ch := range chunks {
		n, err := sc.Write(ch)
		if err != nil {
			res.consumed = sc.Consumed()
			switch e := err.(type) {
			case *SyntaxError:
				res.errOff = e.Offset
			case *LimitError:
				res.errOff = e.Offset
			}
			return res, err
		}
		if sc.Stopped() {
			res.consumed = sc.Consumed()
			res.ok = true
			res.errOff = -2
			return res, nil
		}
		if n != len(ch) {
			t.Fatalf("unexpected short write: %d of %d", n, len(ch))
		}
	}

	err := sc.Finish()
	res.consumed = sc.Consumed()
	if err != nil {
		if e, ok := err.(*SyntaxError); ok {
			res.errOff = e.Offset
		}
		return res, err
	}
	res.ok = true
	return res, nil
}

func TestIncrementalSplits(t *testing.T) {
	for _, data := range incrementalCases {
		for si, split := range splitPoints(data) {
			name := string(data)
			t.Run(shortName(name)+"@"+itoa(si), func(t *testing.T) {
				want := referenceScan(data, alwaysContinue)
				got, _ := runIncremental(t, [][]byte{split[0], split[1]}, alwaysContinue)
				assertResult(t, data, want, got)
			})
		}
	}
}

func TestIncrementalByteByByte(t *testing.T) {
	for _, data := range incrementalCases {
		t.Run(shortName(string(data)), func(t *testing.T) {
			want := referenceScan(data, alwaysContinue)
			chunks := make([][]byte, 0, len(data))
			for i := 0; i < len(data); i++ {
				chunks = append(chunks, data[i:i+1])
			}
			got, _ := runIncremental(t, chunks, alwaysContinue)
			assertResult(t, data, want, got)
		})
	}
}

func TestIncrementalAllPrefixWrites(t *testing.T) {
	// Feed every possible prefix followed by the remainder, including empty
	// chunks interleaved.
	for _, data := range incrementalCases {
		for i := 0; i <= len(data); i++ {
			want := referenceScan(data, alwaysContinue)
			chunks := [][]byte{nil, data[:i], nil, data[i:], nil}
			got, _ := runIncremental(t, chunks, alwaysContinue)
			assertResult(t, data, want, got)
		}
	}
}

// TestIncrementalControls compares ControlBreak and ControlSkip behavior at
// every split point and every break position.
func TestIncrementalControls(t *testing.T) {
	data := []byte(`foo;a=1,bar;b=2,baz;c=3`)
	for breakAt := 0; breakAt < 2; breakAt++ {
		for split := 0; split <= len(data); split++ {
			policy := func(e incEvent) Control {
				if e.index >= breakAt {
					return ControlBreak
				}
				return ControlContinue
			}
			want := referenceScan(data, policy)
			got, _ := runIncremental(t, [][]byte{data[:split], data[split:]}, policy)
			assertResult(t, data, want, got)
		}
	}

	skipData := []byte(`foo;a=1,bad;x=99,good;g=7,also\,$bad;y=5`)
	for split := 0; split <= len(skipData); split++ {
		policy := func(e incEvent) Control {
			if bytes.Equal(e.option, []byte("bad")) {
				return ControlSkip
			}
			return ControlContinue
		}
		want := referenceScan(skipData, policy)
		got, _ := runIncremental(t, [][]byte{skipData[:split], skipData[split:]}, policy)
		assertResult(t, skipData, want, got)
	}
}

// TestIncrementalUnconsumed verifies that after an early ControlBreak the
// caller can read the unconsumed position of the last chunk.
func TestIncrementalUnconsumed(t *testing.T) {
	data := []byte(`a,b,c,d`)
	for split := 0; split <= len(data); split++ {
		sc := NewOptionScanner(0, 0, func(index int, name, attr, val []byte) Control {
			if bytes.Equal(name, []byte("b")) {
				return ControlBreak
			}
			return ControlContinue
		})
		n1, err := sc.Write(data[:split])
		_ = n1
		if err != nil && err != ErrStopped {
			t.Fatalf("split=%d: unexpected first write error: %v", split, err)
		}
		stoppedEarly := false
		if sc.Stopped() {
			stoppedEarly = true
		}
		var n2 int
		if !stoppedEarly {
			n2, err = sc.Write(data[split:])
			if err != nil && err != ErrStopped {
				t.Fatalf("split=%d: unexpected second write error: %v", split, err)
			}
			if !sc.Stopped() {
				t.Fatalf("split=%d: scanner not stopped", split)
			}
		}
		// The second option's event fires at the following comma, so the
		// one-shot lexer position is past that comma (offset 4).
		if want := 4; sc.Consumed() != want {
			t.Fatalf("split=%d: consumed=%d want %d (n2=%d err=%v)",
				split, sc.Consumed(), want, n2, err)
		}
	}
}

// TestIncrementalLimits exercises total input and per-option limits.
func TestIncrementalLimits(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		sc := NewOptionScanner(4, 0, func(int, []byte, []byte, []byte) Control {
			return ControlContinue
		})
		n, err := sc.Write([]byte("abcdef"))
		if n != 4 {
			t.Fatalf("consumed %d want 4", n)
		}
		le, ok := err.(*LimitError)
		if !ok || !le.Input || le.Offset != 4 {
			t.Fatalf("err=%#v want input LimitError at 4", err)
		}
		if sc.Consumed() != 4 {
			t.Fatalf("consumed position %d want 4", sc.Consumed())
		}
	})

	t.Run("option", func(t *testing.T) {
		data := []byte("aaaa,bb")
		for split := 0; split <= len(data); split++ {
			sc := NewOptionScanner(0, 3, func(int, []byte, []byte, []byte) Control {
				return ControlContinue
			})
			var err error
			if split > 0 {
				_, err = sc.Write(data[:split])
			}
			if err == nil && split < len(data) {
				_, err = sc.Write(data[split:])
			}
			if err == nil {
				err = sc.Finish()
			}
			le, ok := err.(*LimitError)
			if !ok || le.Input {
				t.Fatalf("split=%d: err=%#v want option LimitError", split, err)
			}
			// "aaaa" exceeds 3 at offset 3.
			if le.Offset != 3 {
				t.Fatalf("split=%d: offset=%d want 3", split, le.Offset)
			}
		}
	})

	t.Run("option_ok", func(t *testing.T) {
		sc := NewOptionScanner(0, 3, func(int, []byte, []byte, []byte) Control {
			return ControlContinue
		})
		if _, err := sc.Write([]byte("aaa,bb")); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if err := sc.Finish(); err != nil {
			t.Fatalf("unexpected finish: %v", err)
		}
	})
}

// TestIncrementalLifetime verifies that slices delivered by the callback stay
// valid when the caller reuses the chunk memory immediately after Write, and
// that a long option assembled over many chunks is owned by the scanner.
func TestIncrementalLifetime(t *testing.T) {
	data := []byte(`foo;bar="a,b",baz;qux=yyy`)

	// Case 1: callback retains slices and the chunk backing array is mutated
	// after Write returns.
	chunk := append([]byte(nil), data...)
	var retained []incEvent
	sc := NewOptionScanner(0, 0, func(index int, name, attr, val []byte) Control {
		retained = append(retained, copyEvent(incEvent{index, name, attr, val}))
		return ControlContinue
	})
	if _, err := sc.Write(chunk); err != nil {
		t.Fatal(err)
	}
	for i := range chunk {
		chunk[i] = 'X'
	}
	if err := sc.Finish(); err != nil {
		t.Fatal(err)
	}
	want := referenceScan(data, alwaysContinue)
	assertResult(t, data, want, refResult{events: retained, ok: true, errOff: -1, consumed: len(data)})

	// Case 2: feed one byte at a time using a reused 1-byte buffer.
	var events []incEvent
	sc2 := NewOptionScanner(0, 0, func(index int, name, attr, val []byte) Control {
		events = append(events, copyEvent(incEvent{index, name, attr, val}))
		return ControlContinue
	})
	buf := make([]byte, 1)
	for i := 0; i < len(data); i++ {
		buf[0] = data[i]
		n, err := sc2.Write(buf)
		if err != nil {
			t.Fatalf("byte %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("byte %d: consumed %d", i, n)
		}
		buf[0] = 0 // overwrite immediately
	}
	if err := sc2.Finish(); err != nil {
		t.Fatal(err)
	}
	assertResult(t, data, want, refResult{events: events, ok: true, errOff: -1, consumed: len(data)})
}

// TestIncrementalSplitEscapes checks escaping and LWS split specifically at
// every boundary, including backslash, quote, CR and separator cuts.
func TestIncrementalSplitEscapes(t *testing.T) {
	samples := [][]byte{
		[]byte(`a;b="\""`),
		[]byte(`a;b="x\\"`),
		[]byte(`a;b="a,b;c=d"`),
		[]byte("a\r\n b,c"),
		[]byte("a\r\nb"),
		[]byte(`a, b ,c`),
		[]byte(`a;b="\"\",c"`),
		[]byte(`a;b=1\,2`),
	}
	for _, data := range samples {
		for split := 0; split <= len(data); split++ {
			want := referenceScan(data, alwaysContinue)
			got, _ := runIncremental(t, [][]byte{data[:split], data[split:]}, alwaysContinue)
			assertResult(t, data, want, got)
		}
	}
}

func assertResult(t *testing.T, data []byte, want, got refResult) {
	t.Helper()
	if want.ok != got.ok {
		t.Fatalf("ok mismatch: got %v want %v (errOff got %d want %d)",
			got.ok, want.ok, got.errOff, want.errOff)
	}
	if want.errOff != got.errOff {
		t.Fatalf("error offset mismatch: got %d want %d (data %v)", got.errOff, want.errOff, data)
	}
	if want.consumed != got.consumed {
		t.Fatalf("consumed mismatch: got %d want %d", got.consumed, want.consumed)
	}
	if len(want.events) != len(got.events) {
		t.Fatalf("events len mismatch: got %d want %d\ngot: %v\nwant: %v",
			len(got.events), len(want.events), got.events, want.events)
	}
	for i := range want.events {
		w, g := want.events[i], got.events[i]
		if w.index != g.index ||
			!bytes.Equal(w.option, g.option) ||
			!bytes.Equal(w.attribute, g.attribute) ||
			!bytes.Equal(w.value, g.value) {
			t.Fatalf("event %d mismatch:\ngot  %#v\nwant %#v", i, g, w)
		}
	}
}

func shortName(s string) string {
	const n = 16
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
