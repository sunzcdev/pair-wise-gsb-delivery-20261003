package gjson

import (
	"strconv"
	"unsafe"
)

func unsafeStringHeader(s string) unsafe.Pointer {
	return unsafe.Pointer(&s)
}

// planExec is the per-execution mutable state. A fresh one is allocated on
// every Plan.Get, which guarantees that concurrent executions and repeated
// calls never share or leak state.
type planExec struct {
	json  string
	value Result
	pipe  *Plan
	calcd bool
}

func fillIndexPlan(json string, e *planExec) {
	if len(e.value.Raw) > 0 && !e.calcd {
		jhdr := *(*stringHeader)(unsafeStringHeader(json))
		rhdr := *(*stringHeader)(unsafeStringHeader(e.value.Raw))
		e.value.Index = int(uintptr(rhdr.data) - uintptr(jhdr.data))
		if e.value.Index < 0 || e.value.Index >= len(json) {
			e.value.Index = 0
		}
	}
}

// exec dispatches on the plan kind. off is the offset of this plan's json
// within the outer input and is added to indexes (mirrors Result.Get).
func (p *Plan) exec(json string, e *planExec, off int) Result {
	switch p.k {
	case pHead:
		return p.execRoot(json, e, off)
	case pLines:
		return p.execLines(json, e, off)
	case pMod:
		return p.execMod(json, off)
	case pStatic:
		return p.execStatic(json, off)
	case pSubsel:
		return p.execSubsel(json, off)
	}
	return Result{}
}

// execResult runs a sub-plan as a standalone Result.Get would, adjusting
// indexes by the parent offset.
func (p *Plan) execResult(json string, off int) Result {
	r := p.exec(json, nil, 0)
	if r.Indexes != nil {
		for i := 0; i < len(r.Indexes); i++ {
			r.Indexes[i] += off
		}
	} else {
		r.Index += off
	}
	return r
}

func (p *Plan) execRoot(json string, outer *planExec, off int) Result {
	var e *planExec
	owned := outer == nil
	if owned {
		e = &planExec{json: json}
	} else {
		e = outer
	}
	for i := 0; i < len(json); i++ {
		if json[i] == '{' {
			execObjectPlan(e, i+1, p.steps, 0)
			break
		}
		if json[i] == '[' {
			execArrayPlan(e, i+1, p.steps, 0)
			break
		}
	}
	if !owned {
		return e.value
	}
	if e.pipe != nil {
		res := e.pipe.exec(e.value.Raw, nil, 0)
		res.Index = 0
		res.Indexes = nil
		return res
	}
	fillIndexPlan(json, e)
	if off != 0 {
		if e.value.Indexes != nil {
			for i := range e.value.Indexes {
				e.value.Indexes[i] += off
			}
		} else {
			e.value.Index += off
		}
	}
	return e.value
}

func (p *Plan) execLines(json string, outer *planExec, off int) Result {
	var e *planExec
	owned := outer == nil
	if owned {
		e = &planExec{json: json}
	} else {
		e = outer
	}
	execArrayPlan(e, 0, p.steps, 0)
	if !owned {
		return e.value
	}
	if e.pipe != nil {
		res := e.pipe.exec(e.value.Raw, nil, 0)
		res.Index = 0
		return res
	}
	fillIndexPlan(json, e)
	return e.value
}

func (p *Plan) execMod(json string, off int) Result {
	rjson := p.fn(json, p.args)
	if p.after != nil {
		res := p.after.exec(rjson, nil, 0)
		res.Index = 0
		res.Indexes = nil
		return res
	}
	res := Parse(rjson)
	res.Index = 0
	return res
}

func (p *Plan) execStatic(json string, off int) Result {
	res := p.staticVal
	if p.after != nil {
		r := p.after.exec(res.Raw, nil, 0)
		r.Index = 0
		return r
	}
	res.Index = 0
	return res
}

func (p *Plan) execSubsel(json string, off int) Result {
	var b []byte
	b = append(b, p.kind)
	var i int
	for _, sub := range p.subs {
		res := sub.plan.exec(json, nil, 0)
		if res.Exists() {
			if i > 0 {
				b = append(b, ',')
			}
			if p.kind == '{' {
				if len(sub.name) > 0 {
					if sub.name[0] == '"' && Valid(sub.name) {
						b = append(b, sub.name...)
					} else {
						b = AppendJSONString(b, sub.name)
					}
				} else {
					last := nameOfLast(sub.plan.raw)
					if isSimpleName(last) {
						b = AppendJSONString(b, last)
					} else {
						b = AppendJSONString(b, "_")
					}
				}
				b = append(b, ':')
			}
			raw := res.Raw
			if len(raw) == 0 {
				raw = res.String()
				if len(raw) == 0 {
					raw = "null"
				}
			}
			b = append(b, raw...)
			i++
		}
	}
	b = append(b, p.kind+2)
	var res Result
	res.Raw = string(b)
	res.Type = JSON
	if p.after != nil {
		res = p.after.exec(res.Raw, nil, 0)
	}
	res.Index = 0
	return res
}

// execObjectPlan mirrors parseObject driven by compiled steps.
func execObjectPlan(
	e *planExec, i int, steps []planStep, si int,
) (int, bool) {
	var pmatch, kesc, ok, hit bool
	var key, val string
	st := &steps[si]
	last := si == len(steps)-1
	if si == len(steps)-1 && st.pipe != nil {
		e.pipe = st.pipe
	}
	for i < len(e.json) {
		for ; i < len(e.json); i++ {
			if e.json[i] == '"' {
				i++
				s := i
				for ; i < len(e.json); i++ {
					if e.json[i] > '\\' {
						continue
					}
					if e.json[i] == '"' {
						i, key, kesc, ok = i+1, e.json[s:i], false, true
						goto parseKeyStringDone
					}
					if e.json[i] == '\\' {
						i++
						for ; i < len(e.json); i++ {
							if e.json[i] > '\\' {
								continue
							}
							if e.json[i] == '"' {
								if e.json[i-1] == '\\' {
									n := 0
									for j := i - 2; j > 0; j-- {
										if e.json[j] != '\\' {
											break
										}
										n++
									}
									if n%2 == 0 {
										continue
									}
								}
								i, key, kesc, ok = i+1, e.json[s:i], true, true
								goto parseKeyStringDone
							}
						}
						break
					}
				}
				key, kesc, ok = e.json[s:], false, false
			parseKeyStringDone:
				break
			}
			if e.json[i] == '}' {
				return i + 1, false
			}
		}
		if !ok {
			return i, false
		}
		if st.wild {
			if kesc {
				pmatch = matchLimit(unescape(key), st.part)
			} else {
				pmatch = matchLimit(key, st.part)
			}
		} else {
			if kesc {
				pmatch = st.part == unescape(key)
			} else {
				pmatch = st.part == key
			}
		}
		hit = pmatch && last
		more := pmatch && !last
		for ; i < len(e.json); i++ {
			var num bool
			switch e.json[i] {
			default:
				continue
			case '"':
				i++
				var vesc bool
				i, val, vesc, ok = parseString(e.json, i)
				if !ok {
					return i, false
				}
				if hit && st.pipe == nil {
					if vesc {
						e.value.Str = unescape(val[1 : len(val)-1])
					} else {
						e.value.Str = val[1 : len(val)-1]
					}
					e.value.Raw = val
					e.value.Type = String
					return i, true
				}
			case '{':
				if more {
					i, hit = execObjectPlan(e, i+1, steps, si+1)
					if hit {
						return i, true
					}
				} else {
					i, val = parseSquash(e.json, i)
					if hit {
						e.value.Raw = val
						e.value.Type = JSON
						return i, true
					}
				}
			case '[':
				if more {
					i, hit = execArrayPlan(e, i+1, steps, si+1)
					if hit {
						return i, true
					}
				} else {
					i, val = parseSquash(e.json, i)
					if hit {
						e.value.Raw = val
						e.value.Type = JSON
						return i, true
					}
				}
			case 'n':
				if i+1 < len(e.json) && e.json[i+1] != 'u' {
					num = true
					break
				}
				fallthrough
			case 't', 'f':
				vc := e.json[i]
				i, val = parseLiteral(e.json, i)
				if hit && st.pipe == nil {
					e.value.Raw = val
					switch vc {
					case 't':
						e.value.Type = True
					case 'f':
						e.value.Type = False
					}
					return i, true
				}
			case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
				'i', 'I', 'N':
				num = true
			}
			if num {
				i, val = parseNumber(e.json, i)
				if hit && st.pipe == nil {
					e.value.Raw = val
					e.value.Type = Number
					e.value.Num, _ = strconv.ParseFloat(val, 64)
					return i, true
				}
			}
			break
		}
	}
	return i, false
}

// execArrayPlan mirrors parseArray driven by compiled steps.
func execArrayPlan(
	e *planExec, i int, steps []planStep, si int,
) (int, bool) {
	var vesc, ok, hit bool
	var val string
	var h int
	var alog []int
	var multires []byte
	var queryIndexes []int
	st := &steps[si]
	last := si == len(steps)-1
	desc := st.arr

	if last && st.pipe != nil {
		e.pipe = st.pipe
	}

	// "##" style and other components that cannot match arrays: still honor
	// a terminal pipe set above, but never visit elements.
	if desc.kind == aDead {
		return i, false
	}

	pmatch := false

	procQuery := func(qp *queryPlan, qval Result) bool {
		if qp.all {
			if len(multires) == 0 {
				multires = append(multires, '[')
			}
		}
		var tmp planExec
		tmp.value = qval
		fillIndexPlan(e.json, &tmp)
		parentIndex := tmp.value.Index
		var res Result
		if qval.Type == JSON {
			if qp.inner != nil {
				res = qp.inner.exec(qval.Raw, nil, 0)
			} else {
				res = qval
			}
		} else {
			if qp.inner != nil {
				return false
			}
			res = qval
		}
		if queryMatchesPlan(qp, res) {
			if qp.rest != nil || qp.restP != nil {
				if qp.restP != nil {
					e.pipe = qp.restP
				}
				if qp.rest != nil {
					res = qp.rest.exec(qval.Raw, nil, 0)
				} else {
					res = qval
				}
			} else {
				res = qval
			}
			if qp.all {
				raw := res.Raw
				if len(raw) == 0 {
					raw = res.String()
				}
				if raw != "" {
					if len(multires) > 1 {
						multires = append(multires, ',')
					}
					multires = append(multires, raw...)
					queryIndexes = append(queryIndexes,
						res.Index+parentIndex)
				}
			} else {
				e.value = res
				return true
			}
		}
		return false
	}

	for i < len(e.json)+1 {
		if desc.kind == aPlain {
			pmatch = desc.idx == h
			hit = pmatch && last
		}
		h++
		if desc.kind == aAlog {
			alog = append(alog, i)
		}
		for ; ; i++ {
			var ch byte
			if i > len(e.json) {
				break
			} else if i == len(e.json) {
				ch = ']'
			} else {
				ch = e.json[i]
			}
			var num bool
			switch ch {
			default:
				continue
			case '"':
				i++
				i, val, vesc, ok = parseString(e.json, i)
				if !ok {
					return i, false
				}
				if desc.kind == aQuery {
					var qval Result
					if vesc {
						qval.Str = unescape(val[1 : len(val)-1])
					} else {
						qval.Str = val[1 : len(val)-1]
					}
					qval.Raw = val
					qval.Type = String
					if procQuery(desc.query, qval) {
						return i, true
					}
				} else if hit && st.pipe == nil {
					e.value.Str = unescapeOr(val, vesc)
					e.value.Raw = val
					e.value.Type = String
					return i, true
				}
			case '{':
				if pmatch && !hit && last == false {
					i, hit = execObjectPlan(e, i+1, steps, si+1)
					if hit {
						return i, true
					}
				} else {
					i, val = parseSquash(e.json, i)
					if desc.kind == aQuery {
						if procQuery(desc.query,
							Result{Raw: val, Type: JSON}) {
							return i, true
						}
					} else if hit && st.pipe == nil {
						e.value.Raw = val
						e.value.Type = JSON
						return i, true
					}
				}
			case '[':
				if pmatch && !hit && last == false {
					i, hit = execArrayPlan(e, i+1, steps, si+1)
					if hit {
						return i, true
					}
				} else {
					i, val = parseSquash(e.json, i)
					if desc.kind == aQuery {
						if procQuery(desc.query,
							Result{Raw: val, Type: JSON}) {
							return i, true
						}
					} else if hit && st.pipe == nil {
						e.value.Raw = val
						e.value.Type = JSON
						return i, true
					}
				}
			case 'n':
				if i+1 < len(e.json) && e.json[i+1] != 'u' {
					num = true
					break
				}
				fallthrough
			case 't', 'f':
				vc := e.json[i]
				i, val = parseLiteral(e.json, i)
				if desc.kind == aQuery {
					var qval Result
					qval.Raw = val
					switch vc {
					case 't':
						qval.Type = True
					case 'f':
						qval.Type = False
					}
					if procQuery(desc.query, qval) {
						return i, true
					}
				} else if hit && st.pipe == nil {
					e.value.Raw = val
					switch vc {
					case 't':
						e.value.Type = True
					case 'f':
						e.value.Type = False
					}
					return i, true
				}
			case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
				'i', 'I', 'N':
				num = true
			case ']':
				if desc.kind == aCount {
					e.value.Type = Number
					e.value.Num = float64(h - 1)
					e.value.Raw = strconv.Itoa(h - 1)
					e.calcd = true
					return i + 1, true
				}
				if desc.kind == aAlog {
					return finishAlog(e, desc, alog), true
				}
				if !e.value.Exists() {
					if len(multires) > 0 {
						e.value = Result{
							Raw:     string(append(multires, ']')),
							Type:    JSON,
							Indexes: queryIndexes,
						}
					} else if desc.kind == aQuery && desc.query.all {
						e.value = Result{Raw: "[]", Type: JSON}
					}
				}
				return i + 1, false
			}
			if num {
				i, val = parseNumber(e.json, i)
				if desc.kind == aQuery {
					var qval Result
					qval.Raw = val
					qval.Type = Number
					qval.Num, _ = strconv.ParseFloat(val, 64)
					if procQuery(desc.query, qval) {
						return i, true
					}
				} else if hit && st.pipe == nil {
					e.value.Raw = val
					e.value.Type = Number
					e.value.Num, _ = strconv.ParseFloat(val, 64)
					return i, true
				}
			}
			break
		}
	}
	return i, false
}

func unescapeOr(val string, vesc bool) string {
	if vesc {
		return unescape(val[1 : len(val)-1])
	}
	return val[1 : len(val)-1]
}

// finishAlog gathers the "#.key" results, mirroring the ']' handling of the
// array-log branch in parseArray.
func finishAlog(e *planExec, desc arrayDesc, alog []int) int {
	if desc.apipe != nil {
		left, right, ok := splitPossiblePipe(desc.alog.raw)
		_ = left
		_ = right
		_ = ok
		e.pipe = desc.apipe
	}
	indexes := make([]int, 0, 64)
	jsons := make([]byte, 0, 64)
	jsons = append(jsons, '[')
	for j, k := 0, 0; j < len(alog); j++ {
		idx := alog[j]
		for idx < len(e.json) {
			switch e.json[idx] {
			case ' ', '\t', '\r', '\n':
				idx++
				continue
			}
			break
		}
		if idx < len(e.json) && e.json[idx] != ']' {
			_, res, ok := parseAny(e.json, idx, true)
			if ok {
				r := desc.alog.exec(res.Raw, nil, res.Index)
				if r.Exists() {
					if k > 0 {
						jsons = append(jsons, ',')
					}
					raw := r.Raw
					if len(raw) == 0 {
						raw = r.String()
					}
					jsons = append(jsons, []byte(raw)...)
					indexes = append(indexes, r.Index)
					k++
				}
			}
		}
	}
	jsons = append(jsons, ']')
	e.value.Type = JSON
	e.value.Raw = string(jsons)
	e.value.Indexes = indexes
	// closing ']' position: alog loop ends when i == len(json); caller adds 1
	return len(e.json) + 1
}

// queryMatchesPlan mirrors queryMatches using compiled query data.
func queryMatchesPlan(qp *queryPlan, value Result) bool {
	rpv := qp.value
	if len(rpv) > 0 && rpv[0] == '~' {
		rpv = rpv[1:]
		var ish, ok bool
		switch rpv {
		case "*":
			ish, ok = value.Exists(), true
		case "null":
			ish, ok = nullish(value), true
		case "true":
			ish, ok = trueish(value), true
		case "false":
			ish, ok = falseish(value), true
		}
		if ok {
			rpv = "true"
			if ish {
				value = Result{Type: True}
			} else {
				value = Result{Type: False}
			}
		} else {
			rpv = ""
			value = Result{}
		}
	}
	if !value.Exists() {
		return false
	}
	if qp.op == "" {
		return true
	}
	switch value.Type {
	case String:
		switch qp.op {
		case "=":
			return value.Str == rpv
		case "!=":
			return value.Str != rpv
		case "<":
			return value.Str < rpv
		case "<=":
			return value.Str <= rpv
		case ">":
			return value.Str > rpv
		case ">=":
			return value.Str >= rpv
		case "%":
			return matchLimit(value.Str, rpv)
		case "!%":
			return !matchLimit(value.Str, rpv)
		}
	case Number:
		rpvn, _ := strconv.ParseFloat(rpv, 64)
		switch qp.op {
		case "=":
			return value.Num == rpvn
		case "!=":
			return value.Num != rpvn
		case "<":
			return value.Num < rpvn
		case "<=":
			return value.Num <= rpvn
		case ">":
			return value.Num > rpvn
		case ">=":
			return value.Num >= rpvn
		}
	case True:
		switch qp.op {
		case "=":
			return rpv == "true"
		case "!=":
			return rpv != "true"
		case ">":
			return rpv == "false"
		case ">=":
			return true
		}
	case False:
		switch qp.op {
		case "=":
			return rpv == "false"
		case "!=":
			return rpv != "false"
		case "<":
			return rpv == "true"
		case "<=":
			return true
		}
	}
	return false
}
