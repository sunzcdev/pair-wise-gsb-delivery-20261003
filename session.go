package aconfig

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"reflect"
)

// ErrSessionLoaded is returned when a Session is loaded more than once.
// Create a new session for every load so that one failure cannot pollute the
// next attempt.
var ErrSessionLoaded = errors.New("aconfig: session has already been loaded")

// SessionConfig holds the values that belong to a single load: the target
// structure, the file system and files to read, the environment and the
// command-line arguments. Nothing here is shared between sessions.
type SessionConfig struct {
	// SkipFiles set to true will not load config from files for this session.
	SkipFiles bool

	// FailOnFileNotFound stops the session on the first missing file.
	FailOnFileNotFound bool

	// MergeFiles collects all entries from all given files.
	MergeFiles bool

	// FileFlag is the name of a flag that carries the config file path.
	FileFlag string

	// FileSystem from which files are loaded. Default is nil (OS file system).
	FileSystem fs.FS

	// Files from which config is loaded.
	Files []string

	// Envs holds the environment variables ("K=V"). nil means os.Environ().
	Envs []string

	// Args holds the command-line arguments. nil means os.Args[1:].
	Args []string
}

// Session is an independent configuration load created from an immutable Plan.
//
// Each session owns its target, its flag set (including any flag.Value state),
// its file decoders and its error state. Sessions do not share mutable data and
// are safe to run concurrently as long as their targets differ.
type Session struct {
	plan *Plan
	cfg  SessionConfig

	loader *Loader
	used   bool
}

// NewSession creates an independent load for the given target. The target must
// be a pointer to the same structure type the plan was built for.
func (p *Plan) NewSession(dst any, cfg SessionConfig) (*Session, error) {
	if dst == nil {
		return nil, errors.New("aconfig: destination cannot be nil")
	}
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Struct {
		return nil, errors.New("aconfig: destination must be a pointer to a struct")
	}
	if rv.Elem().Type() != p.typ {
		return nil, fmt.Errorf("aconfig: session target %s does not match plan type %s", rv.Elem().Type(), p.typ)
	}

	if cfg.Envs == nil {
		cfg.Envs = os.Environ()
	} else {
		cfg.Envs = cloneStrings(cfg.Envs)
	}
	if cfg.Args == nil {
		cfg.Args = os.Args[1:]
	} else {
		cfg.Args = cloneStrings(cfg.Args)
	}
	cfg.Files = cloneStrings(cfg.Files)

	loader, err := p.newLoader(dst, cfg)
	if err != nil {
		return nil, err
	}
	return &Session{plan: p, cfg: cfg, loader: loader}, nil
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func (p *Plan) newLoader(dst any, sc SessionConfig) (*Loader, error) {
	config := p.baseConfig()
	config.SkipFiles = sc.SkipFiles
	config.FailOnFileNotFound = sc.FailOnFileNotFound
	config.MergeFiles = sc.MergeFiles
	config.FileFlag = sc.FileFlag
	config.Files = sc.Files
	config.Envs = sc.Envs
	config.Args = sc.Args

	fsys := &fsOrOS{FS: sc.FileSystem}

	decoders := make(map[string]FileDecoder, len(p.decoders))
	for ext, factory := range p.decoders {
		dec, err := factory.newSessionDecoder(fsys)
		if err != nil {
			return nil, fmt.Errorf("init file decoder: %w", err)
		}
		decoders[ext] = dec
	}
	config.FileDecoders = decoders
	config.FileSystem = sc.FileSystem

	l := &Loader{
		config: config,
		dst:    dst,
		fsys:   fsys,
	}

	// Materialize per-session field data from the immutable plan without
	// scanning the type again.
	fields, err := p.materialize(dst)
	if err != nil {
		return nil, err
	}
	l.fields = fields

	l.flagSet = flag.NewFlagSet(config.FlagPrefix, flag.ContinueOnError)
	if !config.SkipFlags {
		names := make(map[string]bool, len(fields))
		for _, field := range fields {
			flagName := l.fullTag(config.FlagPrefix, field, "flag")
			if flagName == "" {
				continue
			}
			if names[flagName] && !config.AllowDuplicates {
				return nil, fmt.Errorf("duplicate flag %q", flagName)
			}
			names[flagName] = true
			l.flagSet.String(flagName, field.Tag("default"), field.Tag("usage"))
		}
	}
	if config.FileFlag != "" {
		l.flagSet.String(config.FileFlag, "", "config file param")
	}

	return l, nil
}

// materialize walks the plan tree against the concrete target, allocating
// pointer-to-struct fields exactly like getFieldsHelper, and builds the flat
// list of per-session field data. The immutable plan provides names and tags.
func (p *Plan) materialize(dst any) ([]*fieldData, error) {
	value := reflect.ValueOf(dst)
	for value.Kind() == reflect.Ptr {
		value = value.Elem()
	}

	out := make([]*fieldData, 0, len(p.fields))
	var walk func(nodes []*planField, object reflect.Value, parent *fieldData) error
	walk = func(nodes []*planField, object reflect.Value, parent *fieldData) error {
		for _, node := range nodes {
			fieldValue := object.FieldByName(node.field.Name)

			if node.isStruct {
				var nextObject reflect.Value
				if node.field.Type.Kind() == reflect.Ptr {
					if fieldValue.IsNil() {
						fieldValue.Set(reflect.New(node.field.Type.Elem()))
					}
					nextObject = fieldValue.Elem()
				} else {
					nextObject = fieldValue
				}

				nextParent := parent
				if !node.field.Anonymous {
					nextParent = p.newStructFieldData(node, fieldValue, parent)
				}
				if err := walk(node.children, nextObject, nextParent); err != nil {
					return err
				}
				continue
			}

			out = append(out, p.newFieldData(node, fieldValue, parent))
		}
		return nil
	}
	if err := walk(p.roots, value, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// newFieldData builds a per-session field data backed by immutable plan tags.
func (p *Plan) newFieldData(node *planField, value reflect.Value, parent *fieldData) *fieldData {
	name := node.field.Name
	if parent != nil {
		name = makeName(name, parent)
	}
	return &fieldData{
		name:         name,
		field:        node.field,
		value:        value,
		isRequired:   node.required,
		tags:         copyTags(node.tags),
		resolvedTags: true,
	}
}

// newStructFieldData mirrors Loader.newFieldData for intermediate struct nodes
// used only to build the parent chain and the Go field name path.
func (p *Plan) newStructFieldData(node *planField, value reflect.Value, parent *fieldData) *fieldData {
	name := node.field.Name
	if parent != nil {
		name = makeName(name, parent)
	}
	return &fieldData{
		name:   name,
		parent: parent,
		field:  node.field,
		value:  value,
		tags:   copyTags(node.base),
	}
}

func copyTags(tags map[string]string) map[string]string {
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}

// Flags returns the session's own flag set. Every session gets a fresh set, so
// flag.Value state can never leak between loads.
func (s *Session) Flags() *flag.FlagSet {
	return s.loader.Flags()
}

// WalkFields iterates over the configuration fields of this session.
func (s *Session) WalkFields(fn func(f Field) bool) {
	s.loader.WalkFields(fn)
}

// Load runs the configuration load for this session.
func (s *Session) Load() error {
	if s.used {
		return ErrSessionLoaded
	}
	s.used = true
	return s.loader.Load()
}
