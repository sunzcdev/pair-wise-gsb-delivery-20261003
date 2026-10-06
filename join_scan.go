package sqlx

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx/reflectx"
)

// JoinMap describes how rows from a joined query expand into one struct and its
// nested collections.
type JoinMap struct {
	// Prefix is the literal column prefix used by this entity. An empty prefix
	// is normally only useful for the root mapping.
	Prefix string

	// Keys are the mapped destination field names that identify an entity.
	// Multiple names form a composite key.
	Keys []string

	// Field is the slice field on the parent entity to which this collection
	// belongs. It is ignored on the root mapping.
	Field string

	// Relations describes nested collections.
	Relations []*JoinMap
}

// JoinConflictError reports two different scalar values for one entity key.
type JoinConflictError struct {
	Row         int
	PreviousRow int
	Column      string
}

func (e *JoinConflictError) Error() string {
	return fmt.Sprintf("conflicting value for column %q on row %d; previous value was on row %d", e.Column, e.Row, e.PreviousRow)
}

type joinRows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(...interface{}) error
	Err() error
}

// JoinScan scans joined rows and assembles them into dest, which must be a
// pointer to a struct or a pointer to a slice of structs. Rows are assembled in
// temporary values and dest is modified only after the complete result set has
// been scanned successfully.
func JoinScan(rows joinRows, dest interface{}, mapping *JoinMap) error {
	if err := validateJoinMappingCycle(mapping, nil); err != nil {
		return err
	}

	rootType, isSlice, ptrElems, err := joinDestType(dest)
	if err != nil {
		return err
	}

	fieldMapper := mapper()
	if r, ok := rows.(*Rows); ok && r.Mapper != nil {
		fieldMapper = r.Mapper
	}

	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	builder := &joinBuilder{types: map[*JoinMap]reflect.Type{}}
	root, err := builder.buildRoot(mapping, rootType, isSlice, ptrElems, fieldMapper)
	if err != nil {
		return err
	}
	if err := builder.assignColumns(columns); err != nil {
		return err
	}

	state := newJoinRuntime()
	raw := make([]interface{}, len(columns))
	rowNum := 0
	for rows.Next() {
		rowNum++
		for i := range raw {
			raw[i] = new(interface{})
		}
		if err := rows.Scan(raw...); err != nil {
			return err
		}
		for i := range raw {
			raw[i] = *(raw[i].(*interface{}))
		}
		if _, err := state.process(root, "", rowNum, raw, true); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	return state.writeDestination(root, dest)
}

// JoinScan assembles the remaining rows of an sqlx Rows result using mapping.
func (r *Rows) JoinScan(dest interface{}, mapping *JoinMap) error {
	return JoinScan(r, dest, mapping)
}

func joinDestType(dest interface{}) (reflect.Type, bool, bool, error) {
	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Ptr {
		return nil, false, false, fmt.Errorf("must pass a pointer, not a value, to JoinScan destination")
	}
	if v.IsNil() {
		return nil, false, false, fmt.Errorf("nil pointer passed to JoinScan destination")
	}
	base := v.Elem().Type()
	if base.Kind() == reflect.Struct {
		return base, false, false, nil
	}
	if base.Kind() != reflect.Slice {
		return nil, false, false, fmt.Errorf("expected struct or slice destination, got %s", base.Kind())
	}

	elem := base.Elem()
	ptrElems := false
	if elem.Kind() == reflect.Ptr {
		ptrElems = true
		elem = elem.Elem()
	}
	if elem.Kind() != reflect.Struct {
		return nil, false, false, fmt.Errorf("expected slice of structs, got %s", base.Elem().Kind())
	}
	return elem, true, ptrElems, nil
}

func validateJoinMappingCycle(m *JoinMap, ancestors map[*JoinMap]bool) error {
	if m == nil {
		return fmt.Errorf("JoinMap must not be nil")
	}
	if ancestors[m] {
		return fmt.Errorf("JoinMap contains a cycle at prefix %q", m.Prefix)
	}
	if len(m.Keys) == 0 {
		return fmt.Errorf("JoinMap with prefix %q must declare at least one key", m.Prefix)
	}

	next := make(map[*JoinMap]bool, len(ancestors)+1)
	for ancestor := range ancestors {
		next[ancestor] = true
	}
	next[m] = true

	for _, relation := range m.Relations {
		if err := validateJoinMappingCycle(relation, next); err != nil {
			return err
		}
	}
	return nil
}

type joinBuilder struct {
	entities []*joinEntity
	types    map[*JoinMap]reflect.Type
}

type joinEntity struct {
	mapping    *JoinMap
	typ        reflect.Type
	fieldType  reflect.Type
	structMap  *reflectx.StructMap
	sliceElem  bool
	ptrElems   bool
	fieldIndex []int
	keys       []*joinKey
	columns    []*joinColumn
	relations  []*joinEntity
}

type joinKey struct {
	name      string
	traversal []int
	column    int
}

type joinColumn struct {
	index     int
	name      string
	traversal []int
	key       bool
}

func (b *joinBuilder) buildRoot(spec *JoinMap, typ reflect.Type, isSlice, ptrElems bool, m *reflectx.Mapper) (*joinEntity, error) {
	fieldType := typ
	if isSlice {
		if ptrElems {
			fieldType = reflect.SliceOf(reflect.PtrTo(typ))
		} else {
			fieldType = reflect.SliceOf(typ)
		}
	}
	return b.entity(spec, typ, fieldType, isSlice, ptrElems, nil, m)
}

func (b *joinBuilder) buildRelation(spec *JoinMap, parent *joinEntity, m *reflectx.Mapper) (*joinEntity, error) {
	if spec.Field == "" {
		return nil, fmt.Errorf("JoinMap relation with prefix %q must declare Field", spec.Prefix)
	}

	field, ok := parent.typ.FieldByName(spec.Field)
	if !ok || len(field.PkgPath) != 0 {
		return nil, fmt.Errorf("field %q does not exist or is not exported on %s", spec.Field, parent.typ)
	}
	if field.Type.Kind() != reflect.Slice {
		return nil, fmt.Errorf("field %s on %s must be a slice", spec.Field, parent.typ)
	}

	elemType := field.Type.Elem()
	ptrElems := false
	if elemType.Kind() == reflect.Ptr {
		ptrElems = true
		elemType = elemType.Elem()
	}
	if elemType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("field %s on %s must be a slice of structs", spec.Field, parent.typ)
	}

	return b.entity(spec, elemType, field.Type, true, ptrElems, field.Index, m)
}

func (b *joinBuilder) entity(spec *JoinMap, typ, fieldType reflect.Type, sliceElem, ptrElems bool, index []int, m *reflectx.Mapper) (*joinEntity, error) {
	if existing, ok := b.types[spec]; ok && existing != typ {
		return nil, fmt.Errorf("JoinMap prefix %q is mapped to incompatible destination types", spec.Prefix)
	}
	b.types[spec] = typ

	structMap := m.TypeMap(typ)
	keys := make([]*joinKey, 0, len(spec.Keys))
	for _, name := range spec.Keys {
		fieldInfo := structMap.Names[name]
		if fieldInfo == nil {
			return nil, fmt.Errorf("key field %q does not exist on %s", name, typ)
		}
		keys = append(keys, &joinKey{name: name, traversal: fieldInfo.Index, column: -1})
	}

	e := &joinEntity{
		mapping:    spec,
		typ:        typ,
		fieldType:  fieldType,
		structMap:  structMap,
		sliceElem:  sliceElem,
		ptrElems:   ptrElems,
		fieldIndex: index,
		keys:       keys,
	}
	b.entities = append(b.entities, e)

	for _, relationSpec := range spec.Relations {
		relation, err := b.buildRelation(relationSpec, e, m)
		if err != nil {
			return nil, err
		}
		e.relations = append(e.relations, relation)
	}
	return e, nil
}

func (b *joinBuilder) assignColumns(columns []string) error {
	for index, column := range columns {
		candidates := make([]*joinEntity, 0)
		matchLen := -1
		for _, entity := range b.entities {
			prefix := entity.mapping.Prefix
			if !strings.HasPrefix(column, prefix) {
				continue
			}
			suffix := column[len(prefix):]
			fieldInfo := entity.structMap.Names[suffix]
			if fieldInfo == nil || entity.insideRelation(fieldInfo.Index) {
				continue
			}
			if len(prefix) > matchLen {
				candidates = make([]*joinEntity, 0)
				matchLen = len(prefix)
			}
			if len(prefix) == matchLen {
				candidates = append(candidates, entity)
			}
		}

		mappings := map[*JoinMap]bool{}
		for _, entity := range candidates {
			mappings[entity.mapping] = true
		}
		if len(mappings) != 1 {
			return fmt.Errorf("missing destination name %s", column)
		}

		for _, entity := range candidates {
			suffix := column[matchLen:]
			fieldInfo := entity.structMap.Names[suffix]
			columnInfo := &joinColumn{index: index, name: column, traversal: fieldInfo.Index}
			for _, key := range entity.keys {
				if equalInts(key.traversal, fieldInfo.Index) {
					if key.column >= 0 {
						columnInfo.key = true
						continue
					}
					key.column = index
					columnInfo.key = true
				}
			}
			entity.columns = append(entity.columns, columnInfo)
		}
	}

	for _, e := range b.entities {
		for _, key := range e.keys {
			if key.column < 0 {
				return fmt.Errorf("key column %q was not found in the result for %s", e.mapping.Prefix+key.name, e.typ)
			}
		}
	}
	return nil
}

func (e *joinEntity) insideRelation(index []int) bool {
	for _, relation := range e.relations {
		if hasJoinPrefix(index, relation.fieldIndex) {
			return true
		}
	}
	return false
}

func hasJoinPrefix(value, prefix []int) bool {
	if len(value) < len(prefix) {
		return false
	}
	for i := range prefix {
		if value[i] != prefix[i] {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type joinRuntime struct {
	entities   map[*JoinMap]*joinEntityData
	placements map[*joinEntity]*joinPlacementData
	populated  map[*joinEntity]map[string]bool
}

type joinEntityData struct {
	instances map[string]*joinInstance
}

type joinPlacementData struct {
	roots map[string]*joinInstance
	links map[*joinEntity]map[string]map[string]bool
}

type joinInstance struct {
	value     reflect.Value
	key       string
	keyValues []reflect.Value
	firstRow  int
}

func newJoinRuntime() *joinRuntime {
	return &joinRuntime{
		entities:   map[*JoinMap]*joinEntityData{},
		placements: map[*joinEntity]*joinPlacementData{},
		populated:  map[*joinEntity]map[string]bool{},
	}
}

func (rt *joinRuntime) entityData(e *joinEntity) *joinEntityData {
	data := rt.entities[e.mapping]
	if data == nil {
		data = &joinEntityData{instances: map[string]*joinInstance{}}
		rt.entities[e.mapping] = data
	}
	return data
}

func (rt *joinRuntime) placement(e *joinEntity) *joinPlacementData {
	data := rt.placements[e]
	if data == nil {
		data = &joinPlacementData{
			roots: map[string]*joinInstance{},
			links: map[*joinEntity]map[string]map[string]bool{},
		}
		rt.placements[e] = data
	}
	return data
}

func (rt *joinRuntime) process(e *joinEntity, parentKey string, rowNum int, raw []interface{}, root bool) (string, error) {
	keyValues, key, present, err := rt.readKey(e, rowNum, raw)
	if err != nil {
		return "", err
	}
	if !present {
		if root {
			return "", fmt.Errorf("root key columns are NULL on row %d", rowNum)
		}
		return "", nil
	}

	candidate := reflect.New(e.typ).Elem()
	for _, column := range e.columns {
		field := reflectx.FieldByIndexes(candidate, column.traversal)
		if err := assignJoinValue(field, raw[column.index]); err != nil {
			return "", fmt.Errorf("scanning column %q on row %d: %v", column.name, rowNum, err)
		}
	}

	data := rt.entityData(e)
	instance := data.instances[key]
	if instance == nil {
		instance = &joinInstance{
			value:     candidate,
			key:       key,
			keyValues: keyValues,
			firstRow:  rowNum,
		}
		data.instances[key] = instance
		if root {
			rt.placement(e).roots[key] = instance
		}
	} else {
		for _, column := range e.columns {
			current := reflectx.FieldByIndexesReadOnly(candidate, column.traversal)
			previous := reflectx.FieldByIndexesReadOnly(instance.value, column.traversal)
			if !reflect.DeepEqual(current.Interface(), previous.Interface()) {
				return "", &JoinConflictError{
					Row:         rowNum,
					PreviousRow: instance.firstRow,
					Column:      column.name,
				}
			}
		}
	}

	for _, relation := range e.relations {
		childKey, err := rt.process(relation, key, rowNum, raw, false)
		if err != nil {
			return "", err
		}
		if childKey == "" {
			continue
		}
		rt.addLink(e, relation, key, childKey)
	}
	return key, nil
}

func (rt *joinRuntime) readKey(e *joinEntity, rowNum int, raw []interface{}) ([]reflect.Value, string, bool, error) {
	values := make([]reflect.Value, 0, len(e.keys))
	nullKeys := 0
	for _, key := range e.keys {
		rawValue := raw[key.column]
		if rawValue == nil {
			nullKeys++
			values = append(values, reflect.Value{})
			continue
		}
		values = append(values, reflect.ValueOf(rawValue))
	}

	if nullKeys == len(e.keys) {
		return nil, "", false, nil
	}
	if nullKeys > 0 {
		return nil, "", false, fmt.Errorf("composite key for prefix %q is partially NULL on row %d", e.mapping.Prefix, rowNum)
	}

	key, err := encodeJoinKey(values)
	if err != nil {
		return nil, "", false, err
	}
	return values, key, true, nil
}

func derefJoinValue(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func (rt *joinRuntime) addLink(parent, child *joinEntity, parentKey, childKey string) {
	data := rt.placement(parent)
	byChild := data.links[child]
	if byChild == nil {
		byChild = map[string]map[string]bool{}
		data.links[child] = byChild
	}
	children := byChild[parentKey]
	if children == nil {
		children = map[string]bool{}
		byChild[parentKey] = children
	}
	children[childKey] = true
}

func (rt *joinRuntime) writeDestination(root *joinEntity, dest interface{}) error {
	data := rt.placement(root)
	instances := make([]*joinInstance, 0, len(data.roots))
	for _, instance := range data.roots {
		instances = append(instances, instance)
	}
	if err := sortJoinInstances(instances); err != nil {
		return err
	}

	if root.sliceElem {
		result := reflect.MakeSlice(root.fieldType, 0, len(instances))
		for _, instance := range instances {
			if err := rt.populate(root, instance); err != nil {
				return err
			}
			if root.ptrElems {
				result = reflect.Append(result, instance.value.Addr())
			} else {
				result = reflect.Append(result, instance.value)
			}
		}
		reflect.Indirect(reflect.ValueOf(dest)).Set(result)
		return nil
	}

	if len(instances) == 0 {
		return sql.ErrNoRows
	}
	if len(instances) > 1 {
		return fmt.Errorf("JoinScan got %d root rows, but destination is a single struct", len(instances))
	}

	instance := instances[0]
	if err := rt.populate(root, instance); err != nil {
		return err
	}
	reflect.Indirect(reflect.ValueOf(dest)).Set(instance.value)
	return nil
}

func (rt *joinRuntime) populate(e *joinEntity, instance *joinInstance) error {
	done := rt.populated[e]
	if done == nil {
		done = map[string]bool{}
		rt.populated[e] = done
	}
	if done[instance.key] {
		return nil
	}
	done[instance.key] = true

	data := rt.placement(e)
	for _, relation := range e.relations {
		collection := reflect.MakeSlice(relation.fieldType, 0, 0)
		childKeys := make([]string, 0)
		for childKey := range data.links[relation][instance.key] {
			childKeys = append(childKeys, childKey)
		}

		childInstances := make([]*joinInstance, 0, len(childKeys))
		for _, childKey := range childKeys {
			childInstance := rt.entityData(relation).instances[childKey]
			if childInstance == nil {
				return fmt.Errorf("internal JoinScan error: missing child entity %q", childKey)
			}
			childInstances = append(childInstances, childInstance)
		}
		if err := sortJoinInstances(childInstances); err != nil {
			return err
		}

		for _, childInstance := range childInstances {
			if err := rt.populate(relation, childInstance); err != nil {
				return err
			}
			if relation.ptrElems {
				collection = reflect.Append(collection, childInstance.value.Addr())
			} else {
				collection = reflect.Append(collection, childInstance.value)
			}
		}

		field := reflectx.FieldByIndexes(instance.value, relation.fieldIndex)
		field.Set(collection)
	}
	return nil
}

func sortJoinInstances(instances []*joinInstance) error {
	var sortErr error
	sort.SliceStable(instances, func(i, j int) bool {
		for column := 0; column < len(instances[i].keyValues); column++ {
			less, err := compareJoinValue(instances[i].keyValues[column], instances[j].keyValues[column])
			if err != nil {
				sortErr = err
				return false
			}
			if less != 0 {
				return less < 0
			}
		}
		return false
	})
	return sortErr
}

func compareJoinValue(a, b reflect.Value) (int, error) {
	if !a.IsValid() || !b.IsValid() {
		if !a.IsValid() && !b.IsValid() {
			return 0, nil
		}
		if !a.IsValid() {
			return -1, nil
		}
		return 1, nil
	}

	if at, ok := a.Interface().(time.Time); ok {
		bt := b.Interface().(time.Time)
		if at.Before(bt) {
			return -1, nil
		}
		if bt.Before(at) {
			return 1, nil
		}
		return 0, nil
	}
	if a.Kind() == reflect.Slice && a.Type().Elem().Kind() == reflect.Uint8 {
		return bytes.Compare(a.Bytes(), b.Bytes()), nil
	}

	switch a.Kind() {
	case reflect.Bool:
		av, bv := a.Bool(), b.Bool()
		if av == bv {
			return 0, nil
		}
		if !av {
			return -1, nil
		}
		return 1, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return compareInt64(a.Int(), b.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return compareUint64(a.Uint(), b.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return compareFloat64(a.Float(), b.Float()), nil
	case reflect.String:
		return strings.Compare(a.String(), b.String()), nil
	default:
		return 0, fmt.Errorf("unsupported JoinMap key type %s", a.Type())
	}
}

func compareInt64(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareUint64(a, b uint64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareFloat64(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func encodeJoinKey(values []reflect.Value) (string, error) {
	var builder strings.Builder
	for _, value := range values {
		if !value.IsValid() {
			return "", fmt.Errorf("unsupported nil JoinMap key component")
		}
		if t, ok := value.Interface().(time.Time); ok {
			encoded := t.UTC().Format(time.RFC3339Nano)
			builder.WriteString("t:")
			builder.WriteString(strconv.Itoa(len(encoded)))
			builder.WriteByte(':')
			builder.WriteString(encoded)
			continue
		}

		var encoded string
		switch value.Kind() {
		case reflect.Bool:
			encoded = strconv.FormatBool(value.Bool())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			encoded = strconv.FormatInt(value.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			encoded = strconv.FormatUint(value.Uint(), 10)
		case reflect.Float32, reflect.Float64:
			encoded = strconv.FormatFloat(value.Float(), 'g', -1, 64)
		case reflect.String:
			encoded = value.String()
		case reflect.Slice:
			if value.Type().Elem().Kind() != reflect.Uint8 {
				return "", fmt.Errorf("unsupported JoinMap key type %s", value.Type())
			}
			builder.WriteString("b:")
			builder.WriteString(strconv.Itoa(value.Len()))
			builder.WriteByte(':')
			builder.Write(value.Bytes())
			continue
		default:
			return "", fmt.Errorf("unsupported JoinMap key type %s", value.Type())
		}

		builder.WriteString("s:")
		builder.WriteString(strconv.Itoa(len(encoded)))
		builder.WriteByte(':')
		builder.WriteString(encoded)
	}
	return builder.String(), nil
}

func assignJoinValue(dest reflect.Value, src interface{}) error {
	if !dest.IsValid() || !dest.CanSet() {
		return fmt.Errorf("destination field cannot be set")
	}
	if dest.Kind() == reflect.Ptr {
		if src == nil {
			dest.Set(reflect.Zero(dest.Type()))
			return nil
		}
		if dest.IsNil() {
			dest.Set(reflect.New(dest.Type().Elem()))
		}
		return assignJoinValue(dest.Elem(), src)
	}

	if scanner, ok := dest.Addr().Interface().(sql.Scanner); ok {
		return scanner.Scan(src)
	}
	if src == nil {
		dest.Set(reflect.Zero(dest.Type()))
		return nil
	}

	if t, ok := src.(time.Time); ok {
		if dest.Type() == reflect.TypeOf(time.Time{}) {
			dest.Set(reflect.ValueOf(t))
			return nil
		}
		if dest.Kind() == reflect.String {
			dest.SetString(t.Format(time.RFC3339Nano))
			return nil
		}
		if dest.Kind() == reflect.Slice && dest.Type().Elem().Kind() == reflect.Uint8 {
			dest.SetBytes(t.AppendFormat(nil, time.RFC3339Nano))
			return nil
		}
	}

	source := reflect.ValueOf(src)
	if source.Type().AssignableTo(dest.Type()) {
		if b, ok := src.([]byte); ok {
			copyBytes := append([]byte(nil), b...)
			dest.Set(reflect.ValueOf(copyBytes))
		} else {
			dest.Set(source)
		}
		return nil
	}

	if source.Kind() == dest.Kind() && source.Type().ConvertibleTo(dest.Type()) {
		dest.Set(source.Convert(dest.Type()))
		return nil
	}

	switch dest.Kind() {
	case reflect.Bool:
		value, err := driver.Bool.ConvertValue(src)
		if err != nil {
			return err
		}
		dest.SetBool(value.(bool))
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value, err := strconv.ParseInt(joinString(src), 10, dest.Type().Bits())
		if err != nil {
			return fmt.Errorf("converting driver.Value type %T to %s: %v", src, dest.Kind(), err)
		}
		dest.SetInt(value)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value, err := strconv.ParseUint(joinString(src), 10, dest.Type().Bits())
		if err != nil {
			return fmt.Errorf("converting driver.Value type %T to %s: %v", src, dest.Kind(), err)
		}
		dest.SetUint(value)
		return nil
	case reflect.Float32, reflect.Float64:
		value, err := strconv.ParseFloat(joinString(src), dest.Type().Bits())
		if err != nil {
			return fmt.Errorf("converting driver.Value type %T to %s: %v", src, dest.Kind(), err)
		}
		dest.SetFloat(value)
		return nil
	case reflect.String:
		dest.SetString(joinString(src))
		return nil
	case reflect.Slice:
		if dest.Type().Elem().Kind() == reflect.Uint8 {
			switch value := src.(type) {
			case string:
				dest.SetBytes([]byte(value))
				return nil
			case []byte:
				dest.SetBytes(append([]byte(nil), value...))
				return nil
			}
		}
	case reflect.Interface:
		dest.Set(reflect.ValueOf(src))
		return nil
	}

	return fmt.Errorf("unsupported Scan, storing driver.Value type %T into type %s", src, dest.Type())
}

func joinString(src interface{}) string {
	switch value := src.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(src)
	}
}
