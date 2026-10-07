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
* Reusable parse plans to load the same struct many times without re-scanning it.

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

## Reusable plans

Services that load the same configuration struct many times (e.g. per
request or per task) can pay the reflection and name-validation cost only
once with an immutable `Plan`:

```go
plan, err := aconfig.NewPlan(&MyConfig{}, aconfig.Config{
	EnvPrefix: "APP", // name-affecting options are bound to the plan
})
if err != nil {
	panic(err)
}

// later, as many times as needed, also concurrently:
var cfg MyConfig
loader, err := plan.LoaderFor(&cfg, aconfig.Config{
	Files: []string{"config.json"},
	Envs:  os.Environ(),
	Args:  os.Args[1:],
})
if err != nil {
	panic(err)
}
if err := loader.Load(); err != nil {
	panic(err)
}
```

A `Plan` holds no destination, no parsed flags and no load errors, so
sessions are fully independent. Stateful file decoders must implement
`aconfig.FileDecoderCloner` (each session gets its own clone), otherwise
`NewPlan` rejects them.

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
