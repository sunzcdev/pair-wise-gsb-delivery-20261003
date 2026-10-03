package gjson

import (
	"fmt"
	"strings"
)

// PathError describes a syntax error encountered while compiling a path.
// Position is the byte offset into the original path where the error was
// detected.
type PathError struct {
	Path     string
	Position int
	Message  string
}

// Error implements the error interface.
func (e *PathError) Error() string {
	if e.Position >= 0 && e.Position <= len(e.Path) {
		return fmt.Sprintf("gjson: %s at position %d: %q",
			e.Message, e.Position, e.Path)
	}
	return fmt.Sprintf("gjson: %s: %q", e.Message, e.Path)
}

// Plan is a compiled, reusable query plan returned by Compile.
// A Plan is immutable after creation and is safe for concurrent use by
// multiple goroutines on different inputs. Executing a Plan never mutates
// shared state, so a failed or empty result on one input never affects the
// result of any later execution.
type Plan struct {
	raw string
	k   planKind

	// head / lines
	steps []planStep
	lines bool

	// mod
	fn    func(json, arg string) string
	args  string
	after *Plan

	// static
	staticVal Result

	// subsel
	subs []compiledSub
	kind byte // '[' or '{' for subsel
}

type planKind int

const (
	pHead planKind = iota
	pLines
	pMod
	pStatic
	pSubsel
)

// compiledSub is a precompiled subselector entry.
type compiledSub struct {
	plan *Plan
	name string // raw name text (including quotes) for '{' subselectors
}

// planStep is one compiled path component with both object and array
// interpretations precomputed.
type planStep struct {
	part string // object interpretation, unescaped
	wild bool
	pipe *Plan

	arr arrayDesc
}

type arrayKind int

const (
	aPlain arrayKind = iota
	aDead
	aCount
	aAlog
	aQuery
)

type arrayDesc struct {
	kind  arrayKind
	idx   int  // aPlain index
	alog  *Plan // aAlog: per-element sub path
	apipe *Plan // aAlog: trailing pipe
	query *queryPlan
}

type queryPlan struct {
	inner  *Plan  // plan run on each candidate before comparing
	op     string
	value  string // comparison value after quote stripping/unescaping
	quoted bool   // value was a quoted string
	all    bool   // gather all matches ("#(q)#")
	rest   *Plan  // suffix run on a matched candidate
	restP  *Plan  // pipe suffix, applied to first match
}

// Compile parses path once and returns a reusable query plan.
//
// All path syntax processing — escaped dots and pipes, strings inside
// queries, nested subselectors and modifiers with JSON arguments — is done
// during compilation. Executing the returned plan with Plan.Get performs no
// path parsing.
//
// Invalid paths return a *PathError describing the exact byte position of
// the problem, such as an unterminated query or subselector, or a modifier
// argument with unbalanced JSON. Every accepted plan preserves the exact
// semantics of Get for that path.
func Compile(path string) (*Plan, error) {
	cp := &compiler{raw: path}
	return cp.compile(path, 0)
}

// MustCompile is like Compile but panics if the path cannot be compiled.
func MustCompile(path string) *Plan {
	p, err := Compile(path)
	if err != nil {
		panic(err)
	}
	return p
}

// Get executes the compiled plan against json and returns the Result.
// It is safe to call concurrently on different json inputs.
func (p *Plan) Get(json string) Result {
	return p.exec(json, nil, 0)
}

// GetBytes executes the compiled plan against json and returns the Result.
// It is safe to call concurrently on different json inputs.
func (p *Plan) GetBytes(json []byte) Result {
	if json == nil {
		return Result{}
	}
	res := p.Get(bytesString(json))
	if res.Raw != "" {
		res.Raw = string(stringBytes(res.Raw))
	}
	if res.Str != "" {
		res.Str = string(stringBytes(res.Str))
	}
	return res
}

// String returns the original path used to compile the plan.
func (p *Plan) String() string {
	return p.raw
}

type compiler struct {
	raw string
}

func (cp *compiler) err(pos int, msg string) error {
	return &PathError{Path: cp.raw, Position: pos, Message: msg}
}

// compile compiles path, where base is the offset of path within cp.raw.
func (cp *compiler) compile(path string, base int) (*Plan, error) {
	p := &Plan{raw: path}

	// "..rest" -> json lines mode
	if len(path) >= 2 && path[0] == '.' && path[1] == '.' {
		steps, err := cp.compileHead(path[2:], base+2)
		if err != nil {
			return nil, err
		}
		p.k = pLines
		p.steps = steps
		p.lines = true
		return p, nil
	}

	if len(path) > 1 {
		// leading modifier
		if path[0] == '@' && !DisableModifiers {
			if mp, ok, err := cp.compileModifier(path, base); err != nil {
				return nil, err
			} else if ok {
				return mp, nil
			}
		}
		// leading static value
		if path[0] == '!' {
			if sp, ok, err := cp.compileStatic(path, base); err != nil {
				return nil, err
			} else if ok {
				return sp, nil
			}
		}
		// leading subselector
		if path[0] == '[' || path[0] == '{' {
			if sp, ok, err := cp.compileSubsel(path, base); err != nil {
				return nil, err
			} else if ok {
				return sp, nil
			}
		}
	}

	steps, err := cp.compileHead(path, base)
	if err != nil {
		return nil, err
	}
	p.k = pHead
	p.steps = steps
	return p, nil
}

// headToken is one object-interpretation token of a path prefix.
type headToken struct {
	epart string // raw component text
	part  string // unescaped component text
	wild  bool
	esc   bool // component contained a backslash
	start int  // token start offset within the compiled substring
	end   int  // token end offset (exclusive)
	pipe  string
}

// tokenizeHead mirrors parseObjectPath exactly, including escape handling
// and dot-pipe detection.
func (cp *compiler) tokenizeHead(path string, base int) ([]headToken, error) {
	var tokens []headToken
	i := 0
	start := 0
	for i < len(path) {
		switch path[i] {
		case '|':
			tokens = append(tokens, headToken{
				epart: path[start:i], part: path[start:i],
				start: start, end: i, pipe: path[i+1:],
			})
			return tokens, nil
		case '.':
			if i < len(path)-1 && isDotPiperChar(path[i+1:]) {
				tokens = append(tokens, headToken{
					epart: path[start:i], part: path[start:i],
					start: start, end: i, pipe: path[i+1:],
				})
				return tokens, nil
			}
			tokens = append(tokens, headToken{
				epart: path[start:i], part: path[start:i],
				start: start, end: i,
			})
			i++
			start = i
		case '\\':
			// escape mode: build the unescaped component and stop at
			// the first unescaped '.' or '|'
			epart := []byte(path[start:i])
			var wild bool
			i++
			if i < len(path) {
				epart = append(epart, path[i])
				if path[i] == '*' || path[i] == '?' {
					wild = true
				}
				i++
				for i < len(path) {
					if path[i] == '\\' {
						i++
						if i < len(path) {
							epart = append(epart, path[i])
						}
						continue
					}
					if path[i] == '.' {
						if i < len(path)-1 &&
							isDotPiperChar(path[i+1:]) {
							tokens = append(tokens, headToken{
								epart: string(epart),
								part:  string(epart),
								wild:  wild, esc: true,
								start: start, end: i,
								pipe:  path[i+1:],
							})
							return tokens, nil
						}
						tokens = append(tokens, headToken{
							epart: string(epart),
							part:  string(epart),
							wild:  wild, esc: true,
							start: start, end: i,
						})
						i++
						start = i
						goto nextComp
					}
					if path[i] == '|' {
						tokens = append(tokens, headToken{
							epart: string(epart),
							part:  string(epart),
							wild:  wild, esc: true,
							start: start, end: i,
							pipe:  path[i+1:],
						})
						return tokens, nil
					}
					if path[i] == '*' || path[i] == '?' {
						wild = true
					}
					epart = append(epart, path[i])
					i++
				}
			}
			tokens = append(tokens, headToken{
				epart: string(epart), part: string(epart), wild: wild,
				esc: true, start: start, end: len(path),
			})
			return tokens, nil
		default:
			if i == start && path[i] == '#' && i+1 < len(path) &&
				(path[i+1] == '(' || path[i+1] == '[') {
				_, _, _, _, fi, _, qok := parseQuery(path[i:])
				if qok {
					i += fi
					if i < len(path) && path[i] == '#' {
						i++
					}
					continue
				}
			}
			i++
		}
		continue
	nextComp:
		for i < len(path) {
			switch path[i] {
			case '|':
				tokens = append(tokens, headToken{
					epart: path[start:i], part: path[start:i],
					start: start, end: i, pipe: path[i+1:],
				})
				return tokens, nil
			case '.':
				if i < len(path)-1 && isDotPiperChar(path[i+1:]) {
					tokens = append(tokens, headToken{
						epart: path[start:i], part: path[start:i],
						start: start, end: i, pipe: path[i+1:],
					})
					return tokens, nil
				}
				tokens = append(tokens, headToken{
					epart: path[start:i], part: path[start:i],
					start: start, end: i,
				})
				i++
				start = i
			case '\\':
				i++
				if i < len(path) {
					i++
				}
			default:
				i++
			}
		}
	}
	tokens = append(tokens, headToken{
		epart: path[start:], part: path[start:],
		start: start, end: len(path),
	})
	return tokens, nil
}

// compileHead compiles a plain chain of components, possibly ending in a
// pipe.
func (cp *compiler) compileHead(
	path string, base int,
) (steps []planStep, err error) {
	tokens, err := cp.tokenizeHead(path, base)
	if err != nil {
		return nil, err
	}
	for ti := range tokens {
		tok := &tokens[ti]
		st := planStep{part: tok.part, wild: tok.wild}
		st.arr, err = cp.compileArrayDesc(path, tok, base)
		if err != nil {
			return nil, err
		}
		if tok.pipe != "" {
			st.pipe, err = cp.compile(tok.pipe, base+tok.end+1)
			if err != nil {
				return nil, err
			}
			steps = append(steps, st)
			return steps, nil
		}
		steps = append(steps, st)
	}
	return steps, nil
}

// compileArrayDesc derives the array interpretation of a step using
// parseArrayPath on the full suffix exactly as the runtime does.
func (cp *compiler) compileArrayDesc(
	path string, tok *headToken, base int,
) (desc arrayDesc, err error) {
	suffix := path[tok.start:]
	tokBase := base + tok.start
	rp := parseArrayPath(suffix)

	// components containing escapes never match under array semantics
	if tok.esc {
		desc.kind = aDead
		return desc, nil
	}

	if !rp.arrch {
		n, ok := parseUint(rp.part)
		if !ok {
			desc.kind = aDead
			return desc, nil
		}
		desc.kind = aPlain
		desc.idx = int(n)
		return desc, nil
	}

	// arrch is only meaningful when '#' begins the first component
	if tok.epart != "#" && !(len(tok.epart) >= 2 && tok.epart[0] == '#' &&
		(tok.epart[1] == '(' || tok.epart[1] == '[' ||
			tok.epart[1] == '.')) {
		desc.kind = aDead
		return desc, nil
	}

	switch {
	case rp.alogok:
		return cp.compileAlog(suffix, rp.alogkey, tokBase)
	case rp.query.on:
		qp, qerr := cp.compileQuery(suffix, &rp, tokBase)
		if qerr != nil {
			return desc, qerr
		}
		desc.kind = aQuery
		desc.query = qp
		return desc, nil
	default:
		// bare "#": array length
		desc.kind = aCount
		return desc, nil
	}
}

// compileAlog compiles the "#.key" array-log descriptor.
func (cp *compiler) compileAlog(
	suffix, alogkey string, tokBase int,
) (desc arrayDesc, err error) {
	alogBase := tokBase + 2
	left, right, ok := splitPossiblePipe(alogkey)
	var ap *Plan
	if ok {
		ap, err = cp.compile(left, alogBase)
	} else {
		ap, err = cp.compile(alogkey, alogBase)
	}
	if err != nil {
		return desc, err
	}
	desc.kind = aAlog
	desc.alog = ap
	if ok {
		desc.apipe, err = cp.compile(right,
			tokBase+len(suffix)-len(right))
		if err != nil {
			return desc, err
		}
	}
	return desc, nil
}

// compileQuery compiles the "#(...)"/"#[...]" array descriptor.
func (cp *compiler) compileQuery(
	suffix string, rp *arrayPathResult, tokBase int,
) (*queryPlan, error) {
	// re-run parseQuery to obtain exact boundaries
	qpath, op, value, remain, fi, vesc, ok := parseQuery(suffix)
	if !ok {
		return nil, cp.err(tokBase, "unterminated query")
	}
	qp := &queryPlan{op: op, all: rp.query.all}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
		if vesc {
			value = unescape(value)
		}
	}
	qp.value = value

	// inner plan: executed against each candidate before comparison
	if qpath != "" {
		ipos := strings.Index(suffix[2:fi], qpath)
		ip, err := cp.compile(qpath, tokBase+2+ipos)
		if err != nil {
			return nil, err
		}
		qp.inner = ip
	}

	// remain starts right after the closing bracket
	remainBase := tokBase + fi
	rest := remain

	// a trailing "#" immediately after the bracket turns on gather-all
	if rp.query.all && len(rest) > 0 && rest[0] == '#' {
		rest = rest[1:]
		remainBase++
	}

	if len(rest) > 0 {
		switch rest[0] {
		case '|':
			pp, err := cp.compile(rest[1:], remainBase+1)
			if err != nil {
				return nil, err
			}
			qp.restP = pp
		case '.':
			left, right, hasPipe := splitPossiblePipe(rest[1:])
			if hasPipe {
				rp2, err := cp.compile(left, remainBase+1)
				if err != nil {
					return nil, err
				}
				pp, err := cp.compile(right,
					remainBase+1+len(rest[1:])-len(right))
				if err != nil {
					return nil, err
				}
				qp.rest = rp2
				qp.restP = pp
			} else {
				rp2, err := cp.compile(rest[1:], remainBase+1)
				if err != nil {
					return nil, err
				}
				qp.rest = rp2
			}
		}
	}
	return qp, nil
}

// compileStatic compiles a path beginning with '!'. The second return value
// is false when the path is not a recognized static form and should fall
// back to ordinary head compilation.
func (cp *compiler) compileStatic(
	path string, base int,
) (*Plan, bool, error) {
	name := path[1:]
	var pathOut string
	var res string
	if len(name) > 0 {
		switch name[0] {
		case '{', '[':
			end, perr := balancedEnd(name, 0)
			if perr != "" {
				return nil, false, cp.err(base+1, perr)
			}
			_, res = parseSquash(name, 0)
			pathOut = name[end:]
			return cp.finishStatic(path, res, pathOut, base, base+1+end)
		case '"':
			end, perr := balancedEnd(name, 0)
			if perr != "" {
				return nil, false, cp.err(base+1, perr)
			}
			res = name[:end]
			pathOut = name[end:]
			return cp.finishStatic(path, res, pathOut, base, base+1+end)
		case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			_, res = parseSquash(name, 0)
			pathOut = name[len(res):]
			return cp.finishStatic(path, res, pathOut, base,
				base+1+len(res))
		}
	}
	for i := 1; i < len(path); i++ {
		if path[i] == '|' {
			pathOut = path[i:]
			name = path[1:i]
			break
		}
		if path[i] == '.' {
			pathOut = path[i:]
			name = path[1:i]
			break
		}
	}
	switch strings.ToLower(name) {
	case "true", "false", "null", "nan", "inf":
		return cp.finishStatic(path, name, pathOut, base,
			base+1+len(name))
	}
	return nil, false, nil
}

// finishStatic builds the pStatic plan and compiles any trailing path.
func (cp *compiler) finishStatic(
	rawPath, res, pathOut string, base, resEnd int,
) (*Plan, bool, error) {
	p := &Plan{raw: rawPath, k: pStatic, staticVal: Parse(res)}
	if len(pathOut) > 0 && (pathOut[0] == '|' || pathOut[0] == '.') {
		after, err := cp.compile(pathOut[1:], resEnd+1)
		if err != nil {
			return nil, false, err
		}
		p.after = after
	}
	return p, true, nil
}

// compileModifier compiles a path beginning with '@'. The second return
// value is false when the leading token is not a registered modifier and
// the path should fall back to ordinary head compilation.
func (cp *compiler) compileModifier(
	path string, base int,
) (*Plan, bool, error) {
	name := path[1:]
	var pathOut string
	var hasArgs bool
	for i := 1; i < len(path); i++ {
		switch path[i] {
		case ':':
			pathOut = path[i+1:]
			name = path[1:i]
			hasArgs = len(pathOut) > 0
			goto found
		case '|':
			pathOut = path[i:]
			name = path[1:i]
			goto found
		case '.':
			pathOut = path[i:]
			name = path[1:i]
			goto found
		}
	}
found:
	fn, exists := modifiers[name]
	if !exists {
		return nil, false, nil
	}
	p := &Plan{raw: path, k: pMod, fn: fn}
	if hasArgs {
		var args string
		var parsedArgs bool
		switch pathOut[0] {
		case '{', '[':
			end, perr := balancedEnd(pathOut, 0)
			if perr != "" {
				return nil, false, cp.err(
					base+len(path)-len(pathOut),
					"unbalanced JSON modifier argument")
			}
			if res := Parse(pathOut); res.Exists() {
				args = pathOut[:end]
				pathOut = pathOut[end:]
				parsedArgs = true
			}
		case '"':
			end, perr := balancedEnd(pathOut, 0)
			if perr != "" {
				return nil, false, cp.err(
					base+len(path)-len(pathOut),
					"unbalanced string modifier argument")
			}
			if res := Parse(pathOut); res.Exists() {
				args = pathOut[:end]
				pathOut = pathOut[end:]
				parsedArgs = true
			}
		}
		if !parsedArgs {
			i := 0
			for ; i < len(pathOut); i++ {
				if pathOut[i] == '|' {
					break
				}
				switch pathOut[i] {
				case '{', '[', '"', '(':
					s := squash(pathOut[i:])
					i += len(s) - 1
				}
			}
			args = pathOut[:i]
			pathOut = pathOut[i:]
		}
		p.args = args
	}
	if len(pathOut) > 0 && (pathOut[0] == '|' || pathOut[0] == '.') {
		after, err := cp.compile(pathOut[1:],
			base+len(path)-len(pathOut)+1)
		if err != nil {
			return nil, false, err
		}
		p.after = after
	}
	return p, true, nil
}

// compileSubsel compiles a path beginning with '[' or '{'.
func (cp *compiler) compileSubsel(
	path string, base int,
) (*Plan, bool, error) {
	kind := path[0]
	subs, out, ok := parseSubSelectors(path)
	if !ok {
		return nil, false, cp.err(base, "unterminated subselector")
	}
	if len(out) > 0 && out[0] != '|' && out[0] != '.' {
		return nil, false, cp.err(base+len(path)-len(out),
			"unexpected text after subselector")
	}
	p := &Plan{raw: path, k: pSubsel, kind: kind}
	cursor := 1
	for _, sub := range subs {
		// sub.path is a substring of path; locate it by scanning
		spos, err := subPathOffset(path[cursor:], sub.path, sub.name)
		if err != nil {
			return nil, false, cp.err(base, "invalid subselector")
		}
		sp, err := cp.compile(sub.path, base+cursor+spos)
		if err != nil {
			return nil, false, err
		}
		p.subs = append(p.subs, compiledSub{plan: sp, name: sub.name})
		cursor += spos + subPathLen(sub)
	}
	if len(out) > 0 {
		after, err := cp.compile(out[1:], base+len(path)-len(out)+1)
		if err != nil {
			return nil, false, err
		}
		p.after = after
	}
	return p, true, nil
}

func subPathLen(sub subSelector) int {
	if sub.name != "" {
		return len(sub.name) + 1 + len(sub.path)
	}
	return len(sub.path)
}

// subPathOffset finds the byte offset of sub.path within the subselector
// body segment, tolerating whitespace.
func subPathOffset(seg, subPath, name string) (int, error) {
	if name != "" {
		// "name":path
		ni := strings.Index(seg, name)
		if ni < 0 {
			return 0, fmt.Errorf("name not found")
		}
		rest := seg[ni+len(name):]
		ci := strings.IndexByte(rest, ':')
		if ci < 0 {
			return 0, fmt.Errorf("colon not found")
		}
		pi := strings.Index(rest[ci+1:], subPath)
		if pi < 0 {
			return 0, fmt.Errorf("path not found")
		}
		return ni + ci + 1 + pi, nil
	}
	pi := strings.Index(seg, subPath)
	if pi < 0 {
		return 0, fmt.Errorf("path not found")
	}
	return pi, nil
}

// balancedEnd returns the end offset (exclusive) of a balanced JSON-like
// value starting at seg[start], which must be '[', '{', or '"'.
func balancedEnd(seg string, start int) (int, string) {
	switch seg[start] {
	case '"':
		i := start + 1
		for i < len(seg) {
			if seg[i] == '\\' {
				i += 2
				continue
			}
			if seg[i] == '"' {
				return i + 1, ""
			}
			i++
		}
		return len(seg), "unterminated string"
	case '[', '{':
		open := seg[start]
		close := byte(']')
		if open == '{' {
			close = '}'
		}
		depth := 0
		i := start
		for i < len(seg) {
			switch seg[i] {
			case '"':
				var perr string
				i, perr = balancedEnd(seg, i)
				if perr != "" {
					return len(seg), perr
				}
				continue
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return i + 1, ""
				}
			}
			i++
		}
		return len(seg), "unterminated JSON value"
	}
	return start, "not a balanced value"
}
