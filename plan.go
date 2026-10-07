package aconfig

import (
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"sync/atomic"
)

// structuralScans counts how many times a config structure type is walked with
// reflection. A plan performs this work exactly once, regardless of how many
// sessions use it.
var structuralScans uint64

// StructuralScans returns how many times config structures have been scanned
// with reflection since the process started. It is mostly useful in tests that
// prove a Plan is reused instead of being re-parsed.
func StructuralScans() uint64 {
	return atomic.LoadUint64(&structuralScans)
}

// PlanConfig binds the options that influence how field names are derived and
// validated. Everything here is computed once by PlanFor and shared safely
// between every session created from the resulting Plan.
//
// Values that depend on a concrete load (the target, files, environment,
// arguments) are intentionally absent; they are provided per session through
// SessionConfig.
type PlanConfig struct {
	// SkipDefaults set to true will not load config from the 'default' tag.
	SkipDefaults bool
	// SkipEnv set to true will not load config from environment variables.
	SkipEnv bool
	// SkipFlags set to true will not load config from flag parameters.
	SkipFlags bool

	EnvPrefix  string // EnvPrefix for environment variables.
	FlagPrefix string // FlagPrefix for flag parameters.

	// FlagDelimiter for flag parameters. If not set - default is ".".
	FlagDelimiter string

	// AllFieldRequired set to true fails a session if any field was not set.
	AllFieldRequired bool

	// AllowDuplicates set to true will not fail on duplicated names (env, flag).
	AllowDuplicates bool

	// AllowUnknownFields set to true will not fail on unknown fields in files.
	AllowUnknownFields bool

	// AllowUnknownEnvs set to true will not fail on unknown environment variables.
	AllowUnknownEnvs bool

	// AllowUnknownFlags set to true will not fail on unknown flag parameters.
	AllowUnknownFlags bool

	// DontGenerateTags disables automatic tag generation for file formats.
	DontGenerateTags bool

	// FileDecoders enable non-JSON formats. The map itself is copied by the
	// plan; decoder instances are cloned or copied for every session (see
	// CloneableFileDecoder). Decoders that cannot be safely reused are rejected.
	FileDecoders map[string]FileDecoder

	// SliceSeparator holds the separator for slice values. Default is ",".
	SliceSeparator string
}

// CloneableFileDecoder is implemented by file decoders that keep mutable state
// and therefore cannot be shared between sessions. Clone must return a fresh,
// zero-value-equivalent decoder; each session calls Init (when present) on it
// with its own file system.
//
// Stateless decoders that are plain struct values (for example the YAML
// submodule decoder) need not implement it: the plan keeps a template and
// creates a per-session copy. Decoders with hidden state that can neither be
// copied nor cloned are rejected by PlanFor instead of being silently shared.
type CloneableFileDecoder interface {
	FileDecoder
	Clone() FileDecoder
}

// Plan is an immutable description of how to load a specific structure type.
// It caches the reflection scan together with the derived and validated names,
// so creating sessions is cheap. A Plan is safe for concurrent use.
//
// A Plan never keeps a concrete target, a "field is set" marker, parsed flag
// state or a previous error; therefore a failed session can never pollute the
// next one.
type Plan struct {
	typ      reflect.Type
	cfg      PlanConfig
	decoders map[string]decoderFactory
	roots    []*planField
	fields   []*planField // leaves, in scan order
}

// PlanFor builds an immutable Plan for the given structure type and the options
// that affect name derivation. The value is used only to learn its type; it is
// not retained. Pass a non-nil pointer to a structure, exactly as for LoaderFor.
func PlanFor(dst any, cfg PlanConfig) (*Plan, error) {
	if dst == nil {
		panic("aconfig: destination cannot be nil")
	}
	value := reflect.ValueOf(dst)
	if value.Kind() != reflect.Ptr {
		panic("aconfig: destination must be a pointer")
	}
	typ := value.Type().Elem()
	if typ.Kind() != reflect.Struct {
		panic("aconfig: destination must be struct")
	}

	p := &Plan{typ: typ}
	if err := p.build(cfg); err != nil {
		return nil, err
	}
	return p, nil
}

// Type returns the structure type the plan was built for.
func (p *Plan) Type() reflect.Type {
	return p.typ
}

// decoderFactory knows how to produce an independent decoder instance for a
// single session. Either a CloneableFileDecoder is used, or a shallow-copyable
// struct template is copied.
type decoderFactory struct {
	format   string
	clone    CloneableFileDecoder
	template reflect.Value // pointer-to-struct or struct value
}

func (f decoderFactory) newSessionDecoder(fsys fs.FS) (FileDecoder, error) {
	var dec FileDecoder

	switch {
	case f.clone != nil:
		cloned := f.clone.Clone()
		if cloned == nil {
			return nil, fmt.Errorf("decoder %q returned nil from Clone", f.format)
		}
		dec = cloned
	case f.template.IsValid():
		template := f.template
		if template.Kind() == reflect.Ptr {
			clone := reflect.New(template.Elem().Type())
			clone.Elem().Set(template.Elem())
			dec = clone.Interface().(FileDecoder)
		} else {
			dec = template.Interface().(FileDecoder)
		}
	default:
		return nil, fmt.Errorf("file format %q decoder cannot be reused across sessions", f.format)
	}

	if initer, ok := dec.(interface{ Init(fs.FS) }); ok {
		initer.Init(fsys)
	}
	return dec, nil
}

func (p *Plan) build(cfg PlanConfig) error {
	if cfg.FlagDelimiter == "" {
		cfg.FlagDelimiter = "."
	}
	if cfg.EnvPrefix != "" {
		cfg.EnvPrefix += "_"
	}
	if cfg.FlagPrefix != "" {
		cfg.FlagPrefix += cfg.FlagDelimiter
	}
	if cfg.SliceSeparator == "" {
		cfg.SliceSeparator = ","
	}

	factories := make(map[string]decoderFactory, len(cfg.FileDecoders)+1)
	addDecoder := func(ext string, dec FileDecoder) error {
		factory := decoderFactory{format: dec.Format()}

		if cloneable, ok := dec.(CloneableFileDecoder); ok {
			factory.clone = cloneable
		} else {
			rv := reflect.ValueOf(dec)
			switch {
			case rv.Kind() == reflect.Ptr && rv.Elem().Kind() == reflect.Struct,
				rv.Kind() == reflect.Struct:
				factory.template = rv
			default:
				return fmt.Errorf("file format %q decoder %T cannot be safely reused; implement aconfig.CloneableFileDecoder", ext, dec)
			}
		}

		factories[ext] = factory
		return nil
	}

	if err := addDecoder(".json", &jsonDecoder{}); err != nil {
		return err
	}
	for ext, dec := range cfg.FileDecoders {
		if dec == nil {
			return fmt.Errorf("file format %q decoder is nil", ext)
		}
		if err := addDecoder(ext, dec); err != nil {
			return err
		}
	}

	p.cfg = cfg
	p.decoders = factories

	// The single reflection scan of the structure type for this plan.
	atomic.AddUint64(&structuralScans, 1)
	roots, err := p.scan(reflect.New(p.typ).Elem(), nil)
	if err != nil {
		return err
	}
	p.roots = roots

	if !cfg.AllowDuplicates {
		if err := p.checkDuplicates(); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plan) checkDuplicates() error {
	if !p.cfg.SkipEnv {
		seen := map[string]struct{}{}
		for _, leaf := range p.fields {
			name := leaf.tags["env"]
			if name == "" {
				continue
			}
			name = p.cfg.EnvPrefix + name
			if _, ok := seen[name]; ok {
				return fmt.Errorf("field %q is duplicated", name)
			}
			seen[name] = struct{}{}
		}
	}
	if !p.cfg.SkipFlags {
		seen := map[string]struct{}{}
		for _, leaf := range p.fields {
			name := leaf.tags["flag"]
			if name == "" {
				continue
			}
			name = p.cfg.FlagPrefix + name
			if _, ok := seen[name]; ok {
				return fmt.Errorf("duplicate flag %q", name)
			}
			seen[name] = struct{}{}
		}
	}
	return nil
}

// planField is the immutable per-field metadata produced by the single scan.
type planField struct {
	field    reflect.StructField
	parent   *planField // logical parent used when joining names (embedded flattened)
	required bool

	isStruct bool
	children []*planField

	// base holds the field's own names before parent joining, generated when
	// the struct tag is absent (mirrors Loader.tagsForField).
	base map[string]string
	// tags holds resolved leaf names. For env/flag the value is parent-joined
	// but prefix-free (""/"-" semantics already applied); the same is true for
	// file format tags. Keys "default" and "usage" hold the raw struct tags.
	tags map[string]string
}

func (p *Plan) scan(object reflect.Value, parent *planField) ([]*planField, error) {
	typ := object.Type()
	nodes := make([]*planField, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		value := object.Field(i)
		if field.Anonymous {
			// Anonymous (embedded) fields are traversed even when the field
			// itself is unexported; promoted fields remain settable.
		} else if !value.CanSet() {
			continue // non-embedded unexported field
		}
		node := &planField{
			field:    field,
			parent:   parent,
			required: isRequiredTag(field),
		}

		fieldType := field.Type
		for fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}

		if fieldType.Kind() == reflect.Struct {
			node.isStruct = true
			node.base = p.baseNameTags(field)

			// Mirror getFieldsHelper: allocate pointer-to-struct fields and
			// flatten anonymous (embedded) structs.
			if field.Type.Kind() == reflect.Ptr {
				value.Set(reflect.New(field.Type.Elem()))
				value = value.Elem()
			}
			childParent := node
			if field.Anonymous {
				childParent = parent
			}
			children, err := p.scan(value, childParent)
			if err != nil {
				return nil, err
			}
			node.children = children
			nodes = append(nodes, node)
			continue
		}

		node.tags = p.leafTags(node, field)
		p.fields = append(p.fields, node)
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func isRequiredTag(field reflect.StructField) bool {
	value := field.Tag.Get("required")
	if value != "" && value != "true" {
		panic(fmt.Sprintf("aconfig: incorrect value for 'required' tag: %v", value))
	}
	return value == "true"
}

func (p *Plan) leafTags(node *planField, field reflect.StructField) map[string]string {
	node.base = p.baseNameTags(field)

	tags := map[string]string{
		"default": field.Tag.Get("default"),
		"usage":   field.Tag.Get("usage"),
	}
	for tag, value := range node.base {
		tags[tag] = value
	}

	// Resolve parent chains once; the result has no env/flag prefix so a
	// session can reuse Loader.fullTag with the plan-level prefixes.
	for _, tag := range p.nameTags() {
		tags[tag] = p.resolveTag(tag, tags[tag], node.parent)
	}
	return tags
}

func (p *Plan) baseNameTags(field reflect.StructField) map[string]string {
	words := splitNameByWords(field.Name)
	base := map[string]string{
		"env":  p.makeBaseTag(field, "env", words),
		"flag": p.makeBaseTag(field, "flag", words),
	}
	for _, factory := range p.decoders {
		base[factory.format] = p.makeBaseTag(field, factory.format, words)
	}
	return base
}

func (p *Plan) nameTags() []string {
	tags := make([]string, 0, len(p.decoders)+2)
	tags = append(tags, "env", "flag")
	for _, factory := range p.decoders {
		tags = append(tags, factory.format)
	}
	return tags
}

func (p *Plan) makeBaseTag(field reflect.StructField, tag string, words []string) string {
	if value := field.Tag.Get(tag); value != "" {
		return value
	}
	if p.cfg.DontGenerateTags {
		return field.Name
	}
	name := strings.Join(words, "_")
	if tag == "env" {
		return strings.ToUpper(name)
	}
	return strings.ToLower(name)
}

// resolveTag mirrors Loader.fullTag minus the env/flag prefix, operating on the
// immutable plan metadata instead of per-session values.
func (p *Plan) resolveTag(tag, value string, parent *planField) string {
	sep := "."
	switch tag {
	case "env":
		sep = "_"
	case "flag":
		sep = p.cfg.FlagDelimiter
	}

	if value == "-" {
		return ""
	}
	if before, _, ok := cut(value, ",exact"); ok {
		return before
	}
	if before, _, ok := cut(value, ",omitempty"); ok {
		value = before
	}

	for ancestor := parent; ancestor != nil; ancestor = ancestor.parent {
		own := ancestor.ownTag(tag)
		if own != "-" {
			value = own + sep + value
		}
	}
	return value
}

// ownTag returns the field's non-resolved tag value; planField.tags stores the
// resolved value, so the raw value is reconstructed here.
func (f *planField) ownTag(tag string) string {
	return f.base[tag]
}

func (p *Plan) baseConfig() Config {
	return Config{
		SkipDefaults:       p.cfg.SkipDefaults,
		SkipEnv:            p.cfg.SkipEnv,
		SkipFlags:          p.cfg.SkipFlags,
		EnvPrefix:          p.cfg.EnvPrefix,
		FlagPrefix:         p.cfg.FlagPrefix,
		envDelimiter:       "_",
		FlagDelimiter:      p.cfg.FlagDelimiter,
		AllFieldRequired:   p.cfg.AllFieldRequired,
		AllowDuplicates:    p.cfg.AllowDuplicates,
		AllowUnknownFields: p.cfg.AllowUnknownFields,
		AllowUnknownEnvs:   p.cfg.AllowUnknownEnvs,
		AllowUnknownFlags:  p.cfg.AllowUnknownFlags,
		DontGenerateTags:   p.cfg.DontGenerateTags,
		SliceSeparator:     p.cfg.SliceSeparator,
	}
}
