package gjson

import (
	"strconv"
)

// eval applies the compiled program to json.
func (p *program) eval(json string) Result {
	if p.cont != nil {
		return p.cont.run(json)
	}
	c := &parseContext{json: json}
	if p.recurse {
		// Mirrors Get: ".." enters parseArray at the start of the payload.
		execArray(c, 0, p.head)
	} else if p.head != nil {
		evalLevelInto(c, p.head)
	}
	if c.piped {
		if c.pipeP != nil {
			res := c.pipeP.eval(c.value.Raw)
			res.Index = 0
			return res
		}
		res := c.value.Get(c.pipe)
		res.Index = 0
		return res
	}
	fillIndex(json, c)
	return c.value
}

// evalLevelInto runs a plain structural level against the root container in
// c.json.
func evalLevelInto(c *parseContext, lv *level) {
	for i := 0; i < len(c.json); i++ {
		if c.json[i] == '{' {
			execObject(c, i+1, lv)
			return
		}
		if c.json[i] == '[' {
			execArray(c, i+1, lv)
			return
		}
	}
}

// evalLevel applies a plain structural level to a raw json value.
func evalLevel(json string, lv *level) Result {
	c := &parseContext{json: json}
	if lv != nil {
		evalLevelInto(c, lv)
	}
	if c.piped {
		if c.pipeP != nil {
			res := c.pipeP.eval(c.value.Raw)
			res.Index = 0
			return res
		}
		res := c.value.Get(c.pipe)
		res.Index = 0
		return res
	}
	fillIndex(json, c)
	return c.value
}

// getProgram runs prog against a result's raw value, mirroring Result.Get.
func (t Result) getProgram(prog *program) Result {
	r := prog.eval(t.Raw)
	if r.Indexes != nil {
		for i := 0; i < len(r.Indexes); i++ {
			r.Indexes[i] += t.Index
		}
	} else {
		r.Index += t.Index
	}
	return r
}

// getLevel applies a plain structural level to a result's raw value.
func (t Result) getLevel(lv *level) Result {
	r := evalLevel(t.Raw, lv)
	if r.Indexes != nil {
		for i := 0; i < len(r.Indexes); i++ {
			r.Indexes[i] += t.Index
		}
	} else {
		r.Index += t.Index
	}
	return r
}

// run executes a leading continuation (modifier, static literal, or
// subselector) against json.
func (ct *continuation) run(json string) Result {
	var res Result
	switch ct.kind {
	case contModifier:
		res = ct.runModifier(json)
	case contStatic:
		res = Parse(ct.static)
	case contSubselector:
		res = ct.runSubselector(json)
		res.Index = 0
	}
	if ct.next != nil {
		r := ct.next.eval(res.Raw)
		r.Index = 0
		if ct.kind != contSubselector {
			r.Indexes = nil
		}
		return r
	}
	if ct.kind != contSubselector {
		res.Indexes = nil
	}
	return res
}

func (ct *continuation) runModifier(json string) Result {
	return Parse(ct.modFn(json, ct.modArgs))
}

func (ct *continuation) runSubselector(json string) Result {
	var b []byte
	b = append(b, ct.subKind)
	var i int
	for _, sub := range ct.subs {
		res := sub.prog.eval(json)
		if res.Exists() {
			if i > 0 {
				b = append(b, ',')
			}
			if ct.subKind == '{' {
				if len(sub.name) > 0 {
					if sub.name[0] == '"' && Valid(sub.name) {
						b = append(b, sub.name...)
					} else {
						b = AppendJSONString(b, sub.name)
					}
				} else {
					last := nameOfLast(sub.prog.path)
					if isSimpleName(last) {
						b = AppendJSONString(b, last)
					} else {
						b = AppendJSONString(b, "_")
					}
				}
				b = append(b, ':')
			}
			var raw string
			if len(res.Raw) == 0 {
				raw = res.String()
				if len(raw) == 0 {
					raw = "null"
				}
			} else {
				raw = res.Raw
			}
			b = append(b, raw...)
			i++
		}
	}
	if ct.subKind == '{' {
		b = append(b, '}')
	} else {
		b = append(b, ']')
	}
	return Result{Raw: string(b), Type: JSON}
}

// execObject mirrors parseObject using a precomputed level.
func execObject(c *parseContext, i int, lv *level) (int, bool) {
	var pmatch, kesc, vesc, ok, hit bool
	var key, val string
	if lv.opipe != nil {
		c.pipeP = lv.opipe
		c.piped = true
	}
	for i < len(c.json) {
		for ; i < len(c.json); i++ {
			if c.json[i] == '"' {
				i++
				s := i
				for ; i < len(c.json); i++ {
					if c.json[i] > '\\' {
						continue
					}
					if c.json[i] == '"' {
						i, key, kesc, ok = i+1, c.json[s:i], false, true
						goto objKeyDone
					}
					if c.json[i] == '\\' {
						i++
						for ; i < len(c.json); i++ {
							if c.json[i] > '\\' {
								continue
							}
							if c.json[i] == '"' {
								if c.json[i-1] == '\\' {
									n := 0
									for j := i - 2; j > 0; j-- {
										if c.json[j] != '\\' {
											break
										}
										n++
									}
									if n%2 == 0 {
										continue
									}
								}
								i, key, kesc, ok = i+1, c.json[s:i], true, true
								goto objKeyDone
							}
						}
						key, kesc, ok = c.json[s:], false, false
					}
				}
			objKeyDone:
				break
			}
			if c.json[i] == '}' {
				return i + 1, false
			}
		}
		if !ok {
			return i, false
		}
		if lv.wild {
			if kesc {
				pmatch = matchLimit(unescape(key), lv.part)
			} else {
				pmatch = matchLimit(key, lv.part)
			}
		} else {
			if kesc {
				pmatch = lv.part == unescape(key)
			} else {
				pmatch = lv.part == key
			}
		}
		hit = pmatch && lv.onext == nil
		for ; i < len(c.json); i++ {
			var num bool
			switch c.json[i] {
			default:
				continue
			case '"':
				i++
				i, val, vesc, ok = parseString(c.json, i)
				if !ok {
					return i, false
				}
				if hit {
					if vesc {
						c.value.Str = unescape(val[1 : len(val)-1])
					} else {
						c.value.Str = val[1 : len(val)-1]
					}
					c.value.Raw = val
					c.value.Type = String
					return i, true
				}
			case '{':
				if pmatch && !hit {
					i, hit = execObject(c, i+1, lv.onext)
					if hit {
						return i, true
					}
				} else {
					i, val = parseSquash(c.json, i)
					if hit {
						c.value.Raw = val
						c.value.Type = JSON
						return i, true
					}
				}
			case '[':
				if pmatch && !hit {
					i, hit = execArray(c, i+1, lv.onext)
					if hit {
						return i, true
					}
				} else {
					i, val = parseSquash(c.json, i)
					if hit {
						c.value.Raw = val
						c.value.Type = JSON
						return i, true
					}
				}
			case 'n':
				if i+1 < len(c.json) && c.json[i+1] != 'u' {
					num = true
					break
				}
				fallthrough
			case 't', 'f':
				vc := c.json[i]
				i, val = parseLiteral(c.json, i)
				if hit {
					c.value.Raw = val
					switch vc {
					case 't':
						c.value.Type = True
					case 'f':
						c.value.Type = False
					}
					return i, true
				}
			case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
				'i', 'I', 'N':
				num = true
			}
			if num {
				i, val = parseNumber(c.json, i)
				if hit {
					c.value.Raw = val
					c.value.Type = Number
					c.value.Num, _ = strconv.ParseFloat(val, 64)
					return i, true
				}
			}
			break
		}
	}
	return i, false
}

func queryMatchesPlan(q *cQuery, value Result) bool {
	rpv := q.value
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
	if q.op == "" {
		return true
	}
	switch value.Type {
	case String:
		switch q.op {
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
		switch q.op {
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
		switch q.op {
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
		switch q.op {
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

// execArray mirrors parseArray using a precomputed level.
func execArray(c *parseContext, i int, lv *level) (int, bool) {
	var pmatch, vesc, ok, hit bool
	var val string
	var h int
	var alog []int
	var multires []byte
	var queryIndexes []int

	if lv.apipe != nil {
		c.pipeP = lv.apipe
		c.piped = true
	}

	q := lv.query

	procQuery := func(qval Result) bool {
		if q.all {
			if len(multires) == 0 {
				multires = append(multires, '[')
			}
		}
		var tmp parseContext
		tmp.value = qval
		fillIndex(c.json, &tmp)
		parentIndex := tmp.value.Index
		var res Result
		if qval.Type == JSON {
			if q.pathLv != nil {
				res = qval.getLevel(q.pathLv)
			} else {
				res = qval.getProgram(q.pathP)
			}
		} else {
			if q.hasPath {
				return false
			}
			res = qval
		}
		if queryMatchesPlan(q, res) {
			if q.pipeP != nil {
				c.pipeP = q.pipeP
				c.piped = true
			}
			if q.moreLv != nil {
				res = qval.getLevel(q.moreLv)
			} else if q.moreP != nil {
				if q.pipeP != nil {
					// already installed above
				}
				res = qval.getProgram(q.moreP)
			} else {
				res = qval
			}
			if q.all {
				raw := res.Raw
				if len(raw) == 0 {
					raw = res.String()
				}
				if raw != "" {
					if len(multires) > 1 {
						multires = append(multires, ',')
					}
					multires = append(multires, raw...)
					queryIndexes = append(queryIndexes, res.Index+parentIndex)
				}
			} else {
				c.value = res
				return true
			}
		}
		return false
	}

	for i < len(c.json)+1 {
		if !lv.arrch {
			pmatch = lv.isIndex && lv.index == h
			hit = pmatch && lv.anext == nil
		}
		h++
		if lv.alog {
			alog = append(alog, i)
		}
		for ; ; i++ {
			var ch byte
			if i > len(c.json) {
				break
			} else if i == len(c.json) {
				ch = ']'
			} else {
				ch = c.json[i]
			}
			var num bool
			switch ch {
			default:
				continue
			case '"':
				i++
				i, val, vesc, ok = parseString(c.json, i)
				if !ok {
					return i, false
				}
				if q != nil {
					var qval Result
					if vesc {
						qval.Str = unescape(val[1 : len(val)-1])
					} else {
						qval.Str = val[1 : len(val)-1]
					}
					qval.Raw = val
					qval.Type = String
					if procQuery(qval) {
						return i, true
					}
				} else if hit {
					if lv.alog {
						break
					}
					if vesc {
						c.value.Str = unescape(val[1 : len(val)-1])
					} else {
						c.value.Str = val[1 : len(val)-1]
					}
					c.value.Raw = val
					c.value.Type = String
					return i, true
				}
			case '{':
				if pmatch && !hit && q == nil {
					i, hit = execObject(c, i+1, lv.anext)
					if hit {
						if lv.alog {
							break
						}
						return i, true
					}
				} else {
					i, val = parseSquash(c.json, i)
					if q != nil {
						if procQuery(Result{Raw: val, Type: JSON}) {
							return i, true
						}
					} else if hit {
						if lv.alog {
							break
						}
						c.value.Raw = val
						c.value.Type = JSON
						return i, true
					}
				}
			case '[':
				if pmatch && !hit && q == nil {
					i, hit = execArray(c, i+1, lv.anext)
					if hit {
						if lv.alog {
							break
						}
						return i, true
					}
				} else {
					i, val = parseSquash(c.json, i)
					if q != nil {
						if procQuery(Result{Raw: val, Type: JSON}) {
							return i, true
						}
					} else if hit {
						if lv.alog {
							break
						}
						c.value.Raw = val
						c.value.Type = JSON
						return i, true
					}
				}
			case 'n':
				if i+1 < len(c.json) && c.json[i+1] != 'u' {
					num = true
					break
				}
				fallthrough
			case 't', 'f':
				vc := c.json[i]
				i, val = parseLiteral(c.json, i)
				if q != nil {
					var qval Result
					qval.Raw = val
					switch vc {
					case 't':
						qval.Type = True
					case 'f':
						qval.Type = False
					}
					if procQuery(qval) {
						return i, true
					}
				} else if hit {
					if lv.alog {
						break
					}
					c.value.Raw = val
					switch vc {
					case 't':
						c.value.Type = True
					case 'f':
						c.value.Type = False
					}
					return i, true
				}
			case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
				'i', 'I', 'N':
				num = true
			case ']':
				if lv.arrch && lv.part == "#" {
					if lv.alog {
						if lv.alogPipeP != nil {
							c.pipeP = lv.alogPipeP
							c.piped = true
						}
						var indexes = make([]int, 0, 64)
						var jsons = make([]byte, 0, 64)
						jsons = append(jsons, '[')
						k := 0
						for j := 0; j < len(alog); j++ {
							idx := alog[j]
							for idx < len(c.json) {
								switch c.json[idx] {
								case ' ', '\t', '\r', '\n':
									idx++
									continue
								}
								break
							}
							if idx < len(c.json) && c.json[idx] != ']' {
								_, ares, aok := parseAny(c.json, idx, true)
								if aok {
									res := ares.getProgram(lv.alogP)
									if res.Exists() {
										if k > 0 {
											jsons = append(jsons, ',')
										}
										raw := res.Raw
										if len(raw) == 0 {
											raw = res.String()
										}
										jsons = append(jsons, []byte(raw)...)
										indexes = append(indexes, res.Index)
										k++
									}
								}
							}
						}
						jsons = append(jsons, ']')
						c.value.Type = JSON
						c.value.Raw = string(jsons)
						c.value.Indexes = indexes
						return i + 1, true
					}

					c.value.Type = Number
					c.value.Num = float64(h - 1)
					c.value.Raw = strconv.Itoa(h - 1)
					c.calcd = true
					return i + 1, true
				}
				if !c.value.Exists() {
					if len(multires) > 0 {
						c.value = Result{
							Raw:     string(append(multires, ']')),
							Type:    JSON,
							Indexes: queryIndexes,
						}
					} else if q != nil && q.all {
						c.value = Result{
							Raw:  "[]",
							Type: JSON,
						}
					}
				}
				return i + 1, false
			}
			if num {
				i, val = parseNumber(c.json, i)
				if q != nil {
					var qval Result
					qval.Raw = val
					qval.Type = Number
					qval.Num, _ = strconv.ParseFloat(val, 64)
					if procQuery(qval) {
						return i, true
					}
				} else if hit {
					if lv.alog {
						break
					}
					c.value.Raw = val
					c.value.Type = Number
					c.value.Num, _ = strconv.ParseFloat(val, 64)
					return i, true
				}
			}
			break
		}
	}
	return i, false
}
