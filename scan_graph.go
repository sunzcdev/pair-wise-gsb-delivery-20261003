package sqlx

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jmoiron/sqlx/reflectx"
)

// Relation declares how rows of a single table (or table alias) map onto one
// level of a graph of structs assembled from a joined result set.
//
// A relation tree mirrors the shape of the destination struct: the root
// relation describes the root struct and each entry of Relations describes a
// slice field on the parent struct that holds child entities.
//
// Model is the struct type carried by this level (e.g. Order{}).
// Prefix is the literal column prefix shared by every column belonging to
// this level; it is stripped before the column name is matched against the
// struct's db tags. The root relation normally uses the empty prefix.
// Keys lists the Go field names that together form the composite identity of
// this level. A left-joined level is considered absent from a row when all of
// its key columns are NULL; it is an error if only some of them are NULL.
// Relations declares the collection fields owned by this level. The key of
// each entry must be the name of a slice field on Model and is also used to
// report which collection an entity belongs to.
type Relation struct {
	Model     interface{}
	Prefix    string
	Keys      []string
	Relations map[string]*Relation
}

// ScanGraph assembles the joined rows returned by rows into dest according to
// root.
//
// dest must be a pointer to either a single struct or a slice of structs
// (pointers to structs are allowed). Each row is split across the relation
// levels by column prefix; entities carrying the same composite key are merged
// into a single reference, so repeating a parent for several children does
// not duplicate it. Conflicting non-key values for the same entity are
// reported with the offending row number and field instead of letting the
// last row win.
//
// The relation graph must be a tree: a level that revisits an ancestor (a
// cycle) is rejected before any row is scanned. On any scanning or assembly
// error dest is left untouched. If rows is an *sqlx.Rows its Mapper is used,
// otherwise the package default mapper is used.
func ScanGraph(rows rowsi, dest interface{}, root *Relation) error {
	if root == nil {
		return errors.New("sqlx: ScanGraph requires a non-nil root Relation")
	}

	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(columns) == 0 {
		return errors.New("sqlx: ScanGraph requires a result set with at least one column")
	}

	var m *reflectx.Mapper
	if rx, ok := rows.(*Rows); ok {
		m = rx.Mapper
	} else {
		m = mapper()
	}

	plan, err := compileGraph(dest, root, columns, m)
	if err != nil {
		return err
	}

	// Everything is assembled into freshly allocated values and only committed
	// to dest after every row has been consumed without error.
	var rootOrder []string
	rootEnts := map[string]reflect.Value{}
	rowNum := 0

	for rows.Next() {
		rowNum++
		scratch := plan.newScratch()
		targets := plan.targets(scratch)
		if err := rows.Scan(targets...); err != nil {
			return fmt.Errorf("sqlx: ScanGraph failed scanning row %d: %w", rowNum, err)
		}

		var visit func(n *graphNode, parentKey string, parentEnt reflect.Value) error
		visit = func(n *graphNode, parentKey string, parentEnt reflect.Value) error {
			nulls := 0
			for _, c := range n.keyCols {
				if c.target.null {
					nulls++
				}
			}
			if nulls == len(n.keys) {
				// Fully NULL outer-join key: this level and everything below it
				// is absent from this row.
				return nil
			}
			if nulls > 0 {
				return fmt.Errorf("sqlx: row %d: composite key of %s is partially NULL", rowNum, n.name)
			}

			assignNode(scratch[n.idx], n)
			ownKey, err := identityKey(scratch, n)
			if err != nil {
				return fmt.Errorf("sqlx: row %d: %w", rowNum, err)
			}

			// Entities are identified per level by their own composite key,
			// independently of the parent they are reached through, so the
			// same child repeated under several parents is one reference.
			ent, ok := n.ents[ownKey]
			if !ok {
				ent = reflect.New(n.model)
				n.ents[ownKey] = ent
				copyLeaves(ent.Elem(), scratch[n.idx], n.cols)
				if n.parent == nil {
					rootEnts[ownKey] = ent
					rootOrder = append(rootOrder, ownKey)
				}
			} else {
				if err := checkConflict(ent.Elem(), scratch[n.idx], n, rowNum); err != nil {
					return err
				}
			}
			if n.parent != nil {
				n.link(parentKey, ownKey)
			}

			for _, ch := range n.children {
				if err := visit(ch, ownKey, ent); err != nil {
					return err
				}
			}
			return nil
		}

		if err := visit(plan.root, "", reflect.Value{}); err != nil {
			return err
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlx: ScanGraph failed after row %d: %w", rowNum, err)
	}

	return plan.commit(dest, rootOrder, rootEnts)
}

// scanTarget receives one scanned column through a temporary destination so
// that NULL outer-join columns do not fail against non-nullable struct
// fields. Non-NULL values are converted like database/sql and copied onto the
// scratch struct only once the level is known to be present in the row.
type scanTarget struct {
	dst  interface{}
	null bool
}

func (k *scanTarget) Scan(src interface{}) error {
	if src == nil {
		k.null = true
		return nil
	}
	k.null = false
	return convertAssign(k.dst, src)
}

// graphNode is the compiled form of a Relation for one struct level.
type graphNode struct {
	name            string
	model           reflect.Type
	prefix          string
	keys            []*keyBinding
	cols            []*colBinding
	keyCols         []*colBinding
	children        []*graphNode
	parent          *graphNode
	collectionField *reflect.StructField
	elemIsPtr       bool
	ents            map[string]reflect.Value
	created         []childRef
	idx             int
}

type childRef struct {
	childKey  string
	parentKey string
}

// link records that parentKey owns childKey, ignoring repeated parent rows.
func (n *graphNode) link(parentKey, childKey string) {
	for _, r := range n.created {
		if r.parentKey == parentKey && r.childKey == childKey {
			return
		}
	}
	n.created = append(n.created, childRef{childKey: childKey, parentKey: parentKey})
}

type keyBinding struct {
	traversal []int
	goName    string
}

type colBinding struct {
	traversal []int
	fieldName string
	key       *keyBinding
	target    *scanTarget
}

type graphPlan struct {
	root    *graphNode
	nodes   []*graphNode
	columns []*colSlot
	single  bool
	slice   reflect.Type
	elemPtr bool
}

// colSlot remembers, for one result column, which node and which of its
// fields it is scanned into.
type colSlot struct {
	node int
	col  *colBinding
}

func compileGraph(dest interface{}, root *Relation, columns []string, m *reflectx.Mapper) (*graphPlan, error) {
	value := reflect.ValueOf(dest)
	if value.Kind() != reflect.Ptr {
		return nil, errors.New("must pass a pointer, not a value, to ScanGraph destination")
	}
	if value.IsNil() {
		return nil, errors.New("nil pointer passed to ScanGraph destination")
	}
	direct := reflect.Indirect(value)

	plan := &graphPlan{}
	switch direct.Kind() {
	case reflect.Struct:
		plan.single = true
	case reflect.Slice:
		elem := direct.Type().Elem()
		plan.elemPtr = elem.Kind() == reflect.Ptr
		base := reflectx.Deref(elem)
		if base.Kind() != reflect.Struct {
			return nil, fmt.Errorf("expected a slice of structs but got a slice of %s", elem.Kind())
		}
		plan.slice = direct.Type()
	default:
		return nil, fmt.Errorf("expected a pointer to a struct or a slice of structs but got %s", direct.Kind())
	}

	visited := map[*Relation]bool{}
	n, err := buildNode(root, "", nil, nil, visited, m)
	if err != nil {
		return nil, err
	}
	plan.root = n
	indexNodes(plan, n)
	if err := checkPrefixes(plan.nodes); err != nil {
		return nil, err
	}

	if plan.single {
		if n.model != direct.Type() {
			return nil, fmt.Errorf("root relation model %s does not match destination %s", n.model, direct.Type())
		}
	} else {
		rootElem := n.model
		if plan.elemPtr {
			rootElem = reflect.PtrTo(rootElem)
		}
		if rootElem != plan.slice.Elem() {
			return nil, fmt.Errorf("root relation model %s does not match slice element %s", n.model, plan.slice.Elem())
		}
	}

	// Bind every result column to exactly one node field.
	unassigned := make([]bool, len(columns))
	plan.columns = make([]*colSlot, len(columns))
	for i := range columns {
		unassigned[i] = true
	}
	for _, node := range plan.nodes {
		tm := m.TypeMap(node.model)
		for ci, col := range columns {
			if !strings.HasPrefix(col, node.prefix) {
				continue
			}
			if longestPrefixNode(plan.nodes, col) != node {
				continue
			}
			stripped := col[len(node.prefix):]
			// Match the full column name first (sqlx convention: the db tag is
			// the actual column, e.g. "item_name"). Fall back to the prefix-
			// stripped name for structs tagged with leaf names (e.g. "name").
			traversal := m.TraversalsByName(node.model, []string{col})[0]
			if len(traversal) == 0 {
				traversal = m.TraversalsByName(node.model, []string{stripped})[0]
			}
			if len(traversal) == 0 {
				return nil, fmt.Errorf("column %q has prefix %q but %s has no field mapped to %q", col, node.prefix, node.name, stripped)
			}
			fi := tm.GetByTraversal(traversal)
			binding := &colBinding{traversal: traversal, fieldName: fi.Field.Name}
			if kb := node.keyByGoName(fi.Field.Name); kb != nil {
				binding.key = kb
				node.keyCols = append(node.keyCols, binding)
			}
			node.cols = append(node.cols, binding)
			plan.columns[ci] = &colSlot{node: node.idx, col: binding}
			unassigned[ci] = false
		}
		if len(node.cols) == 0 {
			return nil, fmt.Errorf("%s with prefix %q matched no columns", node.name, node.prefix)
		}
		for _, kb := range node.keys {
			found := false
			for _, cb := range node.keyCols {
				if cb.key == kb {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("key field %s of %s matched no result column under prefix %q", kb.goName, node.name, node.prefix)
			}
		}
	}
	for ci, missing := range unassigned {
		if missing {
			return nil, fmt.Errorf("column %q does not match any relation prefix", columns[ci])
		}
	}

	return plan, nil
}

func buildNode(rel *Relation, name string, parent *graphNode, collectionField *reflect.StructField, visited map[*Relation]bool, m *reflectx.Mapper) (*graphNode, error) {
	if rel == nil {
		return nil, errors.New("sqlx: a relation in the graph is nil")
	}
	if visited[rel] {
		return nil, fmt.Errorf("sqlx: relation graph contains a cycle through %q", name)
	}
	if rel.Model == nil {
		return nil, fmt.Errorf("sqlx: relation %q requires a non-nil Model", name)
	}
	model := reflectx.Deref(reflect.TypeOf(rel.Model))
	if model.Kind() != reflect.Struct {
		return nil, fmt.Errorf("sqlx: relation %q model must be a struct, got %s", name, model.Kind())
	}
	if name == "" {
		name = model.Name()
	}
	if len(rel.Keys) == 0 {
		return nil, fmt.Errorf("sqlx: relation %q must declare at least one key field", name)
	}
	if parent != nil && prefixOverlaps(rel.Prefix, parent.ancestorPrefixes()) {
		return nil, fmt.Errorf("sqlx: prefix %q of %s overlaps a prefix used by one of its ancestors", rel.Prefix, name)
	}

	visited[rel] = true
	node := &graphNode{
		name:            name,
		model:           model,
		prefix:          rel.Prefix,
		parent:          parent,
		collectionField: collectionField,
		ents:            map[string]reflect.Value{},
	}

	tm := m.TypeMap(model)
	for _, keyGoName := range rel.Keys {
		fi := keyFieldInfo(tm, keyGoName)
		if fi == nil {
			return nil, fmt.Errorf("sqlx: key field %q not found on %s", keyGoName, name)
		}
		node.keys = append(node.keys, &keyBinding{traversal: fi.Index, goName: keyGoName})
	}

	for fieldName, childRel := range rel.Relations {
		field, ok := model.FieldByName(fieldName)
		if !ok {
			return nil, fmt.Errorf("sqlx: relation %q is not a field of %s", fieldName, name)
		}
		sliceType := field.Type
		if sliceType.Kind() != reflect.Slice {
			return nil, fmt.Errorf("sqlx: relation %q of %s must be a slice field", fieldName, name)
		}
		elemType := sliceType.Elem()
		elemIsPtr := elemType.Kind() == reflect.Ptr
		childModel := reflectx.Deref(elemType)
		if childModel.Kind() != reflect.Struct {
			return nil, fmt.Errorf("sqlx: collection %q of %s must hold structs", fieldName, name)
		}
		if childRel.Model != nil {
			if reflectx.Deref(reflect.TypeOf(childRel.Model)) != childModel {
				return nil, fmt.Errorf("sqlx: model of relation %q does not match slice element of %s.%s", fieldName, name, fieldName)
			}
		} else {
			childRel.Model = reflect.New(childModel).Interface()
		}
		child, err := buildNode(childRel, name+"."+fieldName, node, &field, visited, m)
		if err != nil {
			return nil, err
		}
		child.elemIsPtr = elemIsPtr
		node.children = append(node.children, child)
	}
	delete(visited, rel)
	return node, nil
}

// indexNodes numbers nodes in pre-order and collects them into plan.nodes.
func indexNodes(plan *graphPlan, n *graphNode) {
	n.idx = len(plan.nodes)
	plan.nodes = append(plan.nodes, n)
	for _, ch := range n.children {
		indexNodes(plan, ch)
	}
}

// checkPrefixes rejects prefixes that are string prefixes of one another,
// since column ownership would then be ambiguous.
func checkPrefixes(nodes []*graphNode) error {
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			a, b := nodes[i].prefix, nodes[j].prefix
			if a == b {
				return fmt.Errorf("sqlx: relations %s and %s share prefix %q", nodes[i].name, nodes[j].name, a)
			}
			if a == "" || b == "" {
				continue
			}
			if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
				return fmt.Errorf("sqlx: prefixes %q (%s) and %q (%s) overlap", a, nodes[i].name, b, nodes[j].name)
			}
		}
	}
	return nil
}

// longestPrefixNode returns the node whose prefix matches the most of col.
func longestPrefixNode(nodes []*graphNode, col string) *graphNode {
	var best *graphNode
	for _, n := range nodes {
		if !strings.HasPrefix(col, n.prefix) {
			continue
		}
		if best == nil || len(n.prefix) > len(best.prefix) {
			best = n
		}
	}
	return best
}

func (n *graphNode) keyByGoName(goName string) *keyBinding {
	for _, k := range n.keys {
		if k.goName == goName {
			return k
		}
	}
	return nil
}

func (n *graphNode) ancestorPrefixes() []string {
	var out []string
	for p := n.parent; p != nil; p = p.parent {
		out = append(out, p.prefix)
	}
	return out
}

func prefixOverlaps(prefix string, others []string) bool {
	for _, other := range others {
		if prefix == "" || other == "" {
			continue
		}
		if prefix == other {
			return true
		}
		if strings.HasPrefix(prefix, other) || strings.HasPrefix(other, prefix) {
			return true
		}
	}
	return false
}

// newScratch allocates one empty struct per node and resets the key scanners.
func (p *graphPlan) newScratch() []reflect.Value {
	scratch := make([]reflect.Value, len(p.nodes))
	for i, n := range p.nodes {
		scratch[i] = reflect.New(n.model).Elem()
		for _, c := range n.cols {
			c.target = &scanTarget{}
		}
	}
	return scratch
}

// targets builds the scan destination slice for one row. Every column is
// scanned into its own scanTarget, which tolerates NULL; values are copied
// onto the scratch structs after the row's present levels are known.
func (p *graphPlan) targets(scratch []reflect.Value) []interface{} {
	targets := make([]interface{}, len(p.columns))
	for i, slot := range p.columns {
		n := p.nodes[slot.node]
		field := reflectx.FieldByIndexes(scratch[n.idx], slot.col.traversal)
		slot.col.target.dst = field.Addr().Interface()
		targets[i] = slot.col.target
	}
	return targets
}

// assignNode copies scanned values for a present level onto its scratch
// struct. NULL non-key columns become the field's zero value.
func assignNode(dst reflect.Value, n *graphNode) {
	for _, c := range n.cols {
		if !c.target.null {
			continue
		}
		target := reflectx.FieldByIndexes(dst, c.traversal)
		target.Set(reflect.Zero(target.Type()))
	}
}

// commit publishes the assembled graph into dest. It is the only point at
// which caller-owned memory is touched, so a failed scan leaves dest as it was
// on entry.
func (p *graphPlan) commit(dest interface{}, order []string, ents map[string]reflect.Value) error {
	// Attach children deepest-first while every entity is still a *T. Value
	// slices copy on append, so attaching before the root copy guarantees
	// grandchildren end up inside the value the root actually receives.
	for i := len(p.nodes) - 1; i >= 0; i-- {
		n := p.nodes[i]
		if n.parent == nil {
			continue
		}
		for _, ref := range n.created {
			parent := n.parent.ents[ref.parentKey]
			child := n.ents[ref.childKey]
			field := parent.Elem().FieldByIndex(n.collectionField.Index)
			appendNewEntity(field, child, n.elemIsPtr)
		}
	}

	direct := reflect.Indirect(reflect.ValueOf(dest))
	if p.single {
		switch len(order) {
		case 0:
			return nil
		case 1:
			direct.Set(ents[order[0]].Elem())
			return nil
		default:
			return fmt.Errorf("sqlx: ScanGraph expected a single %s but the rows contain %d distinct keys", p.root.model, len(order))
		}
	}

	result := reflect.MakeSlice(p.slice, 0, len(order))
	for _, key := range order {
		ent := ents[key]
		if p.elemPtr {
			result = reflect.Append(result, ent)
		} else {
			result = reflect.Append(result, ent.Elem())
		}
	}
	direct.Set(result)
	return nil
}

// copyLeaves copies every bound column of a freshly scanned scratch struct
// onto a newly allocated entity. Key columns are copied alongside the rest.
func copyLeaves(dst, src reflect.Value, cols []*colBinding) {
	for _, c := range cols {
		leaf := reflectx.FieldByIndexesReadOnly(src, c.traversal)
		setLeaf(dst, c.traversal, leaf)
	}
}

// checkConflict compares a row's scratch values against the already stored
// entity. Non-key scalar fields must agree; a mismatch is reported with the
// row number and the offending field rather than silently overwriting.
func checkConflict(ent, src reflect.Value, n *graphNode, rowNum int) error {
	for _, c := range n.cols {
		old := leafValue(ent, c.traversal)
		fresh := leafValue(src, c.traversal)
		if !valuesEqual(old, fresh) {
			return fmt.Errorf("sqlx: row %d: conflicting value for field %s of %s (existing %v, row %v)", rowNum, c.fieldName, n.name, formatValue(old), formatValue(fresh))
		}
	}
	return nil
}

func leafValue(v reflect.Value, traversal []int) reflect.Value {
	for _, i := range traversal {
		if v.Kind() == reflect.Ptr {
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func setLeaf(dst reflect.Value, traversal []int, src reflect.Value) {
	target := reflectx.FieldByIndexes(dst, traversal)
	if !src.IsValid() {
		target.Set(reflect.Zero(target.Type()))
		return
	}
	target.Set(src)
}

func valuesEqual(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

func formatValue(v reflect.Value) interface{} {
	if !v.IsValid() {
		return nil
	}
	return v.Interface()
}

// identityKey builds the composite identity string of node in the current
// row. All key columns have already been confirmed non-NULL.
func identityKey(scratch []reflect.Value, n *graphNode) (string, error) {
	parts := make([]string, 0, len(n.keys))
	for _, k := range n.keys {
		v := leafValue(scratch[n.idx], k.traversal)
		if !v.IsValid() {
			return "", fmt.Errorf("key field %s of %s is NULL", k.goName, n.name)
		}
		var s string
		switch b := v.Interface().(type) {
		case []byte:
			s = string(b)
		default:
			s = fmt.Sprintf("%v", b)
		}
		parts = append(parts, v.Type().String()+":"+s)
	}
	return strings.Join(parts, "\x1f"), nil
}

// appendNewEntity adds a freshly created entity to the parent collection. It
// is only called for keys the node has not seen under this parent, so the
// collection cannot gain duplicates.
func appendNewEntity(field reflect.Value, ent reflect.Value, elemIsPtr bool) {
	if elemIsPtr {
		field.Set(reflect.Append(field, ent))
		return
	}
	field.Set(reflect.Append(field, ent.Elem()))
}

// convertAssign assigns src to the value pointed to by dest, covering the
// conversions database/sql does for the scalar types used in mapped structs.
// It is only used for key columns, which are scanned through keyTarget in
// order to observe NULL; non-key columns are scanned directly by database/sql.
func convertAssign(dest interface{}, src interface{}) error {
	if scanner, ok := dest.(sql.Scanner); ok {
		return scanner.Scan(src)
	}
	dv := reflect.ValueOf(dest)
	if dv.Kind() != reflect.Ptr || dv.IsNil() {
		return fmt.Errorf("sqlx: ScanGraph got non-pointer key target %T", dest)
	}
	sv := reflect.ValueOf(src)
	pt := dv.Type()
	if !sv.IsValid() {
		return fmt.Errorf("sqlx: ScanGraph cannot assign NULL to non-scanner key %s", pt)
	}
	st := sv.Type()
	if st.AssignableTo(pt) {
		dv.Elem().Set(sv)
		return nil
	}
	if st.ConvertibleTo(pt.Elem()) {
		dv.Elem().Set(sv.Convert(pt.Elem()))
		return nil
	}
	return fmt.Errorf("sqlx: ScanGraph cannot convert key value of type %s to %s", st, pt.Elem())
}

// ScanGraph assembles joined rows into dest according to root. See the
// package-level ScanGraph documentation for the mapping rules.
func (r *Rows) ScanGraph(dest interface{}, root *Relation) error {
	return ScanGraph(r, dest, root)
}

// keyFieldInfo resolves a key given as a Go field name or a path through
// embedded structs against the flattened reflectx mapping.
func keyFieldInfo(tm *reflectx.StructMap, keyName string) *reflectx.FieldInfo {
	if fi := tm.Paths[keyName]; fi != nil && fi.Field.Name != "" {
		return fi
	}
	var found *reflectx.FieldInfo
	for _, fi := range tm.Paths {
		if fi.Embedded || fi.Field.Name == "" {
			continue
		}
		if fi.Field.Name == keyName {
			if found != nil {
				return nil
			}
			found = fi
		}
	}
	return found
}
