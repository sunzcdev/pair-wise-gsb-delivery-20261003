package aconfig

import (
	"flag"
	"fmt"
	"reflect"
	"strings"
)

// DisabledConfigPolicy defines what happens when a disabled plugin
// still receives configuration from a file, an environment variable or a flag.
type DisabledConfigPolicy int

const (
	// DisabledConfigError fails loading if a disabled plugin receives any configuration.
	// This is the default and strict policy: a misspelled or stale plugin name is reported.
	DisabledConfigError DisabledConfigPolicy = iota

	// DisabledConfigIgnore silently skips configuration addressed to disabled plugins.
	DisabledConfigIgnore
)

// Plugin is a configuration namespace registered by a service plugin.
//
// Each plugin owns a pointer to its own configuration structure (Config)
// and a namespace (Name) used to address its fields from every source:
//
//   - files use the namespace as a top-level object, e.g. {"cache": {...}};
//   - environment variables use ENV_PREFIX_<NAMESPACE>_<FIELD>;
//   - flags use flagPrefix<FlagDelimiter><namespace><FlagDelimiter><field>.
type Plugin struct {
	name   string
	config any
	enable bool
}

// Name returns the plugin namespace.
func (p *Plugin) Name() string { return p.name }

// Config returns the destination structure of the plugin configuration.
func (p *Plugin) Config() any { return p.config }

// Enabled reports whether the plugin is enabled for loading.
func (p *Plugin) Enabled() bool { return p.enable }

// Registry loads the core configuration together with configurations
// of the registered plugins while keeping every namespace isolated.
//
// All sources (files, environment variables, flags) are read exactly once
// and then split between the core and the plugins. Fields with the same name
// in different plugins never overwrite each other. An unregistered namespace
// or an unknown core field is still reported as an error.
type Registry struct {
	core   any
	config Config

	plugins    []*Plugin
	pluginIdx  map[string]*Plugin
	pluginPtrs map[uintptr]string

	flagSet        *flag.FlagSet
	disabledPolicy DisabledConfigPolicy
}

// NewRegistry creates a Registry for the core configuration structure.
// Panics for the same invalid destinations as LoaderFor.
func NewRegistry(core any, config Config) *Registry {
	assertStruct(core)
	return &Registry{
		core:           core,
		config:         config,
		pluginIdx:      map[string]*Plugin{},
		pluginPtrs:     map[uintptr]string{},
		disabledPolicy: DisabledConfigError,
	}
}

// RegisterPlugin registers a plugin configuration namespace.
// It must be called before Flags and Load.
// The plugin is enabled by default; use Disable to turn it off before loading.
func (r *Registry) RegisterPlugin(namespace string, config any) (*Plugin, error) {
	if r.flagSet != nil {
		return nil, fmt.Errorf("aconfig: plugin %q registered after flags were built", namespace)
	}
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	if _, ok := r.pluginIdx[namespace]; ok {
		return nil, fmt.Errorf("aconfig: plugin namespace %q is registered twice", namespace)
	}
	assertStruct(config)

	ptr := reflect.ValueOf(config).Pointer()
	if ptr == reflect.ValueOf(r.core).Pointer() {
		return nil, fmt.Errorf("aconfig: plugin %q uses the core configuration destination", namespace)
	}
	if other, ok := r.pluginPtrs[ptr]; ok {
		return nil, fmt.Errorf("aconfig: plugin %q reuses destination of plugin %q", namespace, other)
	}

	plugin := &Plugin{name: namespace, config: config, enable: true}
	r.plugins = append(r.plugins, plugin)
	r.pluginIdx[namespace] = plugin
	r.pluginPtrs[ptr] = namespace
	return plugin, nil
}

// MustRegisterPlugin is like RegisterPlugin but panics on an error.
func (r *Registry) MustRegisterPlugin(namespace string, config any) *Plugin {
	plugin, err := r.RegisterPlugin(namespace, config)
	if err != nil {
		panic(err)
	}
	return plugin
}

// Enable turns an already registered plugin on.
func (r *Registry) Enable(namespace string) error {
	plugin, ok := r.pluginIdx[namespace]
	if !ok {
		return fmt.Errorf("aconfig: unknown plugin namespace %q", namespace)
	}
	plugin.enable = true
	return nil
}

// Disable turns an already registered plugin off before loading.
// Configuration addressed to a disabled plugin is handled according
// to the policy set by SetDisabledConfigPolicy.
func (r *Registry) Disable(namespace string) error {
	plugin, ok := r.pluginIdx[namespace]
	if !ok {
		return fmt.Errorf("aconfig: unknown plugin namespace %q", namespace)
	}
	plugin.enable = false
	return nil
}

// SetDisabledConfigPolicy chooses how configuration of disabled plugins is treated.
func (r *Registry) SetDisabledConfigPolicy(policy DisabledConfigPolicy) {
	if policy != DisabledConfigError && policy != DisabledConfigIgnore {
		panic(fmt.Sprintf("aconfig: unknown disabled config policy: %v", policy))
	}
	r.disabledPolicy = policy
}

func validateNamespace(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("aconfig: plugin namespace cannot be empty")
	}
	for _, ch := range namespace {
		switch ch {
		case '.', '_', '-', ' ', '\t':
			return fmt.Errorf("aconfig: plugin namespace %q must not contain %q", namespace, ch)
		}
	}
	return nil
}

// registryTarget is one load destination: the core or a single plugin.
type registryTarget struct {
	namespace string
	plugin    *Plugin
	loader    *Loader

	// Raw (not yet delimiter-suffixed) prefixes used to build shadow loaders.
	rawEnvPrefix  string
	rawFlagPrefix string
}

func newShadow(dst any) any {
	value := reflect.ValueOf(dst).Elem()
	return reflect.New(value.Type()).Interface()
}

func commitShadow(dst, shadow any) {
	dstValue := reflect.ValueOf(dst).Elem()
	shadowValue := reflect.ValueOf(shadow).Elem()
	dstValue.Set(shadowValue)
}

type registryTargets struct {
	core      *registryTarget
	enabled   []*registryTarget
	disabled  []*registryTarget
	flagNames map[string]string // full flag name -> owner namespace ("" is core)
	envNames  map[string]string // full env name -> owner namespace
}

func ownerLabel(namespace string) string {
	if namespace == "" {
		return "core"
	}
	return "plugin " + namespace
}

// Flags builds and returns the shared flag set.
//
// It contains the core flags and the flags of every registered plugin
// (including disabled ones, so that a flag addressed to a disabled plugin
// can be reported with the configured DisabledConfigPolicy).
// All plugins must be registered before calling Flags.
func (r *Registry) Flags() (*flag.FlagSet, error) {
	if r.flagSet != nil {
		return r.flagSet, nil
	}
	if _, err := r.build(true); err != nil {
		return nil, err
	}
	return r.flagSet, nil
}

// build validates all namespaces against each other and prepares
// a loader for the core and for every plugin.
// Flags are registered into the shared flag set only once,
// the first time the set is created.
func (r *Registry) build(withFlags bool) (*registryTargets, error) {
	if r.config.NewParser {
		return nil, fmt.Errorf("aconfig: registry does not support NewParser")
	}

	targets := &registryTargets{
		flagNames: map[string]string{},
		envNames:  map[string]string{},
	}

	coreLoader := LoaderFor(r.core, r.config)
	if coreLoader.errInit != nil {
		return nil, fmt.Errorf("init core loader: %w", coreLoader.errInit)
	}
	targets.core = &registryTarget{
		loader:        coreLoader,
		rawEnvPrefix:  r.config.EnvPrefix,
		rawFlagPrefix: r.config.FlagPrefix,
	}

	var sharedFlags *flag.FlagSet
	if withFlags && !r.config.SkipFlags {
		if r.flagSet == nil {
			r.flagSet = flag.NewFlagSet(r.config.FlagPrefix, flag.ContinueOnError)
			if r.config.FileFlag != "" {
				r.flagSet.String(r.config.FileFlag, "", "config file param")
			}
			sharedFlags = r.flagSet
		}
	}

	if err := r.claimNames(targets, targets.core, r.config.EnvPrefix, r.config.FlagPrefix, sharedFlags); err != nil {
		return nil, err
	}

	coreTopSegments := map[string]struct{}{}
	if !r.config.AllowUnknownFields {
		for _, field := range coreLoader.fields {
			if seg := topFileSegment(coreLoader.fullTag("", field, "json")); seg != "" {
				coreTopSegments[seg] = struct{}{}
			}
		}
	}

	for _, plugin := range r.plugins {
		// Plugin namespaces extend the core prefixes:
		// APP_CACHE_FIELD for env vars and app.cache.field for flags.
		rawEnvPrefix := joinEnvPrefix(r.config.EnvPrefix, plugin.name)
		rawFlagPrefix := joinFlagPrefix(r.config.FlagPrefix, effectiveFlagDelimiter(r.config.FlagDelimiter), plugin.name)

		pcfg := r.pluginConfig(plugin.name)
		loader := LoaderFor(plugin.config, pcfg)
		if loader.errInit != nil {
			return nil, fmt.Errorf("init plugin %q loader: %w", plugin.name, loader.errInit)
		}

		target := &registryTarget{
			namespace:     plugin.name,
			plugin:        plugin,
			loader:        loader,
			rawEnvPrefix:  rawEnvPrefix,
			rawFlagPrefix: rawFlagPrefix,
		}

		if !r.config.AllowUnknownFields {
			if _, ok := coreTopSegments[strings.ToLower(plugin.name)]; ok {
				return nil, fmt.Errorf("aconfig: plugin namespace %q conflicts with a core config field", plugin.name)
			}
		}

		if err := r.claimNames(targets, target, pcfg.EnvPrefix, pcfg.FlagPrefix, sharedFlags); err != nil {
			return nil, err
		}

		if plugin.enable {
			targets.enabled = append(targets.enabled, target)
		} else {
			targets.disabled = append(targets.disabled, target)
		}
	}
	return targets, nil
}

func (r *Registry) claimNames(targets *registryTargets, target *registryTarget, envPrefix, flagPrefix string, sharedFlags *flag.FlagSet) error {
	localFlags := map[string]struct{}{}
	for _, field := range target.loader.fields {
		envName := r.fullFlatName(target, field, "env", envPrefix)
		if envName != "" {
			if owner, ok := targets.envNames[envName]; ok {
				return fmt.Errorf("aconfig: env %q is claimed by %s and %s",
					envName, ownerLabel(owner), ownerLabel(target.namespace))
			}
			targets.envNames[envName] = target.namespace
		}

		flagName := r.fullFlatName(target, field, "flag", flagPrefix)
		if flagName != "" {
			if owner, ok := targets.flagNames[flagName]; ok {
				return fmt.Errorf("aconfig: flag %q is claimed by %s and %s",
					flagName, ownerLabel(owner), ownerLabel(target.namespace))
			}
			targets.flagNames[flagName] = target.namespace
		}

		if sharedFlags == nil || flagName == "" {
			continue
		}
		if _, dup := localFlags[flagName]; dup {
			if !r.config.AllowDuplicates {
				return fmt.Errorf("duplicate flag %q", flagName)
			}
			continue
		}
		localFlags[flagName] = struct{}{}
		sharedFlags.String(flagName, field.Tag("default"), field.Tag("usage"))
	}
	return nil
}

// fullFlatName builds the full env/flag name from the raw (not delimiter
// suffixed) namespace prefix. It mirrors Loader.fullTag but avoids relying on
// the loader init which already appended a delimiter to its own prefix.
func (r *Registry) fullFlatName(target *registryTarget, field *fieldData, tag, rawPrefix string) string {
	loader := target.loader

	var sep string
	switch tag {
	case "env":
		sep = loader.config.envDelimiter
	case "flag":
		sep = loader.config.FlagDelimiter
	default:
		sep = "."
	}

	res := field.Tag(tag)
	if res == "-" {
		return ""
	}
	if before, _, ok := cut(res, ",exact"); ok {
		return before
	}
	if before, _, ok := cut(res, ",omitempty"); ok {
		res = before
	}
	for parent := field.parent; parent != nil; parent = parent.parent {
		if parent.Tag(tag) != "-" {
			res = parent.Tag(tag) + sep + res
		}
	}
	if rawPrefix == "" {
		return res
	}
	return rawPrefix + sep + res
}

// pluginConfig derives the loader Config for a plugin namespace.
func (r *Registry) pluginConfig(namespace string) Config {
	cfg := r.config
	cfg.EnvPrefix = joinEnvPrefix(r.config.EnvPrefix, namespace)
	cfg.FlagPrefix = joinFlagPrefix(r.config.FlagPrefix, effectiveFlagDelimiter(r.config.FlagDelimiter), namespace)

	// Sources are read once by the registry; per-plugin loaders only
	// receive the slice that belongs to their namespace.
	cfg.SkipFiles = true
	cfg.SkipEnv = true
	cfg.SkipFlags = true
	return cfg
}

func effectiveFlagDelimiter(delimiter string) string {
	if delimiter == "" {
		return "."
	}
	return delimiter
}

func joinEnvPrefix(base, namespace string) string {
	base = strings.TrimSuffix(base, "_")
	if base == "" {
		return strings.ToUpper(namespace)
	}
	return base + "_" + strings.ToUpper(namespace)
}

func joinFlagPrefix(base, delimiter, namespace string) string {
	base = strings.TrimSuffix(base, delimiter)
	if base == "" {
		return namespace
	}
	return base + delimiter + namespace
}

func topFileSegment(name string) string {
	if name == "" {
		return ""
	}
	if i := strings.Index(name, "."); i >= 0 {
		return strings.ToLower(name[:i])
	}
	return strings.ToLower(name)
}
