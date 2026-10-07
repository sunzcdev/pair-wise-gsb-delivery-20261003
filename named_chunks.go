package sqlx

import (
	"database/sql"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ErrUnsupportedChunkQuery is returned when a query cannot be split into
// per-row chunks.  Only statements with a single VALUES row template are
// supported, e.g. `INSERT ... VALUES (:a, :b)`.
var ErrUnsupportedChunkQuery = errors.New("sqlx: query does not contain a single, unambiguously parseable VALUES row template; chunked execution is not supported")

// ErrRowExceedsParamLimit is returned when binding a single row, together with
// the parameters outside of the VALUES row template, produces more
// parameters than the requested limit.
var ErrRowExceedsParamLimit = errors.New("sqlx: a single row already exceeds the parameter limit")

// ChunkExecResult reports the outcome of a chunked execution.
//
// Successful and Failed index the original input slice; a half-open interval
// [Start, End) identifies every element it covers.
type ChunkExecResult struct {
	// Successful is the half-open interval of input elements which have been
	// executed successfully.  When a later chunk fails, Successful stops just
	// before that chunk.
	Successful InputRange

	// Failed is the half-open interval of the chunk whose execution failed.
	// It has a zero value when every chunk succeeded.
	Failed InputRange
}

// InputRange is a half-open interval [Start, End) over the caller supplied
// input slice.
type InputRange struct {
	Start int
	End   int
}

// namedToken records the position and name of a named placeholder in a query.
type namedToken struct {
	start int
	end   int
	name  string
}

// scanNamedTokens collects the named placeholders of a query, following the
// same lexical rules as compileNamedQuery so that every emitted token has a
// corresponding bind variable.
func scanNamedTokens(qs []byte) ([]namedToken, error) {
	tokens := make([]namedToken, 0, 8)

	inName := false
	last := len(qs) - 1
	name := make([]byte, 0, 10)
	start := 0

	for i, b := range qs {
		if b == ':' {
			if inName && i > 0 && qs[i-1] == ':' {
				// this is the second ':' of a '::' escape, which is not a placeholder
				inName = false
				continue
			} else if inName {
				return nil, errors.New("unexpected `:` while reading named param at " + strconv.Itoa(i))
			}
			inName = true
			start = i
			name = name[:0]
		} else if inName && i > 0 && b == '=' && len(name) == 0 {
			// ':=' is not a placeholder
			inName = false
		} else if inName && (unicode.IsOneOf(allowedBindRunes, rune(b)) || b == '_' || b == '.') && i != last {
			name = append(name, b)
		} else if inName {
			inName = false
			if i == last && unicode.IsOneOf(allowedBindRunes, rune(b)) {
				name = append(name, b)
			}
			end := i
			if i == last && unicode.IsOneOf(allowedBindRunes, rune(b)) {
				end = i + 1
			}
			tokens = append(tokens, namedToken{start: start, end: end, name: string(name)})
		}
	}

	return tokens, nil
}

var chunkValuesReg = regexp.MustCompile(`(?i)VALUES\s*\(`)

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// findValuesRow locates the opening and matching closing parenthesis of the
// single VALUES row template in query.  It returns an error if there is not
// exactly one reliably identifiable VALUES row template.
func findValuesRow(query string) (open, close int, err error) {
	locs := chunkValuesReg.FindAllStringIndex(query, -1)

	var matched []int
	for _, loc := range locs {
		// the VALUES keyword must start on a token boundary, so that table or
		// column names ending in "values" are not mistaken for a clause
		if loc[0] > 0 && isIdentByte(query[loc[0]-1]) {
			continue
		}
		openParen := loc[1] - 1
		matchedClose := findMatchingClosingBracketIndex(query[openParen:])
		if matchedClose == 0 {
			continue
		}
		closeParen := openParen + matchedClose
		matched = append(matched, openParen, closeParen)
	}

	if len(matched) != 2 {
		return 0, 0, ErrUnsupportedChunkQuery
	}

	open, close = matched[0], matched[1]

	// A trailing comma immediately after the row template means the original
	// statement already declares several rows, which is not a single template.
	tail := strings.TrimLeft(query[close+1:], " \t\r\n")
	if strings.HasPrefix(tail, ",") {
		return 0, 0, ErrUnsupportedChunkQuery
	}

	return open, close, nil
}

// chunkPlan holds the compiled query fragments and placeholder names of a
// splittable statement.
type chunkPlan struct {
	prefixQ string
	rowQ    string
	suffixQ string
	prefixN []string
	rowN    []string
	suffixN []string
}

// buildChunkPlan parses a named query and splits it into the text before the
// VALUES row template, the template itself and the text following it.  Every
// fragment is compiled with the QUESTION bindvar; callers rebind the assembled
// query to their driver bindvar type.
func buildChunkPlan(query string) (*chunkPlan, error) {
	tokens, err := scanNamedTokens([]byte(query))
	if err != nil {
		return nil, err
	}

	open, close, err := findValuesRow(query)
	if err != nil {
		return nil, err
	}

	prefixSrc := query[:open+1]
	rowSrc := query[open+1 : close]
	suffixSrc := query[close:]

	prefixQ, prefixN, err := compileNamedQuery([]byte(prefixSrc), QUESTION)
	if err != nil {
		return nil, err
	}
	rowQ, rowN, err := compileNamedQuery([]byte(rowSrc), QUESTION)
	if err != nil {
		return nil, err
	}
	suffixQ, suffixN, err := compileNamedQuery([]byte(suffixSrc), QUESTION)
	if err != nil {
		return nil, err
	}

	var inRow, outRow int
	for _, tok := range tokens {
		if tok.start > open && tok.end <= close {
			inRow++
		} else {
			outRow++
		}
	}

	// The template must contain at least one named placeholder and the
	// compiler must agree with the scanner on how many placeholders exist.
	if inRow == 0 || inRow != len(rowN) || outRow != len(prefixN)+len(suffixN) {
		return nil, ErrUnsupportedChunkQuery
	}

	return &chunkPlan{
		prefixQ: prefixQ,
		rowQ:    rowQ,
		suffixQ: suffixQ,
		prefixN: prefixN,
		rowN:    rowN,
		suffixN: suffixN,
	}, nil
}

// rowsPerChunk returns how many rows fit within maxParams given the number of
// parameters contributed by a single row and by the fixed fragments.
func (p *chunkPlan) rowsPerChunk(maxParams int) (int, error) {
	fixed := len(p.prefixN) + len(p.suffixN)
	perRow := len(p.rowN)
	if fixed+perRow > maxParams {
		return 0, ErrRowExceedsParamLimit
	}
	return (maxParams - fixed) / perRow, nil
}

// buildChunkQuery assembles a QUESTION-bound statement covering rows copies of
// the row template.
func (p *chunkPlan) buildChunkQuery(rows int) string {
	var b strings.Builder
	b.WriteString(p.prefixQ)
	b.WriteString(p.rowQ)
	for i := 1; i < rows; i++ {
		b.WriteString(",(")
		b.WriteString(p.rowQ)
		b.WriteString(")")
	}
	b.WriteString(p.suffixQ)
	return b.String()
}

// namedExecChunks drives the chunked execution.  execf runs one assembled
// statement against either a DB or a Tx; for a Tx no commit is ever issued.
func namedExecChunks(e binder, query string, arg interface{}, maxParams int, fixed interface{}, execf func(query string, args []interface{}) (sql.Result, error)) (ChunkExecResult, error) {
	if maxParams <= 0 {
		return ChunkExecResult{}, errors.New("sqlx: maxParams must be greater than zero")
	}

	v := reflect.ValueOf(arg)
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
	default:
		return ChunkExecResult{}, errors.New("sqlx: chunked execution requires a slice or array argument")
	}

	total := v.Len()
	result := ChunkExecResult{}
	if total == 0 {
		// nothing to insert; do not execute a statement without rows
		return result, nil
	}

	plan, err := buildChunkPlan(query)
	if err != nil {
		return result, err
	}

	chunkRows, err := plan.rowsPerChunk(maxParams)
	if err != nil {
		return result, err
	}

	m := mapperFor(e)

	var fixedArgs []interface{}
	if fixed != nil {
		fixedNames := append(append([]string{}, plan.prefixN...), plan.suffixN...)
		fixedArgs, err = bindAnyArgs(fixedNames, fixed, m)
		if err != nil {
			return result, err
		}
	}

	bindType := BindType(e.DriverName())

	for start := 0; start < total; {
		rows := chunkRows
		if start+rows > total {
			rows = total - start
		}
		end := start + rows

		args := make([]interface{}, 0, len(plan.prefixN)+rows*len(plan.rowN)+len(plan.suffixN))

		if fixed != nil {
			args = append(args, fixedArgs[:len(plan.prefixN)]...)
		} else if len(plan.prefixN) > 0 {
			prefixArgs, berr := bindAnyArgs(plan.prefixN, v.Index(0).Interface(), m)
			if berr != nil {
				result.Successful = InputRange{Start: 0, End: start}
				result.Failed = InputRange{Start: start, End: end}
				return result, berr
			}
			args = append(args, prefixArgs...)
		}

		for i := start; i < end; i++ {
			rowArgs, berr := bindAnyArgs(plan.rowN, v.Index(i).Interface(), m)
			if berr != nil {
				result.Successful = InputRange{Start: 0, End: start}
				result.Failed = InputRange{Start: start, End: end}
				return result, berr
			}
			args = append(args, rowArgs...)
		}

		if fixed != nil {
			args = append(args, fixedArgs[len(plan.prefixN):]...)
		} else if len(plan.suffixN) > 0 {
			suffixArgs, berr := bindAnyArgs(plan.suffixN, v.Index(0).Interface(), m)
			if berr != nil {
				result.Successful = InputRange{Start: 0, End: start}
				result.Failed = InputRange{Start: start, End: end}
				return result, berr
			}
			args = append(args, suffixArgs...)
		}

		chunkQuery := plan.buildChunkQuery(rows)
		if bindType != QUESTION && bindType != UNKNOWN {
			chunkQuery = Rebind(bindType, chunkQuery)
		}

		if _, err := execf(chunkQuery, args); err != nil {
			result.Successful = InputRange{Start: 0, End: start}
			result.Failed = InputRange{Start: start, End: end}
			return result, err
		}

		start = end
	}

	result.Successful = InputRange{Start: 0, End: total}
	return result, nil
}

// NamedExecChunks executes a slice or array of structs or maps against a
// statement containing a single VALUES row template, splitting the input into
// chunks so that no single statement binds more than maxParams parameters.
//
// maxParams is an upper bound on the number of parameters in any executed
// statement: it counts every placeholder, including parameters outside of the
// VALUES row template and placeholders for a name that repeats within a row.
//
// Only statements whose VALUES clause contains a single, unambiguously
// parseable row template are supported; any other shape returns
// ErrUnsupportedChunkQuery.
//
// When e is a *Tx every chunk runs inside that transaction and sqlx never
// commits or rolls it back; transaction control stays with the caller.  When e
// is a *DB each chunk executes independently, so no whole-batch atomicity is
// guaranteed: on failure the returned ChunkExecResult reports the fully
// successful input interval and the failed chunk interval.
//
// fixed optionally supplies values for named parameters outside of the VALUES
// row template (for example a parameter in a trailing ON CONFLICT clause).
// When fixed is nil those parameters are resolved against the first input row.
func NamedExecChunks(e Ext, query string, arg interface{}, maxParams int, fixed ...interface{}) (ChunkExecResult, error) {
	var fixedArg interface{}
	switch len(fixed) {
	case 0:
	case 1:
		fixedArg = fixed[0]
	default:
		return ChunkExecResult{}, errors.New("sqlx: at most one fixed argument may be supplied")
	}

	return namedExecChunks(e, query, arg, maxParams, fixedArg, func(q string, args []interface{}) (sql.Result, error) {
		return e.Exec(q, args...)
	})
}

// NamedExecChunks using this DB.  Each chunk is executed independently, so a
// failure leaves previously executed chunks committed and the returned result
// identifies both the successful input interval and the failed chunk.
func (db *DB) NamedExecChunks(query string, arg interface{}, maxParams int, fixed ...interface{}) (ChunkExecResult, error) {
	return NamedExecChunks(db, query, arg, maxParams, fixed...)
}

// NamedExecChunks using this Tx.  Every chunk runs inside the transaction and
// the transaction is never committed or rolled back by this call.
func (tx *Tx) NamedExecChunks(query string, arg interface{}, maxParams int, fixed ...interface{}) (ChunkExecResult, error) {
	return NamedExecChunks(tx, query, arg, maxParams, fixed...)
}
