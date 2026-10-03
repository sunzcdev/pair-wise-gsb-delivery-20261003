package gjson

import (
	"strconv"
	"unsafe"
)

// QueryPlan is a reusable, read-only representation of a GJSON path.
//
// A QueryPlan is produced by Compile, which performs all of the path syntax
// processing up front. The same plan can then be applied to any number of JSON
// inputs through its Get and GetBytes methods. Because a QueryPlan never holds
// per-input state, a single plan may be shared between goroutines and reused
// indefinitely; a failed lookup or a missing value on one input has no effect
// on subsequent calls.
//
// Results returned from a plan are identical to those returned from Get (or
// GetBytes) for the same path and JSON.
type QueryPlan struct {
	prog *program
	path string
}

// PathError describes an invalid GJSON path encountered while compiling.
type PathError struct {
	// Path is the path that failed to compile.
	Path string
	// Offset is the zero-based byte position within Path where the syntax
	// problem was detected.
	Offset int
	// Msg describes the problem.
	Msg string
}

// Error implements the error interface.
func (e *PathError) Error() string {
	return "gjson: invalid path '" + e.Path + "' at position " +
		strconv.Itoa(e.Offset) + ": " + e.Msg
}

// program is a compiled path. Every program keeps the original absolute path
// so that error positions reported by sub-programs can be translated back into
// coordinates of the user supplied path.
type program struct {
	root    string // original path supplied to Compile
	path    string // sub-path represented by this program
	recurse bool   // path began with the ".." prefix
	head    *level // plain structural navigation
	cont    *continuation
}

type continuationKind uint8

const (
	contModifier continuationKind = iota
	contStatic
	contSubselector
)

type continuation struct {
	kind continuationKind
	next *program // suffix program (offset into the original path)
	// modifier
	modName string
	modArgs string
	modFn   func(json, arg string) string
	// static
	static string
	// subselector
	subKind byte // '[' or '{'
	subs    []subc
}

// subc is a compiled multipath component.
type subc struct {
	name string
	prog *program
}

// level is a precomputed result of parseObjectPath/parseArrayPath for a single
// structural navigation component. It carries two independent continuations so
// that a component can be interpreted as an object key or as an array index
// without rescanning the path.
type level struct {
	part  string
	wild  bool
	opipe *program // object interpretation pipe
	apipe *program // array interpretation pipe
	onext *level
	anext *level
	// array only
	arrch     bool // component contained a '#'
	isIndex   bool
	index     int
	alog      bool // "#." collect-all-elements form
	alogP     *program
	alogPipeP *program
	query     *cQuery
}

// cQuery is the compiled form of an array "#(...)" selector.
type cQuery struct {
	all     bool // trailing "#", collect all matches
	pathP   *program
	pathLv  *level
	hasPath bool
	op      string
	value   string
	moreP   *program // suffix inside element (may be modifier/multipath)
	moreLv  *level   // plain structural suffix (used when moreP has no prefix)
	pipeP   *program // pipe split out of the query suffix
}

// Compile parses path and returns a reusable QueryPlan.
//
// All path syntax, including escaped dots, strings inside queries, nested
// sub-selectors and modifiers with JSON arguments, is processed during the
// compile step. Invalid paths produce a *PathError whose Offset identifies the
// byte position of the problem.
func Compile(path string) (*QueryPlan, error) {
	pc := &planCompiler{root: path}
	prog, err := pc.compileProgram(path)
	if err != nil {
		return nil, err
	}
	return &QueryPlan{prog: prog, path: path}, nil
}

// MustCompile is like Compile but panics when the path is invalid.
func MustCompile(path string) *QueryPlan {
	p, err := Compile(path)
	if err != nil {
		panic(err)
	}
	return p
}

// Path returns the path the plan was compiled from.
func (p *QueryPlan) Path() string {
	if p == nil {
		return ""
	}
	return p.path
}

// Get applies the compiled path to json and returns the result. It is safe for
// concurrent use on different json inputs.
func (p *QueryPlan) Get(json string) Result {
	if p == nil {
		return Result{}
	}
	return p.prog.eval(json)
}

// GetBytes applies the compiled path to json and returns the result. It is safe
// for concurrent use on different json inputs.
func (p *QueryPlan) GetBytes(json []byte) Result {
	if json == nil {
		return Result{}
	}
	return evalBytes(json, p.prog.eval)
}

// planCompiler carries shared compilation state.
type planCompiler struct {
	root string
}

// subOffset translates a slice position back into a byte offset within root.
func (pc *planCompiler) subOffset(s string) int {
	if len(pc.root) == 0 {
		return 0
	}
	if len(s) == 0 {
		return len(pc.root)
	}
	sh := *(*stringHeader)(unsafe.Pointer(&s))
	rh := *(*stringHeader)(unsafe.Pointer(&pc.root))
	d := int(uintptr(sh.data) - uintptr(rh.data))
	if d < 0 || d > len(pc.root) {
		return 0
	}
	return d
}

func (pc *planCompiler) pathErr(s string, off int, msg string) *PathError {
	abs := pc.subOffset(s) + off
	if abs < 0 {
		abs = 0
	}
	if abs > len(pc.root) {
		abs = len(pc.root)
	}
	return &PathError{Path: pc.root, Offset: abs, Msg: msg}
}

// compileProgram compiles an entire path, handling any leading modifier/static
// prefixes and multipaths exactly the way Get does.
func (pc *planCompiler) compileProgram(path string) (*program, *PathError) {
	prog := &program{root: pc.root, path: path}
	cur := path
	if len(cur) > 1 {
		if (cur[0] == '@' && !DisableModifiers) || cur[0] == '!' {
			if cur[0] == '@' && !DisableModifiers {
				name, args, out, hasMod := parseModifierParts(cur)
				if hasMod {
					c := continuation{
						kind:    contModifier,
						modName: name,
						modArgs: args,
						modFn:   modifiers[name],
					}
					if len(out) > 0 && (out[0] == '|' || out[0] == '.') {
						sub, err := pc.compileProgram(out[1:])
						if err != nil {
							return nil, err
						}
						c.next = sub
					}
					prog.cont = &c
					return prog, nil
				}
			} else if cur[0] == '!' {
				out, res, ok := execStatic("", cur)
				if ok {
					c := continuation{kind: contStatic, static: res}
					if len(out) > 0 && (out[0] == '|' || out[0] == '.') {
						sub, err := pc.compileProgram(out[1:])
						if err != nil {
							return nil, err
						}
						c.next = sub
					}
					prog.cont = &c
					return prog, nil
				}
			}
		}
		if cur[0] == '[' || cur[0] == '{' {
			kind := cur[0]
			subs, out, ok := parseSubSelectors(cur)
			if !ok {
				off, msg := subselectorSyntaxError(cur)
				return nil, pc.pathErr(cur, off, msg)
			}
			if len(out) == 0 || out[0] == '|' || out[0] == '.' {
				c := continuation{kind: contSubselector, subKind: kind}
				for _, sub := range subs {
					sp, err := pc.compileProgram(sub.path)
					if err != nil {
						return nil, err
					}
					c.subs = append(c.subs, subc{name: sub.name, prog: sp})
				}
				if len(out) > 0 {
					sub, err := pc.compileProgram(out[1:])
					if err != nil {
						return nil, err
					}
					c.next = sub
				}
				prog.cont = &c
				return prog, nil
			}
			// Not a valid multipath position; fall through and treat the
			// bracket as structural text, matching Get.
		}
	}
	if len(path) >= 2 && path[0] == '.' && path[1] == '.' {
		prog.recurse = true
		lvl, err := pc.compileLevel(path[2:])
		if err != nil {
			return nil, err
		}
		prog.head = lvl
		return prog, nil
	}
	if len(path) > 0 {
		lvl, err := pc.compileLevel(path)
		if err != nil {
			return nil, err
		}
		prog.head = lvl
	}
	return prog, nil
}

// compileLevel builds the structural level tree for path. leadArray indicates
// whether the current component should be interpreted with the array grammar
// (the root component, or a component reached through an array).
func (pc *planCompiler) compileLevel(path string) (*level, *PathError) {
	op := parseObjectPath(path)
	lvl := &level{part: op.part, wild: op.wild}
	if op.piped {
		pp, err := pc.compileProgram(op.pipe)
		if err != nil {
			return nil, err
		}
		lvl.opipe = pp
	}
	if op.more {
		nl, err := pc.compileLevel(op.path)
		if err != nil {
			return nil, err
		}
		lvl.onext = nl
	}

	ap := parseArrayPath(path)
	lvl.arrch = ap.arrch
	if !ap.arrch {
		if n, ok := parseUint(ap.part); ok {
			lvl.isIndex = true
			lvl.index = int(n)
		}
	}
	if ap.piped {
		pp, err := pc.compileProgram(ap.pipe)
		if err != nil {
			return nil, err
		}
		lvl.apipe = pp
	}
	if ap.alogok {
		lvl.alog = true
		key := ap.alogkey
		if left, right, ok := splitPossiblePipe(key); ok {
			key = left
			pp, err := pc.compileProgram(right)
			if err != nil {
				return nil, err
			}
			lvl.alogPipeP = pp
		}
		pp, err := pc.compileProgram(key)
		if err != nil {
			return nil, err
		}
		lvl.alogP = pp
	}
	if ap.query.on {
		if perr := pc.compileQuery(path); perr != nil {
			return nil, perr
		}
		qp := &cQuery{all: ap.query.all}
		qpath, op2, value, _, _, vesc, qok := parseQuery(path)
		if qok {
			if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
				value = value[1 : len(value)-1]
				if vesc {
					value = unescape(value)
				}
			}
			qp.pathP, _ = pc.compileProgram(qpath)
			if qpath != "" {
				qp.pathLv, _ = pc.compileLevel(qpath)
			}
			qp.hasPath = qpath != ""
			qp.op = op2
			qp.value = value
		}
		if ap.more {
			suffix := ap.path
			if left, right, ok := splitPossiblePipe(suffix); ok {
				suffix = left
				pp, err := pc.compileProgram(right)
				if err != nil {
					return nil, err
				}
				qp.pipeP = pp
			}
			mp, merr := pc.compileProgram(suffix)
			if merr != nil {
				return nil, merr
			}
			qp.moreP = mp
			if mp.cont == nil {
				lv, lerr := pc.compileLevel(suffix)
				if lerr != nil {
					return nil, lerr
				}
				qp.moreLv = lv
			}
		}
		lvl.query = qp
	}
	if ap.more && !ap.query.on {
		nl, err := pc.compileLevel(ap.path)
		if err != nil {
			return nil, err
		}
		lvl.anext = nl
	}
	return lvl, nil
}

// compileQuery validates the leading "#(...)" or "#[...]" selector.
func (pc *planCompiler) compileQuery(path string) *PathError {
	off, msg := querySyntaxError(path)
	if off >= 0 {
		return pc.pathErr(path, off, msg)
	}
	return nil
}

// querySyntaxError returns the byte offset and message of the first syntax
// problem in a path beginning with "#(" or "#[", or -1 when balanced. It mirrors
// the tokenization performed by parseQuery.
func querySyntaxError(path string) (int, string) {
	if len(path) < 2 {
		return len(path), "unexpected end of path while parsing query"
	}
	var end byte
	switch path[1] {
	case '(':
		end = ')'
	case '[':
		end = ']'
	default:
		return -1, ""
	}
	depth := 1
	i := 2
	for ; i < len(path); i++ {
		switch path[i] {
		case '\\':
			i++
		case '"':
			start := i
			i++
			closed := false
			for ; i < len(path); i++ {
				if path[i] == '\\' {
					i++
					continue
				}
				if path[i] == '"' {
					closed = true
					break
				}
			}
			if !closed {
				return start, "unterminated string in query"
			}
		case '(', '[':
			depth++
		case ')', ']':
			depth--
			if depth == 0 {
				// parseQuery accepts either closing bracket at depth zero.
				return -1, ""
			}
		}
	}
	return len(path), "unclosed query, missing '" + string(end) + "'"
}

// subselectorSyntaxError returns the byte offset and message for a malformed
// leading multipath "[...]" or "{...}". It mirrors the tokenization performed
// by parseSubSelectors.
func subselectorSyntaxError(path string) (int, string) {
	var end byte
	switch path[0] {
	case '[':
		end = ']'
	case '{':
		end = '}'
	default:
		return len(path), "invalid subselector"
	}
	depth := 1
	for i := 1; i < len(path); i++ {
		switch path[i] {
		case '\\':
			i++
		case '"':
			start := i
			i++
			closed := false
			for ; i < len(path); i++ {
				switch path[i] {
				case '\\':
					i++
				case '"':
					closed = true
				}
				if closed {
					break
				}
			}
			if !closed {
				return start, "unterminated string in subselector"
			}
		case '[', '(', '{':
			depth++
		case ']', ')', '}':
			depth--
			if depth == 0 {
				// parseSubSelectors accepts any closing bracket at depth zero.
				return i, ""
			}
		}
	}
	return len(path), "unclosed subselector, missing '" + string(end) + "'"
}
