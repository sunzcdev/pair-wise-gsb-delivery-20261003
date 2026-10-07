package msgpack

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vmihailenco/msgpack/v5/msgpcode"
)

// QueryError is returned when a CompiledQuery fails. Path reports the query
// path that was being processed when the error occurred, e.g.
// "records[?status==failed][3].elapsed".
type QueryError struct {
	Query string
	Path  string
	Err   error
}

func (e *QueryError) Error() string {
	return fmt.Sprintf("msgpack: query %q: error at %s: %s", e.Query, e.Path, e.Err)
}

func (e *QueryError) Unwrap() error {
	return e.Err
}

//------------------------------------------------------------------------------

// CompiledQuery is a parsed query that can be reused to extract data from
// any number of msgpack streams. Use CompileQuery to create one.
type CompiledQuery struct {
	query string
	root  queryNode
}

// CompileQuery parses query into a reusable CompiledQuery.
//
// The query is executed directly against the msgpack stream: unselected
// subtrees are skipped using the decoder's fixed-size buffer and are never
// decoded into interface{}, so memory usage stays bounded regardless of how
// large the skipped values are.
//
// The syntax extends Decoder.Query:
//   - key                 map key
//   - 10 or [10]          array index
//   - [1:10]              array slice; either bound may be omitted, e.g. [:10]
//   - *                   all array elements
//   - [?field==value]     keep only array elements (maps) whose field compares
//     equal to value; != is supported as well and value may be
//     a (optionally quoted) string, a number, true, false, or nil
//   - [?field==value][:10]  keep at most 10 matching elements
//   - {key1,key2.nested}  extract multiple paths from the same map in one pass
//
// For example:
//
//	records[?status==failed][:10].{id,elapsed}
//
// selects id and elapsed of the first 10 records with status failed.
//
// Buffering: when the predicate field appears in the stream after the output
// fields, the selected values of the record being processed are buffered
// until the record is fully consumed and the predicate is known. Buffering
// never extends beyond a single array element and only ever holds the
// selected output values of that one element, so it stays bounded no matter
// where the predicate field appears in the record. Elements that do not
// match are discarded, and unselected subtrees are never buffered.
//
// Values are appended to the result in the order they appear in the stream.
func CompileQuery(query string) (*CompiledQuery, error) {
	root, err := parseQuery(query)
	if err != nil {
		return nil, err
	}
	return &CompiledQuery{
		query: query,
		root:  root,
	}, nil
}

func (q *CompiledQuery) String() string {
	return q.query
}

// Query executes the compiled query against the msgpack stream read by d and
// returns the extracted values. When the stream is truncated or malformed,
// the returned error is a *QueryError that reports the path being processed
// at the time of the failure.
func (q *CompiledQuery) Query(d *Decoder) ([]interface{}, error) {
	ctx := queryCtx{
		query: q.query,
	}
	ctx.sink = &ctx.values
	if err := q.root.exec(d, &ctx); err != nil {
		return nil, err
	}
	return ctx.values, nil
}

//------------------------------------------------------------------------------

type queryCtx struct {
	query   string
	values  []interface{}
	sink    *[]interface{}
	bufs    [][]interface{}
	nbuf    int
	path    []queryPathSeg
	keyBufs [][]byte
}

type queryPathSeg struct {
	text string
	idx  int // array element index; -1 when text is used
}

func (ctx *queryCtx) emit(v interface{}) {
	*ctx.sink = append(*ctx.sink, v)
}

// pushSink redirects emitted values to a reusable scratch buffer and returns
// the previous sink.
func (ctx *queryCtx) pushSink() *[]interface{} {
	outer := ctx.sink
	if ctx.nbuf == len(ctx.bufs) {
		ctx.bufs = append(ctx.bufs, nil)
	}
	ctx.bufs[ctx.nbuf] = ctx.bufs[ctx.nbuf][:0]
	ctx.sink = &ctx.bufs[ctx.nbuf]
	ctx.nbuf++
	return outer
}

// popSink restores the outer sink, appending the buffered values to it when
// commit is true and discarding them otherwise.
func (ctx *queryCtx) popSink(outer *[]interface{}, commit bool) {
	ctx.nbuf--
	tmp := ctx.bufs[ctx.nbuf]
	ctx.sink = outer
	if commit {
		*outer = append(*outer, tmp...)
	}
}

func (ctx *queryCtx) push(text string) {
	ctx.path = append(ctx.path, queryPathSeg{text: text, idx: -1})
}

// pushKey is like push, but for keys decoded from the stream that reference
// the decoder's reusable buffer.
func (ctx *queryCtx) pushKey(key string) {
	depth := len(ctx.path)
	for len(ctx.keyBufs) <= depth {
		ctx.keyBufs = append(ctx.keyBufs, make([]byte, 0, 32))
	}
	buf := append(ctx.keyBufs[depth][:0], key...)
	ctx.keyBufs[depth] = buf
	ctx.path = append(ctx.path, queryPathSeg{text: bytesToString(buf), idx: -1})
}

func (ctx *queryCtx) pushIndex(idx int) {
	ctx.path = append(ctx.path, queryPathSeg{idx: idx})
}

func (ctx *queryCtx) pop() {
	ctx.path = ctx.path[:len(ctx.path)-1]
}

func (ctx *queryCtx) pathString() string {
	var sb strings.Builder
	for i := range ctx.path {
		seg := &ctx.path[i]
		text := seg.text
		if seg.idx >= 0 {
			text = "[" + strconv.Itoa(seg.idx) + "]"
		}
		if sb.Len() > 0 && !strings.HasPrefix(text, "[") {
			sb.WriteByte('.')
		}
		sb.WriteString(text)
	}
	if sb.Len() == 0 {
		return "$"
	}
	return sb.String()
}

func (ctx *queryCtx) wrapErr(err error) error {
	if err == nil {
		return nil
	}
	var qerr *QueryError
	if errors.As(err, &qerr) {
		return err
	}
	return &QueryError{
		Query: ctx.query,
		Path:  ctx.pathString(),
		Err:   err,
	}
}

//------------------------------------------------------------------------------

type queryNode interface {
	exec(d *Decoder, ctx *queryCtx) error
}

type chainNode interface {
	queryNode
	setNext(queryNode)
}

type leafNode struct{}

func (n *leafNode) exec(d *Decoder, ctx *queryCtx) error {
	v, err := d.decodeInterfaceCond()
	if err != nil {
		return ctx.wrapErr(err)
	}
	ctx.emit(v)
	return nil
}

//------------------------------------------------------------------------------

type keyNode struct {
	key  string
	next queryNode
}

func (n *keyNode) setNext(next queryNode) {
	n.next = next
}

func (n *keyNode) exec(d *Decoder, ctx *queryCtx) error {
	ctx.push(n.key)
	defer ctx.pop()

	num, err := queryMapLen(d)
	if err != nil {
		return ctx.wrapErr(err)
	}
	if num == -1 {
		return nil
	}

	for i := 0; i < num; i++ {
		key, err := d.decodeStringTemp()
		if err != nil {
			return ctx.wrapErr(err)
		}
		if key == n.key {
			if err := n.next.exec(d, ctx); err != nil {
				return err
			}
			return ctx.wrapErr(d.skipNext((num - i - 1) * 2))
		}
		if err := d.Skip(); err != nil {
			return ctx.wrapErr(err)
		}
	}
	return nil
}

//------------------------------------------------------------------------------

type indexNode struct {
	idx  int
	seg  string
	next queryNode
}

func newIndexNode(idx int) *indexNode {
	return &indexNode{
		idx: idx,
		seg: "[" + strconv.Itoa(idx) + "]",
	}
}

func (n *indexNode) setNext(next queryNode) {
	n.next = next
}

func (n *indexNode) exec(d *Decoder, ctx *queryCtx) error {
	ctx.push(n.seg)
	defer ctx.pop()

	num, err := queryArrayLen(d)
	if err != nil {
		return ctx.wrapErr(err)
	}
	if num == -1 {
		return nil
	}

	for i := 0; i < num; i++ {
		if i == n.idx {
			if err := n.next.exec(d, ctx); err != nil {
				return err
			}
			return ctx.wrapErr(d.skipNext(num - i - 1))
		}
		if err := d.Skip(); err != nil {
			return ctx.wrapErr(err)
		}
	}
	return nil
}

//------------------------------------------------------------------------------

type sliceNode struct {
	lo   int
	hi   int // -1 selects up to the end of the array
	seg  string
	next queryNode
}

func (n *sliceNode) setNext(next queryNode) {
	n.next = next
}

func (n *sliceNode) exec(d *Decoder, ctx *queryCtx) error {
	ctx.push(n.seg)
	defer ctx.pop()

	num, err := queryArrayLen(d)
	if err != nil {
		return ctx.wrapErr(err)
	}
	if num == -1 {
		return nil
	}

	hi := n.hi
	if hi < 0 || hi > num {
		hi = num
	}

	for i := 0; i < num; i++ {
		if i >= n.lo && i < hi {
			ctx.pushIndex(i)
			err := n.next.exec(d, ctx)
			ctx.pop()
			if err != nil {
				return err
			}
			continue
		}
		if err := d.Skip(); err != nil {
			return ctx.wrapErr(err)
		}
	}
	return nil
}

//------------------------------------------------------------------------------

type filterNode struct {
	pred predicate
	lo   int
	hi   int // -1 means no limit on the number of matching elements
	seg  string
	next queryNode // *keyNode, *multiNode, or *leafNode
}

func (n *filterNode) setNext(next queryNode) {
	n.next = next
}

func (n *filterNode) exec(d *Decoder, ctx *queryCtx) error {
	ctx.push(n.seg)
	defer ctx.pop()

	num, err := queryArrayLen(d)
	if err != nil {
		return ctx.wrapErr(err)
	}
	if num == -1 {
		return nil
	}

	matched := 0
	for i := 0; i < num; i++ {
		if n.hi >= 0 && matched >= n.hi {
			if err := d.Skip(); err != nil {
				return ctx.wrapErr(err)
			}
			continue
		}

		ctx.pushIndex(i)
		outer := ctx.pushSink()
		ok, err := n.record(d, ctx)
		ctx.popSink(outer, ok && matched >= n.lo)
		ctx.pop()
		if err != nil {
			return err
		}
		if ok {
			matched++
		}
	}
	return nil
}

// record consumes one array element and reports whether it matches the
// predicate. Values selected from the element are emitted into the current
// (scratch) sink and are committed or discarded by the caller.
func (n *filterNode) record(d *Decoder, ctx *queryCtx) (bool, error) {
	if _, ok := n.next.(*leafNode); ok {
		v, err := d.decodeInterfaceCond()
		if err != nil {
			return false, ctx.wrapErr(err)
		}
		if n.pred.matchValue(v) {
			ctx.emit(v)
			return true, nil
		}
		return false, nil
	}

	num, err := queryMapLen(d)
	if err != nil {
		return false, ctx.wrapErr(err)
	}
	if num == -1 {
		return false, nil
	}

	matched := false
	for i := 0; i < num; i++ {
		key, err := d.decodeStringTemp()
		if err != nil {
			return false, ctx.wrapErr(err)
		}

		if key == n.pred.field {
			matched, err = n.pred.decodeAndMatch(d)
			if err != nil {
				return false, ctx.wrapErr(err)
			}
			continue
		}

		if next := queryBranch(n.next, key); next != nil {
			ctx.pushKey(key)
			err := next.exec(d, ctx)
			ctx.pop()
			if err != nil {
				return false, err
			}
			continue
		}

		if err := d.Skip(); err != nil {
			return false, ctx.wrapErr(err)
		}
	}
	return matched, nil
}

func queryBranch(node queryNode, key string) queryNode {
	switch node := node.(type) {
	case *keyNode:
		if node.key == key {
			return node.next
		}
	case *multiNode:
		return node.branches[key]
	}
	return nil
}

//------------------------------------------------------------------------------

type multiNode struct {
	branches map[string]queryNode
	seg      string
}

func (n *multiNode) exec(d *Decoder, ctx *queryCtx) error {
	ctx.push(n.seg)
	defer ctx.pop()

	num, err := queryMapLen(d)
	if err != nil {
		return ctx.wrapErr(err)
	}
	if num == -1 {
		return nil
	}

	for i := 0; i < num; i++ {
		key, err := d.decodeStringTemp()
		if err != nil {
			return ctx.wrapErr(err)
		}
		if next := n.branches[key]; next != nil {
			ctx.pushKey(key)
			err := next.exec(d, ctx)
			ctx.pop()
			if err != nil {
				return err
			}
			continue
		}
		if err := d.Skip(); err != nil {
			return ctx.wrapErr(err)
		}
	}
	return nil
}

//------------------------------------------------------------------------------

type predicate struct {
	field string
	ne    bool // != instead of ==
	value interface{}
}

func (p *predicate) match(v interface{}) bool {
	eq := queryValuesEqual(v, p.value)
	if p.ne {
		return !eq
	}
	return eq
}

func (p *predicate) matchValue(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok {
		return false
	}
	return p.match(m[p.field])
}

func (p *predicate) decodeAndMatch(d *Decoder) (bool, error) {
	if _, ok := p.value.(string); ok {
		c, err := d.PeekCode()
		if err != nil {
			return false, err
		}
		if msgpcode.IsFixedString(c) || c == msgpcode.Str8 ||
			c == msgpcode.Str16 || c == msgpcode.Str32 {
			s, err := d.decodeStringTemp()
			if err != nil {
				return false, err
			}
			return p.match(s), nil
		}
	}
	v, err := d.decodeInterfaceCond()
	if err != nil {
		return false, err
	}
	return p.match(v), nil
}

func queryValuesEqual(a, b interface{}) bool {
	switch b := b.(type) {
	case nil:
		return a == nil
	case bool:
		av, ok := a.(bool)
		return ok && av == b
	case string:
		av, ok := a.(string)
		return ok && av == b
	case int64:
		if ai, ok := queryInt(a); ok {
			return ai == b
		}
		if af, ok := queryFloat(a); ok {
			return af == float64(b)
		}
		return false
	case float64:
		af, ok := queryFloat(a)
		return ok && af == b
	}
	return false
}

func queryInt(v interface{}) (int64, bool) {
	switch v := v.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		return int64(v), true
	}
	return 0, false
}

func queryFloat(v interface{}) (float64, bool) {
	if n, ok := queryInt(v); ok {
		return float64(n), true
	}
	switch v := v.(type) {
	case float32:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

//------------------------------------------------------------------------------

func queryMapLen(d *Decoder) (int, error) {
	c, err := d.PeekCode()
	if err != nil {
		return -1, err
	}
	if c == msgpcode.Nil {
		if err := d.Skip(); err != nil {
			return -1, err
		}
		return -1, nil
	}
	return d.DecodeMapLen()
}

func queryArrayLen(d *Decoder) (int, error) {
	c, err := d.PeekCode()
	if err != nil {
		return -1, err
	}
	if c == msgpcode.Nil {
		if err := d.Skip(); err != nil {
			return -1, err
		}
		return -1, nil
	}
	return d.DecodeArrayLen()
}
