package aconfig

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Loader of user configuration.
type Loader struct {
	config  Config
	dst     any
	parser  *structParser
	fields  []*fieldData
	fsys    fs.FS
	flagSet *flag.FlagSet
	errInit error
}

// Config to configure configuration loader.
type Config struct {
	// NewParser set to true enables a new and better struct parser.
	// Default is false because there might be bugs.
	// In the future new parser will be enabled by default.
	NewParser bool

	SkipDefaults bool // SkipDefaults set to true will not load config from 'default' tag.
	SkipFiles    bool // SkipFiles set to true will not load config from files.
	SkipEnv      bool // SkipEnv set to true will not load config from environment variables.
	SkipFlags    bool // SkipFlags set to true will not load config from flag parameters.

	EnvPrefix  string // EnvPrefix for environment variables.
	FlagPrefix string // FlagPrefix for flag parameters.

	// envDelimiter for environment variables. Is always "_" due to env-var format.
	// Also unexported cause there is no sense to change it.
	envDelimiter string

	FlagDelimiter string // FlagDelimiter for flag parameters. If not set - default is ".".

	// AllFieldsRequired set to true will fail config loading if one of the fields was not set.
	// File, environment, flag must provide a value for the field.
	// If default is set and this option is enabled (or required tag is set) there will be an error.
	AllFieldRequired bool

	// AllowDuplicates set to true will not fail on duplicated names on fields (env, flag, etc...)
	AllowDuplicates bool

	// AllowUnknownFields set to true will not fail on unknown fields in files.
	AllowUnknownFields bool

	// AllowUnknownEnvs set to true will not fail on unknown environment variables ().
	// When false error is returned only when EnvPrefix isn't empty.
	AllowUnknownEnvs bool

	// AllowUnknownFlags set to true will not fail on unknown flag parameters ().
	// When false error is returned only when FlagPrefix isn't empty.
	AllowUnknownFlags bool

	// DontGenerateTags disables tag generation for JSON, YAML, TOML file formats.
	DontGenerateTags bool

	// FailOnFileNotFound will stop Loader on a first not found file from Files field in this structure.
	FailOnFileNotFound bool

	// FileSystem from which files will be loaded. Default is nil (OS file system).
	FileSystem fs.FS

	// MergeFiles set to true will collect all the entries from all the given files.
	// Easy wat to cobine base.yaml with prod.yaml
	MergeFiles bool

	// FileFlag the name of the flag that defines the path to the configuration file passed through the CLI.
	// (To make it easier to transfer the config file via flags.)
	FileFlag string

	// Files from which config should be loaded.
	Files []string

	// Envs hold the environment variable from which envs will be parsed.
	// By default is nil and then os.Environ() will be used.
	Envs []string

	// Args hold the command-line arguments from which flags will be parsed.
	// By default is nil and then os.Args will be used.
	// Unless loader.Flags() will be explicitly parsed by the user.
	Args []string

	// FileDecoders to enable other than JSON file formats and prevent additional dependencies.
	// Add required submodules to the go.mod and register them in this field.
	// Example:
	//	FileDecoders: map[string]aconfig.FileDecoder{
	//		".yaml": aconfigyaml.New(),
	//		".toml": aconfigtoml.New(),
	//		".env": aconfigdotenv.New(),
	// 	}
	FileDecoders map[string]FileDecoder

	// SliceSeparator hold the separator for slice values. Default is ",".
	SliceSeparator string
}

// FileDecoder is used to read config from files. See aconfig submodules.
type FileDecoder interface {
	Format() string
	DecodeFile(filename string) (map[string]any, error)
	// Init(fsys fs.FS)
}

// FileDecoderIncludes is an optional extension of FileDecoder that enables
// explicit includes: a config file can reference other config files.
//
// Referenced files are resolved relative to the directory of the file that
// references them and are loaded before it, so the referencing file overrides
// its dependencies. Each file is decoded at most once per Load call, cyclic
// includes are reported with the full reference chain and paths cannot escape
// the root of the configured file system.
type FileDecoderIncludes interface {
	// Includes returns the files referenced by the decoded content of a
	// config file. It may remove include-related keys from fields, so they
	// are not treated as unknown fields.
	Includes(fields map[string]any) ([]string, error)
}

// Field of the user configuration structure.
// Done as an interface to export less things in lib.
type Field interface {
	// Name of the field.
	Name() string

	// Tag returns a given tag for a field.
	Tag(tag string) string

	// Parent of the current node.
	Parent() (Field, bool)
}

// LoaderFor creates a new Loader based on a given configuration structure.
// Supports only non-nil structures.
func LoaderFor(dst any, cfg Config) *Loader {
	assertStruct(dst)

	l := &Loader{
		dst:    dst,
		config: cfg,
	}
	l.init()
	return l
}

func (l *Loader) init() {
	l.config.envDelimiter = "_"

	if l.config.FlagDelimiter == "" {
		l.config.FlagDelimiter = "."
	}

	if l.config.EnvPrefix != "" {
		l.config.EnvPrefix += l.config.envDelimiter
	}
	if l.config.FlagPrefix != "" {
		l.config.FlagPrefix += l.config.FlagDelimiter
	}

	l.fsys = &fsOrOS{l.config.FileSystem}

	if _, ok := l.config.FileDecoders[".json"]; !ok {
		if l.config.FileDecoders == nil {
			l.config.FileDecoders = map[string]FileDecoder{}
		}
		l.config.FileDecoders[".json"] = &jsonDecoder{}
	}
	for _, dec := range l.config.FileDecoders {
		dec, ok := dec.(interface{ Init(fs.FS) })
		if !ok {
			continue
		}
		dec.Init(l.fsys)
	}

	if l.config.Envs == nil {
		l.config.Envs = os.Environ()
	}
	if l.config.Args == nil {
		l.config.Args = os.Args[1:]
	}

	if l.config.NewParser {
		l.parser = newStructParser(l.config)
		if err := l.parser.parseStruct(l.dst); err != nil {
			l.errInit = err
			return
		}
	} else {
		l.fields = l.getFields(l.dst)
	}

	l.flagSet = flag.NewFlagSet(l.config.FlagPrefix, flag.ContinueOnError)
	if !l.config.SkipFlags {
		names := make(map[string]bool, len(l.fields))
		if l.config.NewParser {
			l.flagSet = l.parser.flagSet
		} else {
			for _, field := range l.fields {
				flagName := l.fullTag(l.config.FlagPrefix, field, "flag")
				if flagName == "" {
					continue
				}
				if names[flagName] && !l.config.AllowDuplicates {
					l.errInit = fmt.Errorf("duplicate flag %q", flagName)
					return
				}
				names[flagName] = true
				l.flagSet.String(flagName, field.Tag("default"), field.Tag("usage"))
			}
		}
	}

	if l.config.FileFlag != "" {
		// TODO: should be prefixed ?
		l.flagSet.String(l.config.FileFlag, "", "config file param")
	}

	if l.config.SliceSeparator == "" {
		l.config.SliceSeparator = ","
	}
}

// Flags returngs flag.FlagSet to create your own flags.
// FlagSet name is Config.FlagPrefix and error handling is set to ContinueOnError.
func (l *Loader) Flags() *flag.FlagSet {
	return l.flagSet
}

// WalkFields iterates over configuration fields.
// Easy way to create documentation or user-friendly help.
func (l *Loader) WalkFields(fn func(f Field) bool) {
	for _, f := range l.fields {
		if !fn(f) {
			return
		}
	}
}

// Load configuration into a given param.
func (l *Loader) Load() error {
	if l.errInit != nil {
		return fmt.Errorf("init loader: %w", l.errInit)
	}
	if err := l.loadConfig(); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return nil
}

func (l *Loader) loadConfig() error {
	if err := l.parseFlags(); err != nil {
		return err
	}
	if err := l.loadSources(); err != nil {
		return err
	}
	if err := l.checkRequired(); err != nil {
		return err
	}
	return nil
}

func (l *Loader) parseFlags() error {
	// TODO: too simple?
	if l.flagSet.Parsed() || l.config.SkipFlags {
		return nil
	}
	return l.flagSet.Parse(l.config.Args)
}

func (l *Loader) loadSources() error {
	if !l.config.SkipDefaults {
		if err := l.loadDefaults(); err != nil {
			return fmt.Errorf("load defaults: %w", err)
		}
	}
	if !l.config.SkipFiles {
		if err := l.loadFiles(); err != nil {
			return fmt.Errorf("load files: %w", err)
		}
	}
	if !l.config.SkipEnv {
		if err := l.loadEnvironment(); err != nil {
			return fmt.Errorf("load environment: %w", err)
		}
	}
	if !l.config.SkipFlags {
		if err := l.loadFlags(); err != nil {
			return fmt.Errorf("load flags: %w", err)
		}
	}

	if l.config.NewParser {
		if err := l.parser.apply(l.dst); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
	}
	return nil
}

func (l *Loader) checkRequired() error {
	missedFields := []string{}
	for _, field := range l.fields {
		if field.isSet {
			continue
		}
		if field.isRequired || l.config.AllFieldRequired {
			missedFields = append(missedFields, field.name)
		}
	}

	if len(missedFields) == 0 {
		return nil
	}
	return fmt.Errorf("fields required but not set: %s", strings.Join(missedFields, ","))
}

func (l *Loader) loadDefaults() error {
	if l.config.NewParser {
		return nil
	}

	for _, field := range l.fields {
		defaultValue := field.Tag("default")
		if err := l.setFieldData(field, defaultValue); err != nil {
			return err
		}
		field.isSet = (defaultValue != "")
	}
	return nil
}

func (l *Loader) loadFiles() error {
	if l.config.FileFlag != "" {
		if err := l.loadFileFlag(); err != nil {
			return err
		}
	}

	files := make([]string, 0, len(l.config.Files))
	for _, file := range l.config.Files {
		if _, err := fs.Stat(l.fsys, file); os.IsNotExist(err) {
			if l.config.FailOnFileNotFound {
				return err
			}
			continue
		}

		files = append(files, file)
		if !l.config.MergeFiles {
			break
		}
	}

	// Decode all files (resolving includes) before applying any of them,
	// so a failure doesn't leave the destination half-loaded.
	decoded, err := l.decodeFiles(files)
	if err != nil {
		return err
	}
	for _, file := range decoded {
		if err := l.applyFile(file); err != nil {
			return err
		}
	}
	return nil
}

// decodedFile holds a decoded config file ready to be applied.
type decodedFile struct {
	name   string
	tag    string
	fields map[string]any
}

// includeState tracks the include resolution of a single loadFiles call.
type includeState struct {
	visited map[string]bool // canonical names of fully resolved files
	onStack map[string]int  // canonical name -> index in stack
	stack   []string        // canonical names of files being resolved
}

func (l *Loader) decodeFiles(files []string) ([]decodedFile, error) {
	state := &includeState{
		visited: map[string]bool{},
		onStack: map[string]int{},
	}

	var decoded []decodedFile
	for _, file := range files {
		name, err := normalizeFilePath("", file)
		if err != nil {
			return nil, err
		}
		if err := l.decodeFile(name, state, &decoded); err != nil {
			return nil, err
		}
	}
	return decoded, nil
}

func (l *Loader) decodeFile(name string, state *includeState, decoded *[]decodedFile) error {
	if state.visited[name] {
		return nil
	}
	if _, ok := state.onStack[name]; ok {
		chain := append(append([]string{}, state.stack...), name)
		return fmt.Errorf("include cycle detected: %s", strings.Join(chain, " -> "))
	}

	ext := strings.ToLower(filepath.Ext(name))
	decoder, ok := l.config.FileDecoders[ext]
	if !ok {
		return fmt.Errorf("file format %q is not supported", ext)
	}

	actualFields, err := decoder.DecodeFile(name)
	if err != nil {
		return err
	}

	state.onStack[name] = len(state.stack)
	state.stack = append(state.stack, name)
	defer func() {
		delete(state.onStack, name)
		state.stack = state.stack[:len(state.stack)-1]
	}()

	if inc, ok := decoder.(FileDecoderIncludes); ok {
		includes, err := inc.Includes(actualFields)
		if err != nil {
			return fmt.Errorf("includes of %q: %w", name, err)
		}
		dir := path.Dir(name)
		for _, include := range includes {
			dep, err := normalizeFilePath(dir, include)
			if err != nil {
				return fmt.Errorf("include %q in file %q: %w", include, name, err)
			}
			if err := l.decodeFile(dep, state, decoded); err != nil {
				return err
			}
		}
	}

	state.visited[name] = true
	*decoded = append(*decoded, decodedFile{
		name:   name,
		tag:    decoder.Format(),
		fields: actualFields,
	})
	return nil
}

// normalizeFilePath resolves name relative to dir (the directory of the
// referencing file, empty for top-level files) and returns its canonical
// slash-separated form. Different relative spellings of the same file
// produce the same result. Paths starting with "/" are treated as relative
// to the file system root. The result never escapes the file system root.
func normalizeFilePath(dir, name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")

	var cleaned string
	if strings.HasPrefix(name, "/") {
		cleaned = path.Clean(name)
	} else {
		cleaned = path.Clean(path.Join(dir, name))
	}
	cleaned = strings.TrimPrefix(cleaned, "/")

	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes the config file system root", name)
	}
	return cleaned, nil
}

func (l *Loader) applyFile(file decodedFile) error {
	actualFields := file.fields

	tag := file.tag
	if l.config.NewParser {
		if err := l.parser.applyLevel(tag, actualFields); err != nil {
			return fmt.Errorf("apply %s: %w", tag, err)
		}
		return nil
	}

	for _, field := range l.fields {
		name := l.fullTag("", field, tag)
		if name == "" {
			continue
		}
		value, ok := actualFields[name]
		if !ok {
			actualFields = find(actualFields, name)
			value, ok = actualFields[name]
			if !ok {
				continue
			}
		}

		if err := l.setFieldData(field, value); err != nil {
			return err
		}
		field.isSet = true
		delete(actualFields, name)
	}

	if !l.config.AllowUnknownFields {
		for env := range actualFields {
			return fmt.Errorf("unknown field in file %q: %s (see AllowUnknownFields config param)", file.name, env)
		}
	}
	return nil
}

func (l *Loader) loadFileFlag() error {
	fileFlag := getActualFlag(l.config.FileFlag, l.flagSet)
	if fileFlag == nil {
		return nil
	}

	configFile := fileFlag.Value.String()
	if configFile == "" {
		return fmt.Errorf("%s should not be empty", l.config.FileFlag)
	}

	if l.config.MergeFiles {
		l.config.Files = append(l.config.Files, configFile)
	} else {
		l.config.Files = []string{configFile}
	}
	return nil
}

func (l *Loader) loadEnvironment() error {
	actualEnvs := getEnv(l.config.Envs)
	dupls := make(map[string]struct{})

	if l.config.NewParser {
		if err := l.parser.applyFlat("env", actualEnvs); err != nil {
			return fmt.Errorf("apply env: %w", err)
		}
		return nil
	}

	for _, field := range l.fields {
		envName := l.fullTag(l.config.EnvPrefix, field, "env")
		if envName == "" {
			continue
		}
		if err := l.setField(field, envName, actualEnvs, dupls); err != nil {
			return err
		}
	}
	return l.postEnvCheck(actualEnvs, dupls)
}

func (l *Loader) postEnvCheck(values map[string]any, dupls map[string]struct{}) error {
	if l.config.AllowUnknownEnvs || l.config.EnvPrefix == "" {
		return nil
	}
	for name := range dupls {
		delete(values, name)
	}
	for env := range values {
		if strings.HasPrefix(env, l.config.EnvPrefix) {
			return fmt.Errorf("unknown environment var %s (see AllowUnknownEnvs config param)", env)
		}
	}
	return nil
}

func (l *Loader) loadFlags() error {
	actualFlags := getFlags(l.flagSet)
	dupls := make(map[string]struct{})

	if l.config.NewParser {
		if err := l.parser.applyFlat("flag", actualFlags); err != nil {
			return fmt.Errorf("apply flag: %w", err)
		}
		return nil
	}

	for _, field := range l.fields {
		flagName := l.fullTag(l.config.FlagPrefix, field, "flag")
		if flagName == "" {
			continue
		}
		if err := l.setField(field, flagName, actualFlags, dupls); err != nil {
			return err
		}
	}
	return l.postFlagCheck(actualFlags, dupls)
}

func (l *Loader) postFlagCheck(values map[string]any, dupls map[string]struct{}) error {
	if l.config.AllowUnknownFlags || l.config.FlagPrefix == "" {
		return nil
	}
	for name := range dupls {
		delete(values, name)
	}
	for flag := range values {
		if strings.HasPrefix(flag, l.config.FlagPrefix) {
			return fmt.Errorf("unknown flag %s (see AllowUnknownFlags config param)", flag)
		}
	}
	return nil
}

// TODO(cristaloleg): revisit.
func (l *Loader) setField(field *fieldData, name string, values map[string]any, dupls map[string]struct{}) error {
	if !l.config.AllowDuplicates {
		if _, ok := dupls[name]; ok {
			return fmt.Errorf("field %q is duplicated", name)
		}
		dupls[name] = struct{}{}
	}

	val, ok := values[name]
	if !ok {
		return nil
	}

	if err := l.setFieldData(field, val); err != nil {
		return err
	}

	field.isSet = true
	if !l.config.AllowDuplicates {
		delete(values, name)
	}
	return nil
}
