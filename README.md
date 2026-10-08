# aconfig

[![build-img]][build-url]
[![pkg-img]][pkg-url]
[![version-img]][version-url]

Simple, useful and opinionated config loader.

## Rationale

There are many solutions regarding configuration loading in Go. I was looking for a simple loader that is as easy to use and understand as possible. The goal was to load config from 4 places: defaults (in the code), files, environment variables, command-line flags. This library works with all of these sources.

## Features

* Simple API.
* Clean and tested code.
* Automatic fields mapping.
* Supports different sources:
  * defaults in the code
  * files (JSON, YAML, TOML, DotENV, HCL)
  * environment variables
  * command-line flags
* Dependency-free (file parsers are optional).
* Ability to walk over configuration fields.

## Install

Go version 1.14+

```
go get github.com/cristalhq/aconfig
```

## Example

```go
type MyConfig struct {
	Port int `default:"1111" usage:"just give a number"`
	Auth struct {
		User string `required:"true"`
		Pass string `required:"true"`
	}
	Pass string `default:"" env:"SECRET" flag:"sec_ret"`
}

var cfg MyConfig
loader := aconfig.LoaderFor(&cfg, aconfig.Config{
	// feel free to skip some steps :)
	// SkipDefaults: true,
	// SkipFiles:    true,
	// SkipEnv:      true,
	// SkipFlags:    true,
	EnvPrefix:       "APP",
	FlagPrefix:      "app",
	Files:           []string{"/var/opt/myapp/config.json", "ouch.yaml"},
	FileDecoders: map[string]aconfig.FileDecoder{
		// from `aconfigyaml` submodule
		// see submodules in repo for more formats
		".yaml": aconfigyaml.New(),
	},
})

// IMPORTANT: define your own flags with `flagSet`
flagSet := loader.Flags()

if err := loader.Load(); err != nil {
	panic(err)
}

// configuration fields will be loaded from (in order):
//
// 1. defaults set in structure tags (see MyConfig defenition)
// 2. loaded from files `file.json` if not `ouch.yaml` will be used
// 3. from corresponding environment variables with the prefix `APP_`
// 4. command-line flags with the prefix `app.` if they are
```

Also see examples: [examples_test.go](https://github.com/cristalhq/aconfig/blob/master/example_test.go).

Integration with `spf13/cobra` [playground](https://play.golang.org/p/OsCR8qTCN0H).

## Plugin namespaces

When a service is built from plugins that each declare their own config
structure, use a `Registry`. Every plugin registers its structure together
with a namespace. All namespaces are checked for conflicts before anything is
read; afterwards every source is read once and split between the core and the
enabled plugins.

```go
type CacheConfig struct {
	Port int `default:"6379"`
}
type MetricsConfig struct {
	Port int `default:"9090"`
}

var core MyConfig
var cache CacheConfig
var metrics MetricsConfig

registry := aconfig.NewRegistry(&core, aconfig.Config{
	EnvPrefix:  "APP",
	FlagPrefix: "app",
	Files:      []string{"config.json"},
})

cachePlugin, err := registry.RegisterPlugin("cache", &cache)
if err != nil {
	panic(err)
}
if _, err := registry.RegisterPlugin("metrics", &metrics); err != nil {
	panic(err)
}

// Plugins can be toggled before loading.
// registry.Disable("cache")

flags, err := registry.Flags() // shared flag set: app.port, app.cache.port, ...
if err != nil {
	panic(err)
}
_ = flags

if err := registry.Load(); err != nil {
	panic(err)
}
```

Fields are isolated by namespace, so plugins safely reuse the same field
names:

```json
{
	"port": 8080,
	"cache":   { "port": 6379 },
	"metrics": { "port": 9090 }
}
```

```sh
export APP_CACHE_PORT=6400   # cache only
# app.cache.port=6500        # flag form
```

Rules enforced by the registry:

- duplicate namespaces, a namespace colliding with a core field, or the same
  env/flag name claimed twice are rejected before loading;
- an unregistered namespace in a file/env/flag and an unknown core field are
  still rejected (same strictness as a plain loader);
- fields with equal names in different plugins never overwrite each other;
- `required` (and `AllFieldRequired`) is checked only for enabled plugins;
- configuration sent to a disabled plugin is an error by default
  (`DisabledConfigError`); use `SetDisabledConfigPolicy(DisabledConfigIgnore)`
  to silently drop it;
- loading is atomic: on any error none of the destinations are modified.

## Documentation

See [these docs][pkg-url].

## License

[MIT License](LICENSE).

[build-img]: https://github.com/cristalhq/aconfig/workflows/build/badge.svg
[build-url]: https://github.com/cristalhq/aconfig/actions
[pkg-img]: https://pkg.go.dev/badge/cristalhq/aconfig
[pkg-url]: https://pkg.go.dev/github.com/cristalhq/aconfig
[version-img]: https://img.shields.io/github/v/release/cristalhq/aconfig
[version-url]: https://github.com/cristalhq/aconfig/releases
