package gojq

import (
	"context"
	"math"
	"reflect"
	"slices"
	"strings"
)

func (env *env) execute(bc *Code, v any, vars ...any) Iter {
	env.code = bc
	env.codes = bc.codes
	env.codeinfos = bc.codeinfos
	env.stackTrace = bc.funcFrames != nil
	env.push(v)
	for i := len(vars) - 1; i >= 0; i-- {
		env.push(vars[i])
	}
	env.debugCodes()
	return env
}

func (env *env) Next() (any, bool) {
	var err error
	pc, callpc, index := env.pc, len(env.codes)-1, -1
	backtrack, hasCtx := env.backtrack, env.ctx != context.Background()
	tail := false
	defer func() { env.pc, env.backtrack = pc, true }()
loop:
	for ; pc < len(env.codes); pc++ {
		env.debugState(pc, backtrack)
		code := env.codes[pc]
		if hasCtx {
			select {
			case <-env.ctx.Done():
				pc, env.forks = len(env.codes), nil
				return env.ctx.Err(), true
			default:
			}
		}
		switch code.op {
		case opnop:
			// nop
		case oppush:
			env.push(code.v)
		case oppop:
			env.pop()
		case opdup:
			v := env.pop()
			env.push(v)
			env.push(v)
		case opconst:
			env.pop()
			env.push(code.v)
		case opload:
			env.push(env.values[env.index(code.v.([2]int))])
		case opstore:
			env.values[env.index(code.v.([2]int))] = env.pop()
		case opobject:
			if backtrack {
				break loop
			}
			n := code.v.(int)
			m := make(map[string]any, n)
			for range n {
				v, k := env.pop(), env.pop()
				s, ok := k.(string)
				if !ok {
					err = &objectKeyNotStringError{k}
					break loop
				}
				if _, ok := m[s]; !ok {
					m[s] = v
				}
			}
			env.push(m)
		case opappend:
			i := env.index(code.v.([2]int))
			env.values[i] = append(env.values[i].([]any), env.pop())
		case opfork:
			if backtrack {
				if err != nil {
					break loop
				}
				pc, backtrack = code.v.(int), false
				goto loop
			}
			env.pushfork(pc)
		case opforktrybegin:
			if backtrack {
				switch e := err.(type) {
				case *tryEndError:
					err = e.err
					break loop
				case nil, *breakError, *HaltError:
					break loop
				case ValueError:
					env.pop()
					env.push(e.Value())
				default:
					env.pop()
					env.push(err.Error())
				}
				env.goFrame = nil
				pc, backtrack, err = code.v.(int), false, nil
				goto loop
			}
			env.pushfork(pc)
		case opforktryend:
			if backtrack {
				if err != nil {
					err = &tryEndError{err}
				}
				break loop
			}
			env.pushfork(pc)
		case opforkalt:
			if backtrack {
				switch err.(type) {
				case nil, *breakError, *HaltError:
					break loop
				}
				env.goFrame = nil
				pc, backtrack, err = code.v.(int), false, nil
				goto loop
			}
			env.pushfork(pc)
		case opforklabel:
			if backtrack {
				label := env.pop()
				if e, ok := err.(*breakError); ok && e.v == label {
					err = nil
					env.goFrame = nil
				}
				break loop
			}
			env.push(env.label)
			env.pushfork(pc)
			env.pop()
			env.values[env.index(code.v.([2]int))] = env.label
			env.label++
		case opbacktrack:
			break loop
		case opjump:
			pc = code.v.(int)
			goto loop
		case opjumprec:
			// Tail self-call compiled as a jump (see optimizeTailRec) when
			// stack frames are enabled. It behaves exactly like opjump, but
			// the frame of the current scope is marked as a (compressed)
			// tail call so the trace reports one frame instead of growing.
			if env.stackTrace && env.scopes.index >= 0 {
				env.scopes.data[env.scopes.index].value.tail = true
			}
			pc = code.v.(int)
			goto loop
		case opjumpifnot:
			if v := env.pop(); v == nil || v == false {
				pc = code.v.(int)
				goto loop
			}
		case opindex, opindexarray:
			if backtrack {
				break loop
			}
			p, v := code.v, env.pop()
			if code.op == opindexarray && v != nil {
				if _, ok := v.([]any); !ok {
					err = &expectedArrayError{v}
					break loop
				}
			}
			w := funcIndex2(nil, v, p)
			if e, ok := w.(error); ok {
				err = e
				break loop
			}
			env.push(w)
			if !env.paths.empty() && env.expdepth == 0 {
				if !env.pathIntact(v) {
					err = &invalidPathError{v}
					break loop
				}
				env.paths.push(pathValue{path: p, value: w})
			}
		case opcall:
			if backtrack {
				break loop
			}
			switch v := code.v.(type) {
			case int:
				pc, callpc, index = v, pc, env.scopes.index
				tail = false
				goto loop
			case [3]any:
				argcnt := v[1].(int)
				x, args := env.pop(), env.args[:argcnt]
				for i := range argcnt {
					args[i] = env.pop()
				}
				env.goFrame = env.goCallFrame(v[2].(string), pc)
				w := v[0].(func(any, []any) any)(x, args)
				if e, ok := w.(error); ok {
					err = e
					break loop
				}
				env.goFrame = nil
				env.push(w)
				if !env.paths.empty() && env.expdepth == 0 {
					switch v[2].(string) {
					case "_index":
						if x = args[0]; !env.pathIntact(x) {
							err = &invalidPathError{x}
							break loop
						}
						env.paths.push(pathValue{path: args[1], value: w})
					case "_slice":
						if x = args[0]; !env.pathIntact(x) {
							err = &invalidPathError{x}
							break loop
						}
						env.paths.push(pathValue{
							path:  map[string]any{"start": args[2], "end": args[1]},
							value: w,
						})
					case "getpath":
						if !env.pathIntact(x) {
							err = &invalidPathError{x}
							break loop
						}
						for _, p := range args[0].([]any) {
							env.paths.push(pathValue{path: p, value: w})
						}
					}
				}
			default:
				panic(v)
			}
		case opcallrec:
			pc, callpc, index = code.v.(int), -1, env.scopes.index
			tail = true
			goto loop
		case oppushpc:
			env.push([2]int{code.v.(int), env.scopes.index})
		case opcallpc:
			xs := env.pop().([2]int)
			pc, callpc, index = xs[0], pc, xs[1]
			tail = false
			goto loop
		case opscope:
			xs := code.v.([3]int)
			var saveindex, outerindex int
			if index == env.scopes.index {
				if callpc >= 0 {
					saveindex = index
				} else {
					callpc, saveindex = env.popscope()
				}
			} else {
				saveindex = env.scopes.index
			}
			tailEntry := tail
			tail = false
			if outerindex = index; outerindex >= 0 {
				if s := env.scopes.data[outerindex].value; s.id == xs[0] {
					outerindex = s.outerindex
				}
			}
			env.scopes.push(scope{xs[0], env.offset, callpc, saveindex, outerindex, tailEntry})
			env.offset += xs[1]
			if env.offset > len(env.values) {
				vs := make([]any, env.offset*2)
				copy(vs, env.values)
				env.values = vs
			}
		case opret:
			if backtrack {
				break loop
			}
			pc, env.scopes.index = env.popscope()
			if env.scopes.empty() {
				return env.pop(), true
			}
		case opiter:
			if err != nil {
				break loop
			}
			backtrack = false
			var xs []pathValue
			switch v := env.pop().(type) {
			case []pathValue:
				xs = v
			case []any:
				if !env.paths.empty() && env.expdepth == 0 && !env.pathIntact(v) {
					err = &invalidPathIterError{v}
					env.push(emptyIter{})
					break loop
				}
				if len(v) == 0 {
					break loop
				}
				xs = make([]pathValue, len(v))
				for i, v := range v {
					xs[i] = pathValue{path: i, value: v}
				}
			case map[string]any:
				if !env.paths.empty() && env.expdepth == 0 && !env.pathIntact(v) {
					err = &invalidPathIterError{v}
					env.push(emptyIter{})
					break loop
				}
				if len(v) == 0 {
					break loop
				}
				xs = make([]pathValue, len(v))
				var i int
				for k, v := range v {
					xs[i] = pathValue{path: k, value: v}
					i++
				}
				slices.SortFunc(xs, func(x, y pathValue) int {
					return strings.Compare(x.path.(string), y.path.(string))
				})
			case Iter:
				if w, ok := v.Next(); ok {
					env.push(v)
					env.pushfork(pc)
					env.pop()
					if e, ok := w.(error); ok {
						err = e
						if env.stackTrace {
							env.goFrame = env.goCallFrame("", pc)
						}
						break loop
					}
					env.push(w)
					continue
				}
				break loop
			default:
				err = &iteratorError{v}
				env.push(emptyIter{})
				break loop
			}
			if len(xs) > 1 {
				env.push(xs[1:])
				env.pushfork(pc)
				env.pop()
			}
			env.push(xs[0].value)
			if !env.paths.empty() && env.expdepth == 0 {
				env.paths.push(xs[0])
			}
		case opexpbegin:
			env.expdepth++
		case opexpend:
			env.expdepth--
		case oppathbegin:
			env.paths.push(env.expdepth)
			env.paths.push(pathValue{value: env.stack.top()})
			env.expdepth = 0
		case oppathend:
			if backtrack {
				break loop
			}
			env.pop()
			if v := env.pop(); !env.pathIntact(v) {
				err = &invalidPathError{v}
				break loop
			}
			env.push(env.poppaths())
			env.expdepth = env.paths.pop().(int)
		default:
			panic(code.op)
		}
	}
	if len(env.forks) > 0 {
		if err == nil {
			env.goFrame = nil
		}
		pc, backtrack = env.popfork(), true
		goto loop
	}
	if err != nil {
		return env.withStackFrames(err), true
	}
	return nil, false
}

// withStackFrames attaches the current structured stack trace to a terminal
// error. Errors caught by try-catch never reach here; they are unwrapped by
// opforktrybegin and turned into plain jq values, so the frames stay invisible
// inside the language. Halt errors keep their concrete type (they are not
// catchable) but carry the same frames.
func (env *env) withStackFrames(err error) error {
	if !env.stackTrace {
		return err
	}
	if _, ok := err.(*tryEndError); ok {
		return err
	}
	frames := env.buildStackFrames()
	if env.goFrame != nil {
		frames = append([]stackFrame{*env.goFrame}, frames...)
	}
	if len(frames) == 0 {
		return err
	}
	if halt, ok := err.(*HaltError); ok {
		halt.frames = frames
		halt.traceCode = env.code
		return halt
	}
	if _, ok := err.(StackTracer); ok {
		return err
	}
	return &stackError{err: err, code: env.code, frames: frames}
}

// goCallFrame builds the innermost frame for a function implemented in Go (a
// built-in or a function registered with WithFunction/WithIterFunction). Such
// functions have no jq definition range; only the call site is reported.
func (env *env) goCallFrame(name string, pc int) *stackFrame {
	if !env.stackTrace {
		return nil
	}
	frame := stackFrame{kind: FrameGoFunc, function: name, defStart: -1}
	if info, ok := env.code.callInfos[pc]; ok {
		info := info
		if frame.function == "" {
			frame.function = info.function
		}
		frame.call = &info
	}
	return &frame
}

// buildStackFrames walks the active scope chain (innermost first) and resolves
// each frame against the compiler metadata. A tail-recursive scope carries the
// TailCall marker; opjumprec keeps a single scope, so the list is bounded.
func (env *env) buildStackFrames() []stackFrame {
	if env.code == nil || env.code.funcFrames == nil {
		return nil
	}
	var frames []stackFrame
	for i := env.scopes.index; i >= 0; i = env.scopes.data[i].value.saveindex {
		s := env.scopes.data[i].value
		meta, ok := env.code.funcFrames[s.id]
		if !ok {
			continue
		}
		frame := stackFrame{
			kind:     meta.kind,
			module:   meta.module,
			function: meta.function,
			defStart: meta.defStart,
			defEnd:   meta.defEnd,
			tailCall: s.tail,
		}
		if s.pc >= 0 {
			if info, ok := env.code.callInfos[s.pc]; ok {
				info := info
				frame.call = &info
			}
		}
		frames = append(frames, frame)
	}
	return frames
}

func (env *env) push(v any) {
	env.stack.push(v)
}

func (env *env) pop() any {
	return env.stack.pop()
}

func (env *env) popscope() (int, int) {
	free := env.scopes.index > env.scopes.limit
	s := env.scopes.pop()
	if free {
		env.offset = s.offset
	}
	return s.pc, s.saveindex
}

func (env *env) pushfork(pc int) {
	f := fork{pc: pc, offset: env.offset, expdepth: env.expdepth}
	f.stackindex, f.stacklimit = env.stack.save()
	f.scopeindex, f.scopelimit = env.scopes.save()
	f.pathindex, f.pathlimit = env.paths.save()
	env.forks = append(env.forks, f)
	env.debugForks(pc, ">>>")
}

func (env *env) popfork() int {
	f := env.forks[len(env.forks)-1]
	env.debugForks(f.pc, "<<<")
	env.forks, env.offset, env.expdepth =
		env.forks[:len(env.forks)-1], f.offset, f.expdepth
	env.stack.restore(f.stackindex, f.stacklimit)
	env.scopes.restore(f.scopeindex, f.scopelimit)
	env.paths.restore(f.pathindex, f.pathlimit)
	return f.pc
}

func (env *env) index(v [2]int) int {
	for id, i := v[0], env.scopes.index; i >= 0; {
		s := env.scopes.data[i].value
		if s.id == id {
			return s.offset + v[1]
		}
		i = s.outerindex
	}
	panic("env.index")
}

type pathValue struct {
	path, value any
}

func (env *env) pathIntact(v any) bool {
	w := env.paths.top().(pathValue).value
	switch v := v.(type) {
	case []any, map[string]any:
		switch w.(type) {
		case []any, map[string]any:
			v, w := reflect.ValueOf(v), reflect.ValueOf(w)
			return v.Pointer() == w.Pointer() && v.Len() == w.Len()
		}
	case float64:
		if w, ok := w.(float64); ok {
			return v == w || math.IsNaN(v) && math.IsNaN(w)
		}
	}
	return v == w
}

func (env *env) poppaths() []any {
	xs := []any{}
	for {
		p := env.paths.pop().(pathValue)
		if p.path == nil {
			break
		}
		xs = append(xs, p.path)
	}
	slices.Reverse(xs)
	return xs
}
