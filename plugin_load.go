package aconfig

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// loadConfigWithPlugins is the plugin-aware loading pipeline.
//
// Every source is read once and then partitioned between the core
// configuration and the registered namespaces. Namespaces were already
// validated for conflicts at init time; unregistered namespaces and
// unknown core fields are rejected by the strict checks below.
// Destinations are mutated under a snapshot taken before parsing, so any
// error rolls the whole release back.
func (l *Loader) loadConfigWithPlugins() error {
	if err := l.parseFlags(); err != nil {
		return err
	}

	if !l.config.SkipDefaults {
		if err := l.applyDefaults(l.fields); err != nil {
			return fmt.Errorf("load defaults: %w", err)
		}
		for _, ns := range l.plugins {
			if !ns.enabled() {
				continue
			}
			if err := l.applyDefaults(ns.fields); err != nil {
				return fmt.Errorf("load defaults for plugin %q: %w", ns.spec.Namespace, err)
			}
		}
	}

	if !l.config.SkipFiles {
		if err := l.loadFilesWithPlugins(); err != nil {
			return err
		}
	}
	if !l.config.SkipEnv {
		if err := l.loadEnvironmentWithPlugins(); err != nil {
			return err
		}
	}
	if !l.config.SkipFlags {
		if err := l.loadFlagsWithPlugins(); err != nil {
			return err
		}
	}

	if err := l.checkRequiredWithPlugins(); err != nil {
		return err
	}
	return nil
}

func (l *Loader) applyDefaults(fields []*fieldData) error {
	for _, field := range fields {
		defaultValue := field.Tag("default")
		if err := l.setFieldData(field, defaultValue); err != nil {
			return err
		}
		field.isSet = (defaultValue != "")
	}
	return nil
}

func (l *Loader) loadFilesWithPlugins() error {
	if l.config.FileFlag != "" {
		if err := l.loadFileFlag(); err != nil {
			return err
		}
	}

	for _, file := range l.config.Files {
		if _, err := fs.Stat(l.fsys, file); os.IsNotExist(err) {
			if l.config.FailOnFileNotFound {
				return err
			}
			continue
		}

		if err := l.loadOneFileWithPlugins(file); err != nil {
			return err
		}

		if !l.config.MergeFiles {
			break
		}
	}
	return nil
}

func (l *Loader) loadOneFileWithPlugins(file string) error {
	ext := strings.ToLower(filepath.Ext(file))
	decoder, ok := l.config.FileDecoders[ext]
	if !ok {
		return fmt.Errorf("file format %q is not supported", ext)
	}

	raw, err := decoder.DecodeFile(file)
	if err != nil {
		return err
	}
	tag := decoder.Format()

	// Read once: split the decoded document between core and namespaces.
	coreFields := cloneMap(raw)
	pluginFields := map[*preparedPlugin]map[string]any{}

	for _, ns := range l.plugins {
		value, present := raw[ns.spec.Prefix]
		if !present {
			continue
		}
		delete(coreFields, ns.spec.Prefix)

		if !ns.enabled() {
			if err := l.disabledPluginConfig(ns, fmt.Sprintf("file %q field", file), ns.spec.Prefix); err != nil {
				return err
			}
			continue
		}

		sub, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("namespace %q in file %q must be an object, got %T", ns.spec.Namespace, file, value)
		}
		// Wrap under the prefix so field resolution matches nested fields.
		pluginFields[ns] = map[string]any{ns.spec.Prefix: sub}
	}

	if err := l.applyFileFields(l.fields, tag, file, coreFields, ""); err != nil {
		return fmt.Errorf("load files: %w", err)
	}
	for _, ns := range l.plugins {
		values, ok := pluginFields[ns]
		if !ok {
			continue
		}
		if err := l.applyFileFields(ns.fields, tag, file, values, "plugin "+ns.spec.Namespace); err != nil {
			return fmt.Errorf("load files: %w", err)
		}
	}
	return nil
}

// applyFileFields applies one decoded document to one field set.
// scope is empty for the core config and identifies the plugin otherwise.
func (l *Loader) applyFileFields(fields []*fieldData, tag, file string, actualFields map[string]any, scope string) error {
	for _, field := range fields {
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
		for name := range actualFields {
			if scope == "" {
				return fmt.Errorf("unknown field in file %q: %s (see AllowUnknownFields config param)", file, name)
			}
			return fmt.Errorf("unknown field in file %q for %s: %s (see AllowUnknownFields config param)", file, scope, name)
		}
	}
	return nil
}

func (l *Loader) loadEnvironmentWithPlugins() error {
	all := getEnv(l.config.Envs)
	coreValues, pluginValues, err := l.partitionFlat(all, "env",
		func(ns *preparedPlugin) string { return ns.fullEnvRoot(l.config.EnvPrefix) },
		func(ns *preparedPlugin) map[string]struct{} { return ns.envNames })
	if err != nil {
		return fmt.Errorf("load environment: %w", err)
	}

	dupls := map[string]struct{}{}
	for _, field := range l.fields {
		envName := l.fullTag(l.config.EnvPrefix, field, "env")
		if envName == "" {
			continue
		}
		if err := l.setField(field, envName, coreValues, dupls); err != nil {
			return fmt.Errorf("load environment: %w", err)
		}
	}
	if err := l.postEnvCheck(coreValues, dupls); err != nil {
		return fmt.Errorf("load environment: %w", err)
	}

	for _, ns := range l.plugins {
		values, ok := pluginValues[ns]
		if !ok {
			continue
		}
		if err := l.applyFlatValues(ns.fields, "env", values, "plugin "+ns.spec.Namespace); err != nil {
			return fmt.Errorf("load environment: %w", err)
		}
	}
	return nil
}

func (l *Loader) loadFlagsWithPlugins() error {
	all := getFlags(l.flagSet)
	coreValues, pluginValues, err := l.partitionFlat(all, "flag",
		func(ns *preparedPlugin) string { return ns.fullFlagRoot(l.config.FlagPrefix, l.config.FlagDelimiter) },
		func(ns *preparedPlugin) map[string]struct{} { return ns.flagNames })
	if err != nil {
		return fmt.Errorf("load flags: %w", err)
	}

	dupls := map[string]struct{}{}
	for _, field := range l.fields {
		flagName := l.fullTag(l.config.FlagPrefix, field, "flag")
		if flagName == "" {
			continue
		}
		if err := l.setField(field, flagName, coreValues, dupls); err != nil {
			return fmt.Errorf("load flags: %w", err)
		}
	}
	if err := l.postFlagCheck(coreValues, dupls); err != nil {
		return fmt.Errorf("load flags: %w", err)
	}

	for _, ns := range l.plugins {
		values, ok := pluginValues[ns]
		if !ok {
			continue
		}
		if err := l.applyFlatValues(ns.fields, "flag", values, "plugin "+ns.spec.Namespace); err != nil {
			return fmt.Errorf("load flags: %w", err)
		}
	}
	return nil
}

// partitionFlat splits a flat source (environment or flags) between the
// core and the namespaces. Values for disabled plugins are removed from
// the core view and reported according to DisabledPluginPolicy.
func (l *Loader) partitionFlat(
	all map[string]any,
	kind string,
	root func(*preparedPlugin) string,
	exactNames func(*preparedPlugin) map[string]struct{},
) (map[string]any, map[*preparedPlugin]map[string]any, error) {
	coreValues := cloneMap(all)
	pluginValues := map[*preparedPlugin]map[string]any{}

	for name, value := range all {
		for _, ns := range l.plugins {
			prefixed := strings.HasPrefix(name, root(ns))
			_, exact := exactNames(ns)[name]
			if !prefixed && !exact {
				continue
			}

			delete(coreValues, name)
			if !ns.enabled() {
				if err := l.disabledPluginConfig(ns, kind, name); err != nil {
					return nil, nil, err
				}
				break
			}
			if pluginValues[ns] == nil {
				pluginValues[ns] = map[string]any{}
			}
			pluginValues[ns][name] = value
			break
		}
	}
	return coreValues, pluginValues, nil
}

func (l *Loader) applyFlatValues(fields []*fieldData, tag string, values map[string]any, scope string) error {
	dupls := map[string]struct{}{}
	for _, field := range fields {
		name := l.fullTag(mapPrefix(tag, l.config), field, tag)
		if name == "" {
			continue
		}
		if err := l.setField(field, name, values, dupls); err != nil {
			return err
		}
	}
	for name := range dupls {
		delete(values, name)
	}
	// Every key in the partition belongs to this namespace, so anything
	// left is a misspelled plugin field.
	for name := range values {
		return fmt.Errorf("unknown %s %s for %s (see AllowUnknown%ss config param)",
			tag, name, scope, capitalize(tag))
	}
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func mapPrefix(tag string, cfg Config) string {
	if tag == "env" {
		return cfg.EnvPrefix
	}
	return cfg.FlagPrefix
}

func (l *Loader) disabledPluginConfig(ns *preparedPlugin, source, name string) error {
	if l.config.DisabledPluginPolicy == IgnoreDisabledPluginConfig {
		return nil
	}
	return fmt.Errorf("disabled plugin %q received %s %q (see DisabledPluginPolicy config param)",
		ns.spec.Namespace, source, name)
}

func (l *Loader) checkRequiredWithPlugins() error {
	missed := l.missingRequired(l.fields, l.config.AllFieldRequired, nil)
	for _, ns := range l.plugins {
		if !ns.enabled() {
			continue
		}
		missed = l.missingRequired(ns.fields, l.config.AllFieldRequired, missed)
	}
	if len(missed) == 0 {
		return nil
	}
	return fmt.Errorf("fields required but not set: %s", strings.Join(missed, ","))
}

func (l *Loader) missingRequired(fields []*fieldData, allRequired bool, missed []string) []string {
	for _, field := range fields {
		if field.isSet {
			continue
		}
		if field.isRequired || allRequired {
			missed = append(missed, field.name)
		}
	}
	return missed
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
