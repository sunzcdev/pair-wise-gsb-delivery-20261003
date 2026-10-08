package aconfig

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// Two fake plugins with no external dependencies. Both declare a field
// named Enabled to prove namespaces keep same-named fields isolated.

type fakeAuthConfig struct {
	Token   string `required:"true"`
	Enabled bool
}

type fakeCacheConfig struct {
	TTL     int    `default:"10"`
	Mode    string `default:"redis"`
	Enabled bool
}

type fakeTLSConfig struct {
	Cert string
}

type fakeNestedAuthConfig struct {
	Token string `required:"true"`
	TLS   fakeTLSConfig
}

type fakeCoreConfig struct {
	Name string `default:"core-def"`
	Port int    `default:"8080"`
}

func fakePlugins(auth, cache bool) (*fakeCoreConfig, *fakeAuthConfig, *fakeCacheConfig, []PluginSpec) {
	core := &fakeCoreConfig{}
	authCfg := &fakeAuthConfig{}
	cacheCfg := &fakeCacheConfig{}
	specs := []PluginSpec{
		{Namespace: "auth", Prefix: "auth", Config: authCfg, Enabled: auth},
		{Namespace: "cache", Prefix: "cache", Config: cacheCfg, Enabled: cache},
	}
	return core, authCfg, cacheCfg, specs
}

func TestPluginNamespaceConflict(t *testing.T) {
	t.Run("duplicate namespace", func(t *testing.T) {
		core := &fakeCoreConfig{}
		loader := LoaderFor(core, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
			Plugins: []PluginSpec{
				{Namespace: "auth", Prefix: "auth", Config: &fakeAuthConfig{}, Enabled: true},
				{Namespace: "auth", Prefix: "other", Config: &fakeCacheConfig{}, Enabled: true},
			},
		})
		err := loader.Load()
		if err == nil || !strings.Contains(err.Error(), `namespace "auth" is registered more than once`) {
			t.Fatalf("want duplicate namespace error, got %v", err)
		}
	})

	t.Run("duplicate prefix", func(t *testing.T) {
		core := &fakeCoreConfig{}
		loader := LoaderFor(core, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
			Plugins: []PluginSpec{
				{Namespace: "auth", Prefix: "auth", Config: &fakeAuthConfig{}, Enabled: true},
				{Namespace: "auth2", Prefix: "Auth", Config: &fakeCacheConfig{}, Enabled: true},
			},
		})
		if err := loader.Load(); err == nil || !strings.Contains(err.Error(), "claimed by") {
			t.Fatalf("want prefix conflict error, got %v", err)
		}
	})

	t.Run("conflict with core field", func(t *testing.T) {
		type coreWithAuth struct {
			Auth struct{ Port int }
			fakeCoreConfig
		}
		core := &coreWithAuth{}
		loader := LoaderFor(core, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
			Plugins: []PluginSpec{
				{Namespace: "auth", Prefix: "auth", Config: &fakeAuthConfig{}, Enabled: true},
			},
		})
		if err := loader.Load(); err == nil || !strings.Contains(err.Error(), "conflicts with core field") {
			t.Fatalf("want core conflict error, got %v", err)
		}
	})

	t.Run("bad plugin spec", func(t *testing.T) {
		core := &fakeCoreConfig{}
		loader := LoaderFor(core, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
			Plugins: []PluginSpec{
				{Namespace: "auth", Prefix: "auth.seg", Config: &fakeAuthConfig{}, Enabled: true},
			},
		})
		if err := loader.Load(); err == nil {
			t.Fatal("want invalid prefix error")
		}
	})
}

func TestPluginSourcePriority(t *testing.T) {
	const filepath = "config.json"
	file := &fstest.MapFile{Data: []byte(`{
		"name": "core-file",
		"port": 9090,
		"auth":  {"token": "auth-file", "enabled": true},
		"cache": {"ttl": 30, "enabled": false}
	}`)}

	core, authCfg, cacheCfg, specs := fakePlugins(true, true)
	loader := LoaderFor(core, Config{
		EnvPrefix:  "APP",
		FlagPrefix: "svc",
		Files:      []string{filepath},
		FileSystem: fstest.MapFS{filepath: file},
		Envs: []string{
			"APP_AUTH_TOKEN=auth-env",
			"APP_CACHE_TTL=60",
		},
		Args: []string{
			"-svc.auth.token=auth-flag",
		},
		Plugins: specs,
	})
	failIfErr(t, loader.Load())

	// defaults < file < env < flag
	if authCfg.Token != "auth-flag" {
		t.Fatalf("auth.Token = %q, want auth-flag", authCfg.Token)
	}
	// file overrides defaults, nothing overrides it later
	if core.Name != "core-file" {
		t.Fatalf("core.Name = %q, want core-file", core.Name)
	}
	if core.Port != 9090 {
		t.Fatalf("core.Port = %d, want 9090", core.Port)
	}
	// env overrides file
	if cacheCfg.TTL != 60 {
		t.Fatalf("cache.TTL = %d, want 60", cacheCfg.TTL)
	}
	// same-named fields stay isolated per namespace
	if !authCfg.Enabled {
		t.Fatal("auth.Enabled must come from its own namespace")
	}
	if cacheCfg.Enabled {
		t.Fatal("cache.Enabled must not be overwritten by auth.Enabled")
	}
	if cacheCfg.Mode != "redis" {
		t.Fatalf("cache.Mode = %q, want redis default", cacheCfg.Mode)
	}

	got, ok := loader.Plugin("auth")
	if !ok || got != any(authCfg) {
		t.Fatalf("Plugin(auth) = %v, %v", got, ok)
	}
}

func TestPluginPartialFailureRollsBack(t *testing.T) {
	const filepath = "config.json"
	// Unknown plugin field and an unregistered namespace in one release.
	file := &fstest.MapFile{Data: []byte(`{
		"name": "core-file",
		"auth":  {"token": "auth-file", "bogus": true},
		"cache": {"ttl": 30},
		"mystery": {"x": 1}
	}`)}

	core, authCfg, cacheCfg, specs := fakePlugins(true, true)
	loader := LoaderFor(core, Config{
		EnvPrefix:  "APP",
		FlagPrefix: "svc",
		Files:      []string{filepath},
		FileSystem: fstest.MapFS{filepath: file},
		Envs:       []string{"APP_CACHE_TTL=60"},
		Plugins:    specs,
		Args:       []string{},
	})

	err := loader.Load()
	if err == nil {
		t.Fatal("want load error")
	}

	// Whole release is consistent: nothing is partially published.
	wantCore := fakeCoreConfig{}
	if !reflect.DeepEqual(*core, wantCore) {
		t.Fatalf("core not rolled back: %+v", *core)
	}
	if *authCfg != (fakeAuthConfig{}) {
		t.Fatalf("auth not rolled back: %+v", *authCfg)
	}
	if *cacheCfg != (fakeCacheConfig{}) {
		t.Fatalf("cache not rolled back: %+v", *cacheCfg)
	}
}

func TestPluginUnknownEnvAndRequired(t *testing.T) {
	t.Run("misspelled plugin env is rejected", func(t *testing.T) {
		core, _, _, specs := fakePlugins(true, false)
		loader := LoaderFor(core, Config{
			SkipFiles: true,
			SkipFlags: true,
			EnvPrefix: "APP",
			Envs:      []string{"APP_AUTH_TOKNE=x"},
			Plugins:   specs,
		})
		err := loader.Load()
		if err == nil || !strings.Contains(err.Error(), "unknown env APP_AUTH_TOKNE for plugin auth") {
			t.Fatalf("want unknown plugin env error, got %v", err)
		}
	})

	t.Run("required only for enabled plugins", func(t *testing.T) {
		core, authCfg, _, specs := fakePlugins(false, false)
		loader := LoaderFor(core, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
			Plugins: specs,
		})
		// auth is disabled: its required Token must not fail the load.
		failIfErr(t, loader.Load())
		if authCfg.Token != "" {
			t.Fatalf("disabled plugin must not be populated, got %q", authCfg.Token)
		}

		// A fresh loader and destination with the plugin enabled
		// must now enforce required.
		core2, authCfg2, _, specs2 := fakePlugins(false, false)
		loader = LoaderFor(core2, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
			Plugins: specs2,
		})
		failIfErr(t, loader.SetPluginEnabled("auth", true))
		err := loader.Load()
		if err == nil || !strings.Contains(err.Error(), "fields required but not set") {
			t.Fatalf("want required error for enabled plugin, got %v", err)
		}
		if authCfg2.Token != "" {
			t.Fatalf("failed load must not populate plugin, got %q", authCfg2.Token)
		}

		if _, ok := loader.Plugin("cache"); !ok {
			t.Fatal("registered disabled plugin must still be retrievable")
		}
	})

	t.Run("enable unknown namespace", func(t *testing.T) {
		loader := LoaderFor(&fakeCoreConfig{}, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
		})
		if err := loader.SetPluginEnabled("ghost", true); err == nil {
			t.Fatal("want error enabling unregistered plugin")
		}
	})
}

func TestDisabledPluginPolicy(t *testing.T) {
	const filepath = "config.json"
	file := &fstest.MapFile{Data: []byte(`{
		"auth":  {"token": "auth-file"},
		"cache": {"ttl": 30, "mode": "mem"}
	}`)}

	t.Run("reject by default", func(t *testing.T) {
		core, _, cacheCfg, specs := fakePlugins(true, false)
		loader := LoaderFor(core, Config{
			EnvPrefix:  "APP",
			FlagPrefix: "svc",
			Files:      []string{filepath},
			FileSystem: fstest.MapFS{filepath: file},
			Plugins:    specs,
			Args:       []string{},
		})
		err := loader.Load()
		if err == nil || !strings.Contains(err.Error(), `disabled plugin "cache"`) {
			t.Fatalf("want disabled plugin error, got %v", err)
		}
		if cacheCfg.TTL != 0 || cacheCfg.Mode != "" {
			t.Fatalf("disabled plugin must stay empty, got %+v", cacheCfg)
		}
	})

	t.Run("ignore when configured", func(t *testing.T) {
		core, authCfg, cacheCfg, specs := fakePlugins(true, false)
		loader := LoaderFor(core, Config{
			EnvPrefix:            "APP",
			FlagPrefix:           "svc",
			Files:                []string{filepath},
			FileSystem:           fstest.MapFS{filepath: file},
			Envs:                 []string{"APP_CACHE_TTL=60"},
			Plugins:              specs,
			DisabledPluginPolicy: IgnoreDisabledPluginConfig,
			Args:                 []string{},
		})
		failIfErr(t, loader.Load())

		if authCfg.Token != "auth-file" {
			t.Fatalf("auth.Token = %q", authCfg.Token)
		}
		if cacheCfg.TTL != 0 || cacheCfg.Mode != "" {
			t.Fatalf("disabled plugin values must be ignored, got %+v", cacheCfg)
		}
	})
}

func TestPluginUnknownCoreField(t *testing.T) {
	const filepath = "config.json"
	file := &fstest.MapFile{Data: []byte(`{
		"naem": "typo",
		"auth": {"token": "auth-file"}
	}`)}

	core, _, _, specs := fakePlugins(true, false)
	loader := LoaderFor(core, Config{
		EnvPrefix:  "APP",
		FlagPrefix: "svc",
		Files:      []string{filepath},
		FileSystem: fstest.MapFS{filepath: file},
		Plugins:    specs,
		Args:       []string{},
	})
	err := loader.Load()
	if err == nil || !strings.Contains(err.Error(), `unknown field in file`) || !strings.Contains(err.Error(), "naem") {
		t.Fatalf("want strict core unknown field error, got %v", err)
	}
}

func TestPluginRetryAfterFailure(t *testing.T) {
	badFile, goodFile := "bad.json", "good.json"

	core, authCfg, _, specs := fakePlugins(true, false)
	loader := LoaderFor(core, Config{
		EnvPrefix:  "APP",
		FlagPrefix: "svc",
		Files:      []string{badFile},
		FileSystem: fstest.MapFS{
			badFile:  {Data: []byte(`{"auth": {"tokne": "x"}}`)},
			goodFile: {Data: []byte(`{"auth": {"token": "auth-file"}}`)},
		},
		Envs:    []string{},
		Args:    []string{},
		Plugins: specs,
	})
	if err := loader.Load(); err == nil {
		t.Fatal("first load with unknown field must fail")
	}

	// Point at a valid document and retry on the same loader.
	loader.config.Files = []string{goodFile}
	failIfErr(t, loader.Load())

	if authCfg.Token != "auth-file" {
		t.Fatalf("retry did not populate plugin: %q", authCfg.Token)
	}
}

func TestPluginNestedAndFlags(t *testing.T) {
	t.Run("nested struct from file and flag override", func(t *testing.T) {
		const filepath = "config.json"
		file := &fstest.MapFile{Data: []byte(`{
			"auth": {"token": "auth-file", "tls": {"cert": "/tmp/cert.pem"}}
		}`)}

		core := &fakeCoreConfig{}
		authCfg := &fakeNestedAuthConfig{}
		loader := LoaderFor(core, Config{
			EnvPrefix:  "APP",
			FlagPrefix: "svc",
			Files:      []string{filepath},
			FileSystem: fstest.MapFS{filepath: file},
			Args:       []string{"-svc.auth.tls.cert=/flag/cert"},
			Plugins: []PluginSpec{
				{Namespace: "auth", Prefix: "auth", Config: authCfg, Enabled: true},
			},
		})
		failIfErr(t, loader.Load())

		if authCfg.Token != "auth-file" {
			t.Fatalf("Token = %q", authCfg.Token)
		}
		if authCfg.TLS.Cert != "/flag/cert" {
			t.Fatalf("TLS.Cert = %q, want /flag/cert", authCfg.TLS.Cert)
		}
	})

	t.Run("unknown plugin flag is rejected", func(t *testing.T) {
		core := &fakeCoreConfig{}
		loader := LoaderFor(core, Config{
			SkipFiles: true, SkipEnv: true,
			EnvPrefix:  "APP",
			FlagPrefix: "svc",
			Args:       []string{"-svc.auth.tokne=x"},
			Plugins: []PluginSpec{
				{Namespace: "auth", Prefix: "auth", Config: &fakeAuthConfig{}, Enabled: true},
			},
		})
		err := loader.Load()
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("want unknown flag error at parse time, got %v", err)
		}
	})

	t.Run("config for disabled plugin via flag", func(t *testing.T) {
		core := &fakeCoreConfig{}
		cacheCfg := &fakeCacheConfig{}
		loader := LoaderFor(core, Config{
			SkipFiles: true, SkipEnv: true,
			EnvPrefix:  "APP",
			FlagPrefix: "svc",
			Args:       []string{"-svc.cache.ttl=42"},
			Plugins: []PluginSpec{
				{Namespace: "auth", Prefix: "auth", Config: &fakeAuthConfig{}, Enabled: true},
				{Namespace: "cache", Prefix: "cache", Config: cacheCfg, Enabled: false},
			},
		})
		err := loader.Load()
		if err == nil || !strings.Contains(err.Error(), `disabled plugin "cache"`) {
			t.Fatalf("want disabled plugin error, got %v", err)
		}
	})
}
