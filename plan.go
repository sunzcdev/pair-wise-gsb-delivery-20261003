package aconfig

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"sync/atomic"
)

// FileDecoderCloner is an optional interface for FileDecoder implementations
// that keep mutable state (for example a filesystem set via Init).
//
// A Plan is shared by many load sessions, often concurrently, so a stateful
// decoder cannot be reused as is: NewPlan rejects stateful decoders that do
// not implement FileDecoderCloner. For decoders that do, every session gets
// its own clone which is then initialized with the session filesystem.
type FileDecoderCloner interface {
	CloneFileDecoder() FileDecoder
}

// Plan is an immutable, pre-parsed description of a configuration struct.
//
// NewPlan performs the reflection scan of the struct and validates the
// resulting names (environment variables, flags, file tags) exactly once.
// The plan is bound to the struct type and to the name-affecting options:
// EnvPrefix, FlagPrefix, FlagDelimiter, DontGenerateTags, AllowDuplicates,
// SkipFlags, FileFlag and FileDecoders.
//
// A Plan holds no destination, no parsed flags and no load errors, so a
// failed Load in one session cannot pollute another. It is safe for
// concurrent use and creates any number of independent load sessions via
// LoaderFor. Each session gets its own destination, environment, args,
// flag.FlagSet and decoder copies.
type Plan struct {
	typ    reflect.Type // pointer to the struct type the plan is bound to
	cfg    Config       // normalized name-affecting options only
	fields []*planField // leaf fields, immutable after NewPlan
}

// structScanCount counts struct fields scanned by NewPlan. It is used in
// tests to prove that sessions created from a Plan never re-scan the struct.
var structScanCount int64

// NewPlan scans the given struct type once and returns an immutable Plan
// that can create independent load sessions. The sample may be a nil pointer
// to the struct, only its type is used.
//
// Runtime options of cfg (Files, Envs, Args, FileSystem, SkipDefaults,
// SkipFiles, SkipEnv, AllowUnknown*, AllFieldRequired, FailOnFileNotFound,
// MergeFiles) are not stored in the Plan, pass them to LoaderFor instead.
// NewParser is not supported.
func NewPlan(sample any, cfg Config) (*Plan, error) {
	typ := reflect.TypeOf(sample)
	if typ == nil || typ.Kind() != reflect.Ptr || typ.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("aconfig: plan sample must be a pointer to struct, got %v", typ)
	}
	if cfg.NewParser {
		return nil, fmt.Errorf("aconfig: Plan does not support NewParser")
	}

	ncfg, err := normalizePlanConfig(cfg)
	if err != nil {
		return nil, err
	}

	p := &Plan{typ: typ, cfg: ncfg}
	scanPlanFields(&p.cfg, typ.Elem(), nil, nil, &p.fields)

	if err := p.validateFlagNames(); err != nil {
		return nil, err
	}
	return p, nil
}

// LoaderFor creates a new independent load session bound to dst, which must
// be a non-nil pointer to the struct type the Plan was built for. The given
// Config may only set runtime options (Files, Envs, Args, FileSystem,
// SkipDefaults, SkipFiles, SkipEnv, AllFieldRequired, AllowUnknownFields,
// AllowUnknownEnvs, AllowUnknownFlags, FailOnFileNotFound, MergeFiles,
// SliceSeparator); name-affecting options are taken from the Plan.
func (p *Plan) LoaderFor(dst any, cfg Config) (*Loader, error) {
	typ := reflect.TypeOf(dst)
	if typ == nil || typ.Kind() != reflect.Ptr || typ.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("aconfig: destination must be a pointer to struct, got %v", typ)
	}
	if typ != p.typ {
		return nil, fmt.Errorf("aconfig: plan is bound to %v, cannot load into %v", p.typ, typ)
	}
	if reflect.ValueOf(dst).IsNil() {
		return nil, fmt.Errorf("aconfig: destination cannot be nil")
	}

	sc, err := p.sessionConfig(cfg)
	if err != nil {
		return nil, err
	}

	l := &Loader{dst: dst, config: sc}
	l.fsys = &fsOrOS{sc.FileSystem}

	// Decoders are session state: stateful ones are cloned, stateless ones
	// are shared, and each session initializes its own filesystem.
	l.config.FileDecoders = make(map[string]FileDecoder, len(p.cfg.FileDecoders))
	for ext, dec := range p.cfg.FileDecoders {
		if cloner, ok := dec.(FileDecoderCloner); ok {
			dec = cloner.CloneFileDecoder()
		}
		if ini, ok := dec.(interface{ Init(fs.FS) }); ok {
			ini.Init(l.fsys)
		}
		l.config.FileDecoders[ext] = dec
	}

	l.fields = p.bind(dst)

	// flag.FlagSet and its flag.Value state are per session.
	l.flagSet = flag.NewFlagSet(sc.FlagPrefix, flag.ContinueOnError)
	if !sc.SkipFlags {
		for _, field := range l.fields {
			flagName := l.fullTag(sc.FlagPrefix, field, "flag")
			if flagName == "" {
				continue
			}
			l.flagSet.String(flagName, field.Tag("default"), field.Tag("usage"))
		}
	}
	if sc.FileFlag != "" {
		l.flagSet.String(sc.FileFlag, "", "config file param")
	}
	return l, nil
}

// normalizePlanConfig keeps only name-affecting options, normalizes them and
// validates that all decoders are safe to share between sessions.
func normalizePlanConfig(cfg Config) (Config, error) {
	cfg.envDelimiter = "_"
	if cfg.FlagDelimiter == "" {
		cfg.FlagDelimiter = "."
	}
	if cfg.EnvPrefix != "" {
		cfg.EnvPrefix += cfg.envDelimiter
	}
	if cfg.FlagPrefix != "" {
		cfg.FlagPrefix += cfg.FlagDelimiter
	}
	if cfg.SliceSeparator == "" {
		cfg.SliceSeparator = ","
	}

	decoders := make(map[string]FileDecoder, len(cfg.FileDecoders)+1)
	for ext, dec := range cfg.FileDecoders {
		if dec == nil {
			return Config{}, fmt.Errorf("aconfig: decoder for %q is nil", ext)
		}
		if _, stateful := dec.(interface{ Init(fs.FS) }); stateful {
			if _, cloner := dec.(FileDecoderCloner); !cloner {
				return Config{}, fmt.Errorf("aconfig: decoder for %q keeps mutable state (implements Init) and cannot be shared between sessions: implement aconfig.FileDecoderCloner or use a stateless decoder", ext)
			}
		}
		decoders[ext] = dec
	}
	if _, ok := decoders[".json"]; !ok {
		decoders[".json"] = &jsonDecoder{}
	}
	cfg.FileDecoders = decoders

	// Runtime options are per-session, the plan must not keep them.
	cfg.Files = nil
	cfg.Envs = nil
	cfg.Args = nil
	cfg.FileSystem = nil
	return cfg, nil
}

// sessionConfig merges per-session runtime options into the plan snapshot
// and rejects name-affecting options, which are bound to the plan.
func (p *Plan) sessionConfig(cfg Config) (Config, error) {
	if cfg.NewParser {
		return Config{}, fmt.Errorf("aconfig: Plan does not support NewParser")
	}
	if cfg.EnvPrefix != "" || cfg.FlagPrefix != "" || cfg.FlagDelimiter != "" ||
		cfg.DontGenerateTags || cfg.AllowDuplicates || cfg.SkipFlags ||
		cfg.FileFlag != "" || cfg.FileDecoders != nil {
		return Config{}, fmt.Errorf("aconfig: EnvPrefix, FlagPrefix, FlagDelimiter, DontGenerateTags, AllowDuplicates, SkipFlags, FileFlag and FileDecoders affect names and are bound to the plan: set them in NewPlan")
	}

	sc := p.cfg
	sc.SkipDefaults = cfg.SkipDefaults
	sc.SkipFiles = cfg.SkipFiles
	sc.SkipEnv = cfg.SkipEnv
	sc.AllFieldRequired = cfg.AllFieldRequired
	sc.AllowUnknownFields = cfg.AllowUnknownFields
	sc.AllowUnknownEnvs = cfg.AllowUnknownEnvs
	sc.AllowUnknownFlags = cfg.AllowUnknownFlags
	sc.FailOnFileNotFound = cfg.FailOnFileNotFound
	sc.MergeFiles = cfg.MergeFiles
	sc.FileSystem = cfg.FileSystem
	sc.Files = cfg.Files
	sc.Envs = cfg.Envs
	sc.Args = cfg.Args
	if cfg.SliceSeparator != "" {
		sc.SliceSeparator = cfg.SliceSeparator
	}
	if sc.Envs == nil {
		sc.Envs = os.Environ()
	}
	if sc.Args == nil {
		sc.Args = os.Args[1:]
	}
	return sc, nil
}

// validateFlagNames mirrors the duplicate flag check of Loader.init, but runs
// once at plan time instead of on every load.
func (p *Plan) validateFlagNames() error {
	if p.cfg.SkipFlags {
		return nil
	}
	names := make(map[string]bool, len(p.fields))
	for _, field := range p.fields {
		flagName := fullTag(&p.cfg, p.cfg.FlagPrefix, field, "flag")
		if flagName == "" {
			continue
		}
		if names[flagName] && !p.cfg.AllowDuplicates {
			return fmt.Errorf("duplicate flag %q", flagName)
		}
		names[flagName] = true
	}
	return nil
}

// planField is the immutable, type-only counterpart of fieldData.
type planField struct {
	name       string
	index      []int // field index path from the root struct
	field      reflect.StructField
	parent     *planField
	isRequired bool
	tags       map[string]string
}

func (f *planField) Tag(tag string) string {
	if t, ok := f.tags[tag]; ok {
		return t
	}
	return f.field.Tag.Get(tag)
}

func (f *planField) metaParent() fieldMeta {
	if f.parent == nil {
		return nil
	}
	return f.parent
}

// scanPlanFields mirrors Loader.getFieldsHelper but works on the struct type
// only, so the result can be shared by any number of load sessions.
func scanPlanFields(cfg *Config, typ reflect.Type, parent *planField, index []int, out *[]*planField) {
	for i := 0; i < typ.NumField(); i++ {
		atomic.AddInt64(&structScanCount, 1)

		field := typ.Field(i)
		if field.PkgPath != "" { // unexported, mirrors !CanSet in getFieldsHelper
			continue
		}

		idx := make([]int, len(index)+1)
		copy(idx, index)
		idx[len(index)] = i

		requiredTag := field.Tag.Get("required")
		if requiredTag != "" && requiredTag != "true" {
			panic(fmt.Sprintf("aconfig: incorrect value for 'required' tag: %v", requiredTag))
		}

		pf := &planField{
			name:       planFieldName(field.Name, parent),
			index:      idx,
			parent:     parent,
			field:      field,
			isRequired: requiredTag == "true",
			tags:       tagsForField(cfg, field),
		}

		kind := field.Type.Kind()
		if kind == reflect.Ptr {
			kind = field.Type.Elem().Kind()
		}
		if kind == reflect.Struct {
			sub := pf
			if field.Anonymous {
				sub = parent
			}
			scanPlanFields(cfg, derefType(field.Type), sub, idx, out)
			continue
		}
		*out = append(*out, pf)
	}
}

func planFieldName(name string, parent *planField) string {
	if parent == nil {
		return name
	}
	return parent.name + "." + name
}

func derefType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ
}

// bind attaches the immutable plan fields to a concrete destination,
// allocating nil struct pointers on the way, exactly like getFieldsHelper
// does during a regular scan.
func (p *Plan) bind(dst any) []*fieldData {
	root := reflect.ValueOf(dst)
	for root.Type().Kind() == reflect.Ptr {
		root = root.Elem()
	}

	bound := make(map[*planField]*fieldData, len(p.fields))
	var bindField func(pf *planField) *fieldData
	bindField = func(pf *planField) *fieldData {
		if fd, ok := bound[pf]; ok {
			return fd
		}
		var parent *fieldData
		if pf.parent != nil {
			parent = bindField(pf.parent)
		}
		fd := &fieldData{
			name:       pf.name,
			parent:     parent,
			field:      pf.field,
			isRequired: pf.isRequired,
			tags:       pf.tags,
		}
		if pf.index != nil {
			fd.value = fieldByIndexAlloc(root, pf.index)
		}
		bound[pf] = fd
		return fd
	}

	fields := make([]*fieldData, 0, len(p.fields))
	for _, pf := range p.fields {
		fields = append(fields, bindField(pf))
	}
	return fields
}

func fieldByIndexAlloc(v reflect.Value, index []int) reflect.Value {
	for _, i := range index {
		for v.Kind() == reflect.Ptr {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v
}
