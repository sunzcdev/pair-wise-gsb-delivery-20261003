package aconfig

import (
	"flag"
	"fmt"
	"reflect"
	"strings"
)

// DisabledPluginPolicy defines what happens when configuration is provided
// for a plugin that is registered but disabled.
type DisabledPluginPolicy int

const (
	// RejectDisabledPluginConfig fails loading if any value is provided
	// for a disabled plugin. This is the default and the safe choice:
	// a typo or stale config never passes silently.
	RejectDisabledPluginConfig DisabledPluginPolicy = iota

	// IgnoreDisabledPluginConfig silently drops every value provided
	// for a disabled plugin. The plugin configuration stays untouched.
	IgnoreDisabledPluginConfig
)

// PluginSpec registers a configuration structure under a namespace.
//
// The Namespace is the unique identifier of the plugin.
// The Prefix is the field prefix used in every configuration source:
// it becomes the top-level key in files (e.g. {"auth": {...}}),
// the ENV segment (e.g. APP_AUTH_TOKEN) and the flag segment
// (e.g. --auth.token). Prefix must be a single segment containing
// only ASCII letters and digits.
//
// Config must be a non-nil pointer to the plugin configuration struct.
// Enabled allows enabling/disabling the plugin before loading starts.
// A disabled plugin is still registered: its namespace is reserved and
// values sent to it are handled according to DisabledPluginPolicy.
type PluginSpec struct {
	Namespace string
	Prefix    string
	Config    any
	Enabled   bool
}

// preparedPlugin is a registered plugin with its parsed fields.
type preparedPlugin struct {
	spec      PluginSpec
	envRoot   string // upper-cased prefix, the env segment
	fields    []*fieldData
	envNames  map[string]struct{}
	flagNames map[string]struct{}
	snapshot  any
}

func (p *preparedPlugin) enabled() bool {
	return p.spec.Enabled
}

// fullEnvRoot is the full environment prefix, including Config.EnvPrefix.
func (p *preparedPlugin) fullEnvRoot(envPrefix string) string {
	return envPrefix + p.envRoot + "_"
}

// fullFlagRoot is the full flag prefix, including Config.FlagPrefix.
func (p *preparedPlugin) fullFlagRoot(flagPrefix, flagDelimiter string) string {
	return flagPrefix + p.spec.Prefix + flagDelimiter
}

// initPluginsLegacy validates and prepares registered plugins using the
// legacy reflection-based parser. It must be called after core fields
// are built and before any configuration source is read.
func (l *Loader) initPluginsLegacy() error {
	if len(l.config.Plugins) == 0 {
		return nil
	}

	byName := make(map[string]struct{}, len(l.config.Plugins))

	l.plugins = make([]*preparedPlugin, 0, len(l.config.Plugins))
	for _, spec := range l.config.Plugins {
		if err := validatePluginSpec(spec); err != nil {
			return err
		}
		if _, ok := byName[spec.Namespace]; ok {
			return fmt.Errorf("plugin namespace %q is registered more than once", spec.Namespace)
		}
		byName[spec.Namespace] = struct{}{}

		ns := &preparedPlugin{
			spec:      spec,
			envRoot:   strings.ToUpper(spec.Prefix),
			envNames:  map[string]struct{}{},
			flagNames: map[string]struct{}{},
		}

		for _, other := range l.plugins {
			if err := checkPrefixConflict(ns, other, l.config.FlagDelimiter); err != nil {
				return err
			}
		}
		l.plugins = append(l.plugins, ns)
	}

	if err := l.checkCoreConflicts(); err != nil {
		return err
	}

	// Fields are built after all prefixes are validated; the virtual
	// parent below makes every plugin field resolve under its prefix.
	coreEnvNames := map[string]struct{}{}
	for _, f := range l.fields {
		if env := l.fullTag(l.config.EnvPrefix, f, "env"); env != "" {
			coreEnvNames[env] = struct{}{}
		}
	}

	for _, ns := range l.plugins {
		root := reflect.ValueOf(ns.spec.Config)
		for root.Kind() == reflect.Ptr {
			root = root.Elem()
		}
		ns.fields = l.getFieldsHelper(root, l.namespaceParent(ns))

		for _, f := range ns.fields {
			if env := l.fullTag(l.config.EnvPrefix, f, "env"); env != "" {
				if _, ok := coreEnvNames[env]; ok && !l.config.AllowDuplicates {
					return fmt.Errorf("environment variable %q is duplicated between core and plugin %q", env, ns.spec.Namespace)
				}
				for _, other := range l.plugins {
					if other == ns {
						continue
					}
					if _, ok := other.envNames[env]; ok && !l.config.AllowDuplicates {
						return fmt.Errorf("environment variable %q is duplicated in plugins %q and %q", env, other.spec.Namespace, ns.spec.Namespace)
					}
				}
				ns.envNames[env] = struct{}{}
			}
			if flag := l.fullTag(l.config.FlagPrefix, f, "flag"); flag != "" {
				ns.flagNames[flag] = struct{}{}
			}
		}
		ns.snapshot = snapshot(ns.spec.Config)
	}

	l.coreSnapshot = snapshot(l.dst)
	return nil
}

// registerLegacyFlags registers core and plugin flags into a fresh flag set.
func (l *Loader) registerLegacyFlags() error {
	names := make(map[string]bool, len(l.fields))
	for _, field := range l.fields {
		flagName := l.fullTag(l.config.FlagPrefix, field, "flag")
		if flagName == "" {
			continue
		}
		if names[flagName] && !l.config.AllowDuplicates {
			return fmt.Errorf("duplicate flag %q", flagName)
		}
		names[flagName] = true
		l.flagSet.String(flagName, field.Tag("default"), field.Tag("usage"))
	}

	for _, ns := range l.plugins {
		for _, field := range ns.fields {
			flagName := l.fullTag(l.config.FlagPrefix, field, "flag")
			if flagName == "" {
				continue
			}
			if names[flagName] && !l.config.AllowDuplicates {
				return fmt.Errorf("duplicate flag %q in plugin %q", flagName, ns.spec.Namespace)
			}
			names[flagName] = true
			l.flagSet.String(flagName, field.Tag("default"), field.Tag("usage"))
		}
	}
	return nil
}

// reparseLegacy rebuilds field metadata, snapshots and the flag set after a
// failed Load, so a retry is not polluted by stale isSet markers or an
// already-parsed flag set. Called while destinations are already restored.
func (l *Loader) reparseLegacy() error {
	l.fields = l.getFields(l.dst)
	l.coreSnapshot = snapshot(l.dst)

	for _, ns := range l.plugins {
		root := reflect.ValueOf(ns.spec.Config)
		for root.Kind() == reflect.Ptr {
			root = root.Elem()
		}
		ns.fields = l.getFieldsHelper(root, l.namespaceParent(ns))
		ns.envNames = map[string]struct{}{}
		ns.flagNames = map[string]struct{}{}
		for _, f := range ns.fields {
			if env := l.fullTag(l.config.EnvPrefix, f, "env"); env != "" {
				ns.envNames[env] = struct{}{}
			}
			if flag := l.fullTag(l.config.FlagPrefix, f, "flag"); flag != "" {
				ns.flagNames[flag] = struct{}{}
			}
		}
		ns.snapshot = snapshot(ns.spec.Config)
	}

	if !l.config.SkipFlags {
		l.flagSet = flag.NewFlagSet(l.config.FlagPrefix, flag.ContinueOnError)
		return l.registerLegacyFlags()
	}
	return nil
}

// namespaceParent builds a virtual field that represents the namespace
// root, so plugin fields inherit the prefix exactly like nested fields.
func (l *Loader) namespaceParent(ns *preparedPlugin) *fieldData {
	tags := map[string]string{
		"env":  ns.envRoot,
		"flag": ns.spec.Prefix,
	}
	for _, dec := range l.config.FileDecoders {
		tags[dec.Format()] = ns.spec.Prefix
	}
	return &fieldData{name: ns.spec.Namespace, tags: tags}
}

func validatePluginSpec(spec PluginSpec) error {
	if spec.Namespace == "" {
		return fmt.Errorf("plugin namespace cannot be empty")
	}
	if spec.Prefix == "" {
		return fmt.Errorf("plugin %q prefix cannot be empty", spec.Namespace)
	}
	if !isSingleSegment(spec.Prefix) {
		return fmt.Errorf("plugin %q prefix %q must contain only letters and digits", spec.Namespace, spec.Prefix)
	}
	if spec.Config == nil {
		return fmt.Errorf("plugin %q config cannot be nil", spec.Namespace)
	}
	value := reflect.ValueOf(spec.Config)
	if value.Kind() != reflect.Ptr || value.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("plugin %q config must be a pointer to a struct", spec.Namespace)
	}
	return nil
}

func isSingleSegment(s string) bool {
	if s == "" {
		return false
	}
	first := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && !first:
		default:
			return false
		}
		first = false
	}
	return true
}

func checkPrefixConflict(a, b *preparedPlugin, flagDelimiter string) error {
	if strings.EqualFold(a.spec.Prefix, b.spec.Prefix) {
		return fmt.Errorf("plugin prefix %q is claimed by %q and %q", a.spec.Prefix, b.spec.Namespace, a.spec.Namespace)
	}

	aEnv, bEnv := a.envRoot+"_", b.envRoot+"_"
	if strings.HasPrefix(aEnv, bEnv) || strings.HasPrefix(bEnv, aEnv) {
		return fmt.Errorf("plugin prefixes %q (%q) and %q (%q) conflict in environment variables",
			a.spec.Prefix, a.spec.Namespace, b.spec.Prefix, b.spec.Namespace)
	}

	aFlag := a.spec.Prefix + flagDelimiter
	bFlag := b.spec.Prefix + flagDelimiter
	if strings.HasPrefix(aFlag, bFlag) || strings.HasPrefix(bFlag, aFlag) {
		return fmt.Errorf("plugin prefixes %q (%q) and %q (%q) conflict in flags",
			a.spec.Prefix, a.spec.Namespace, b.spec.Prefix, b.spec.Namespace)
	}
	return nil
}

// checkCoreConflicts ensures no plugin prefix collides with a core field
// in files, environment variables or flags. Validation happens before any
// configuration source is read.
func (l *Loader) checkCoreConflicts() error {
	for _, ns := range l.plugins {
		for _, field := range l.fields {
			for _, dec := range l.config.FileDecoders {
				tag := dec.Format()
				coreRoot := firstSegment(l.fullTag("", field, tag), ".")
				if strings.EqualFold(coreRoot, ns.spec.Prefix) {
					return fmt.Errorf("plugin namespace %q conflicts with core field %q in %s files",
						ns.spec.Namespace, field.name, tag)
				}
			}

			if envRoot := firstSegment(strings.TrimPrefix(l.fullTag(l.config.EnvPrefix, field, "env"), l.config.EnvPrefix), l.config.envDelimiter); envRoot == ns.envRoot {
				return fmt.Errorf("plugin namespace %q conflicts with core field %q in environment variables",
					ns.spec.Namespace, field.name)
			}

			if flagRoot := firstSegment(strings.TrimPrefix(l.fullTag(l.config.FlagPrefix, field, "flag"), l.config.FlagPrefix), l.config.FlagDelimiter); flagRoot == ns.spec.Prefix {
				return fmt.Errorf("plugin namespace %q conflicts with core field %q in flags",
					ns.spec.Namespace, field.name)
			}
		}
	}
	return nil
}

func firstSegment(s, sep string) string {
	if s == "" {
		return ""
	}
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

// Plugin returns the configuration destination of a registered plugin.
// It is the same pointer passed via PluginSpec.Config and is populated
// after a successful Load. The boolean reports whether the namespace exists.
func (l *Loader) Plugin(namespace string) (any, bool) {
	for _, ns := range l.plugins {
		if ns.spec.Namespace == namespace {
			return ns.spec.Config, true
		}
	}
	return nil, false
}

// SetPluginEnabled enables or disables a plugin before Load is called.
// Required fields are enforced only for enabled plugins; values sent to a
// disabled plugin are handled according to DisabledPluginPolicy.
func (l *Loader) SetPluginEnabled(namespace string, enabled bool) error {
	for _, ns := range l.plugins {
		if ns.spec.Namespace == namespace {
			ns.spec.Enabled = enabled
			return nil
		}
	}
	return fmt.Errorf("plugin namespace %q is not registered", namespace)
}

// restore rolls every destination back to the state captured before the
// plugin fields were built, so a failed load never partially publishes.
func (l *Loader) restorePlugins() {
	restore(l.dst, l.coreSnapshot)
	for _, ns := range l.plugins {
		restore(ns.spec.Config, ns.snapshot)
	}
}

func snapshot(dst any) any {
	elem := reflect.ValueOf(dst).Elem()
	clone := reflect.New(elem.Type())
	clone.Elem().Set(copyValue(elem))
	return clone.Interface()
}

func restore(dst, snapshot any) {
	reflect.ValueOf(dst).Elem().Set(reflect.ValueOf(snapshot).Elem())
}

func copyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		nv := reflect.New(v.Elem().Type())
		nv.Elem().Set(copyValue(v.Elem()))
		return nv

	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		nv := reflect.New(v.Elem().Type()).Elem()
		nv.Set(copyValue(v.Elem()))
		return nv

	case reflect.Struct:
		nv := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			if nv.Field(i).CanSet() {
				nv.Field(i).Set(copyValue(v.Field(i)))
			}
		}
		return nv

	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		nv := reflect.MakeSlice(v.Type(), v.Len(), v.Cap())
		for i := 0; i < v.Len(); i++ {
			nv.Index(i).Set(copyValue(v.Index(i)))
		}
		return nv

	case reflect.Array:
		nv := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			nv.Index(i).Set(copyValue(v.Index(i)))
		}
		return nv

	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		nv := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			nv.SetMapIndex(copyValue(iter.Key()), copyValue(iter.Value()))
		}
		return nv

	default:
		return v
	}
}
