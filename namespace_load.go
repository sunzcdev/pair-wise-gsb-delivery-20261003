package aconfig

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Load reads all configured sources once and builds the core configuration
// and the configuration of every enabled plugin.
//
// Loading is atomic: defaults, files, environment variables and flags are
// applied to zero-valued shadow copies first; user-provided destinations are
// modified only if every enabled target loads successfully.
func (r *Registry) Load() error {
	targets, err := r.build(true)
	if err != nil {
		return fmt.Errorf("init registry: %w", err)
	}

	shadowLoaders := make(map[*registryTarget]*Loader, len(targets.enabled)+1)
	shadows := make(map[*registryTarget]any, len(targets.enabled)+1)

	addShadow := func(target *registryTarget, dst any) {
		shadow := newShadow(dst)
		shadows[target] = shadow

		cfg := target.loader.config
		cfg.EnvPrefix = target.rawEnvPrefix
		cfg.FlagPrefix = target.rawFlagPrefix
		cfg.FlagDelimiter = effectiveFlagDelimiter(r.config.FlagDelimiter)
		cfg.SkipDefaults = r.config.SkipDefaults
		cfg.SkipFiles = true
		cfg.SkipEnv = true
		cfg.SkipFlags = true

		loader := LoaderFor(shadow, cfg)
		if loader.errInit != nil {
			err = fmt.Errorf("init %s loader: %w", ownerLabel(target.namespace), loader.errInit)
			return
		}
		shadowLoaders[target] = loader
	}

	addShadow(targets.core, r.core)
	if err != nil {
		return err
	}

	for _, target := range targets.enabled {
		addShadow(target, target.plugin.config)
		if err != nil {
			return err
		}
	}

	if err := r.parseSharedFlags(); err != nil {
		return err
	}

	if !r.config.SkipDefaults {
		for _, target := range append([]*registryTarget{targets.core}, targets.enabled...) {
			if err := shadowLoaders[target].loadDefaults(); err != nil {
				return fmt.Errorf("load %s defaults: %w", ownerLabel(target.namespace), err)
			}
		}
	}

	if !r.config.SkipFiles {
		if err := r.loadFiles(targets, shadowLoaders); err != nil {
			return err
		}
	}

	if !r.config.SkipEnv {
		if err := r.loadEnv(targets, shadowLoaders); err != nil {
			return err
		}
	}

	if !r.config.SkipFlags {
		if err := r.loadFlags(targets, shadowLoaders); err != nil {
			return err
		}
	}

	// Unknown fields are reported (inside the loaders above) before
	// required fields are checked.
	missed := []string{}
	for _, target := range append([]*registryTarget{targets.core}, targets.enabled...) {
		loader := shadowLoaders[target]
		for _, field := range loader.fields {
			if field.isSet {
				continue
			}
			if field.isRequired || loader.config.AllFieldRequired {
				missed = append(missed, qualifiedName(target.namespace, field.name))
			}
		}
	}
	if len(missed) != 0 {
		return fmt.Errorf("fields required but not set: %s", strings.Join(missed, ","))
	}

	// All targets loaded successfully, publish shadows atomically.
	commitShadow(r.core, shadows[targets.core])
	for _, target := range targets.enabled {
		commitShadow(target.plugin.config, shadows[target])
	}
	return nil
}

func (r *Registry) parseSharedFlags() error {
	if r.flagSet == nil || r.config.SkipFlags {
		return nil
	}
	if r.flagSet.Parsed() {
		return nil
	}
	return r.flagSet.Parse(r.config.Args)
}

func qualifiedName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "." + name
}

func (r *Registry) loadFiles(targets *registryTargets, loaders map[*registryTarget]*Loader) error {
	coreOrig := targets.core.loader
	files := coreOrig.config.Files

	if coreOrig.config.FileFlag != "" && r.flagSet != nil {
		fileFlag := getActualFlag(coreOrig.config.FileFlag, r.flagSet)
		if fileFlag != nil {
			configFile := fileFlag.Value.String()
			if configFile == "" {
				return fmt.Errorf("%s should not be empty", coreOrig.config.FileFlag)
			}
			if coreOrig.config.MergeFiles {
				files = append(files, configFile)
			} else {
				files = []string{configFile}
			}
		}
	}

	fsys := coreOrig.fsys
	for _, file := range files {
		if _, statErr := fs.Stat(fsys, file); os.IsNotExist(statErr) {
			if coreOrig.config.FailOnFileNotFound {
				return statErr
			}
			continue
		}

		if err := r.loadFile(targets, loaders, fsys, file); err != nil {
			return err
		}

		if !coreOrig.config.MergeFiles {
			break
		}
	}
	return nil
}

func (r *Registry) loadFile(targets *registryTargets, loaders map[*registryTarget]*Loader, fsys fs.FS, file string) error {
	ext := strings.ToLower(filepath.Ext(file))
	decoder, ok := targets.core.loader.config.FileDecoders[ext]
	if !ok {
		return fmt.Errorf("file format %q is not supported", ext)
	}

	actualFields, err := decoder.DecodeFile(file)
	if err != nil {
		return err
	}
	tag := decoder.Format()

	pluginSections := map[string]map[string]any{}
	coreFields := map[string]any{}

	for key, value := range actualFields {
		lower := strings.ToLower(key)
		if target := r.findPluginTarget(targets, lower); target != nil {
			section, ok := asStringMap(value)
			if !ok {
				return fmt.Errorf("plugin %q config in file %q must be an object", lower, file)
			}
			pluginSections[lower] = section
			continue
		}
		coreFields[key] = value
	}

	// Plugin sections first: a disabled-plugin section must fail before
	// unknown core fields are reported.
	for _, target := range append(append([]*registryTarget{}, targets.enabled...), targets.disabled...) {
		section, ok := pluginSections[strings.ToLower(target.namespace)]
		if !ok {
			continue
		}
		if target.plugin != nil && !target.plugin.enable {
			if r.disabledPolicy == DisabledConfigError {
				return fmt.Errorf("configuration provided for disabled plugin %q in file %q", target.namespace, file)
			}
			continue
		}
		loader := loaders[target]
		if err := loader.applyDecodedFields(tag, file, section); err != nil {
			return fmt.Errorf("plugin %s: %w", target.namespace, err)
		}
	}

	coreLoader := loaders[targets.core]
	if err := coreLoader.applyDecodedFields(tag, file, coreFields); err != nil {
		return err
	}
	return nil
}

func (r *Registry) findPluginTarget(targets *registryTargets, namespace string) *registryTarget {
	for _, target := range append(append([]*registryTarget{}, targets.enabled...), targets.disabled...) {
		if strings.ToLower(target.namespace) == namespace {
			return target
		}
	}
	return nil
}

func asStringMap(value any) (map[string]any, bool) {
	switch m := value.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		res := make(map[string]any, len(m))
		for key, val := range m {
			res[fmt.Sprint(key)] = val
		}
		return res, true
	default:
		return nil, false
	}
}

func (r *Registry) loadEnv(targets *registryTargets, loaders map[*registryTarget]*Loader) error {
	coreLoader := loaders[targets.core]
	values := getEnv(coreLoader.config.Envs)

	allPlugins := append(append([]*registryTarget{}, targets.enabled...), targets.disabled...)

	unknownEnvs := []string{}

	for _, target := range allPlugins {
		prefix := target.rawEnvPrefix

		pluginValues := map[string]any{}
		for key, value := range values {
			if !flatHasPrefix(key, prefix, target.loader.config.envDelimiter) {
				continue
			}
			delete(values, key)
			if !target.plugin.enable {
				if r.disabledPolicy == DisabledConfigError {
					return fmt.Errorf("environment variable %q provided for disabled plugin %q", key, target.namespace)
				}
				continue
			}
			pluginValues[key] = value
		}
		if !target.plugin.enable {
			continue
		}

		loader := loaders[target]
		dupls := map[string]struct{}{}
		for _, field := range loader.fields {
			envName := r.fullFlatName(target, field, "env", prefix)
			if envName == "" {
				continue
			}
			if err := loader.setField(field, envName, pluginValues, dupls); err != nil {
				return err
			}
		}
		for key := range pluginValues {
			unknownEnvs = append(unknownEnvs, key)
		}
	}

	dupls := map[string]struct{}{}
	for _, field := range coreLoader.fields {
		envName := r.fullFlatName(targets.core, field, "env", targets.core.rawEnvPrefix)
		if envName == "" {
			continue
		}
		if err := coreLoader.setField(field, envName, values, dupls); err != nil {
			return err
		}
	}
	if err := coreLoader.postEnvCheck(values, dupls); err != nil {
		return err
	}
	if len(unknownEnvs) != 0 && !coreLoader.config.AllowUnknownEnvs {
		sort.Strings(unknownEnvs)
		return fmt.Errorf("unknown environment var %s (see AllowUnknownEnvs config param)", strings.Join(unknownEnvs, ","))
	}
	return nil
}

func (r *Registry) loadFlags(targets *registryTargets, loaders map[*registryTarget]*Loader) error {
	if r.flagSet == nil {
		return nil
	}

	actualFlags := getFlags(r.flagSet)

	allPlugins := append(append([]*registryTarget{}, targets.enabled...), targets.disabled...)

	unknownFlags := []string{}

	for _, target := range allPlugins {
		prefix := target.rawFlagPrefix
		delimiter := effectiveFlagDelimiter(r.config.FlagDelimiter)

		pluginFlags := map[string]any{}
		for name, value := range actualFlags {
			if !flatHasPrefix(name, prefix, delimiter) {
				continue
			}
			delete(actualFlags, name)
			if !target.plugin.enable {
				if r.disabledPolicy == DisabledConfigError {
					return fmt.Errorf("flag %q provided for disabled plugin %q", name, target.namespace)
				}
				continue
			}
			pluginFlags[name] = value
		}
		if !target.plugin.enable {
			continue
		}

		loader := loaders[target]
		dupls := map[string]struct{}{}
		for _, field := range loader.fields {
			flagName := r.fullFlatName(target, field, "flag", prefix)
			if flagName == "" {
				continue
			}
			if err := loader.setField(field, flagName, pluginFlags, dupls); err != nil {
				return err
			}
		}
		for name := range pluginFlags {
			unknownFlags = append(unknownFlags, name)
		}
	}

	coreLoader := loaders[targets.core]
	dupls := map[string]struct{}{}
	for _, field := range coreLoader.fields {
		flagName := r.fullFlatName(targets.core, field, "flag", targets.core.rawFlagPrefix)
		if flagName == "" {
			continue
		}
		if err := coreLoader.setField(field, flagName, actualFlags, dupls); err != nil {
			return err
		}
	}
	if err := coreLoader.postFlagCheck(actualFlags, dupls); err != nil {
		return err
	}
	if len(unknownFlags) != 0 && !coreLoader.config.AllowUnknownFlags {
		sort.Strings(unknownFlags)
		return fmt.Errorf("unknown flag %s (see AllowUnknownFlags config param)", strings.Join(unknownFlags, ","))
	}
	return nil
}

func flatHasPrefix(name, prefix, delimiter string) bool {
	if prefix == "" {
		return false
	}
	return strings.HasPrefix(name, prefix+delimiter)
}
