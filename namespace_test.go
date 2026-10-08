package aconfig_test

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cristalhq/aconfig"
)

// Two fake plugins with zero external dependencies.
// They intentionally share field names (Enabled, Address, Token) to prove
// that namespaces isolate fields of different plugins.
type fakeCacheConfig struct {
	Enabled bool   `default:"true"`
	Port    int    `default:"6379"`
	Address string `default:"cache.local:6379"`
	Token   string `required:"true"`
}

type fakeMetricsConfig struct {
	Enabled bool   `default:"false"`
	Port    int    `default:"9090"`
	Address string `default:"metrics.local:9090"`
	Token   string `required:"true"`
}

type registryCoreConfig struct {
	Port int `default:"8080"`
	Auth struct {
		User string `default:"core-user"`
	}
}

func newTestRegistry(t *testing.T, core any, cfg aconfig.Config) *aconfig.Registry {
	t.Helper()

	registry := aconfig.NewRegistry(core, cfg)

	cache := &fakeCacheConfig{}
	metrics := &fakeMetricsConfig{}
	if _, err := registry.RegisterPlugin("cache", cache); err != nil {
		t.Fatalf("register cache: %v", err)
	}
	if _, err := registry.RegisterPlugin("metrics", metrics); err != nil {
		t.Fatalf("register metrics: %v", err)
	}
	t.Cleanup(func() {
		for _, name := range []string{
			"APP_PORT", "APP_CACHE_TOKEN", "APP_CACHE_ADDRESS", "APP_METRICS_TOKEN",
			"CACHE_PORT", "BOGUS_TOKEN", "APP_CACHE_TOKN",
		} {
			os.Unsetenv(name)
		}
	})
	return registry
}

func TestRegistryNameConflict(t *testing.T) {
	t.Run("duplicate namespace", func(t *testing.T) {
		var core registryCoreConfig
		registry := aconfig.NewRegistry(&core, aconfig.Config{
			SkipDefaults: true,
			SkipFiles:    true,
			SkipEnv:      true,
			SkipFlags:    true,
		})
		var cache1, cache2 fakeCacheConfig
		if _, err := registry.RegisterPlugin("cache", &cache1); err != nil {
			t.Fatal(err)
		}
		_, err := registry.RegisterPlugin("cache", &cache2)
		if err == nil || !strings.Contains(err.Error(), "registered twice") {
			t.Fatalf("want duplicate namespace error, got: %v", err)
		}
	})

	t.Run("namespace collides with a core file field", func(t *testing.T) {
		var collisionCore struct {
			Cache string `json:"cache"`
			Port  int
		}
		registry := aconfig.NewRegistry(&collisionCore, aconfig.Config{
			SkipDefaults: true,
			SkipFiles:    true,
			SkipEnv:      true,
			SkipFlags:    true,
		})
		var cache fakeCacheConfig
		if _, err := registry.RegisterPlugin("cache", &cache); err != nil {
			t.Fatal(err)
		}
		err := registry.Load()
		if err == nil || !strings.Contains(err.Error(), `namespace "cache" conflicts`) {
			t.Fatalf("want namespace conflict error, got: %v", err)
		}
	})

	t.Run("unknown plugin namespace in file is rejected", func(t *testing.T) {
		var core registryCoreConfig
		registry := newTestRegistry(t, &core, aconfig.Config{
			SkipDefaults: true,
			SkipEnv:      true,
			SkipFlags:    true,
			Files:        []string{"config.json"},
			FileSystem: fstest.MapFS{
				"config.json": {Data: []byte(`{"bogus": {"token": "x"}}`)},
			},
		})
		err := registry.Load()
		if err == nil || !strings.Contains(err.Error(), `unknown field in file`) {
			t.Fatalf("want unknown field error, got: %v", err)
		}
	})

	t.Run("unknown core field is still rejected", func(t *testing.T) {
		var core registryCoreConfig
		registry := newTestRegistry(t, &core, aconfig.Config{
			SkipDefaults: true,
			SkipEnv:      true,
			SkipFlags:    true,
			Files:        []string{"config.json"},
			FileSystem: fstest.MapFS{
				"config.json": {Data: []byte(`{"prt": 9000}`)},
			},
		})
		err := registry.Load()
		if err == nil || !strings.Contains(err.Error(), `unknown field in file`) {
			t.Fatalf("want unknown core field error, got: %v", err)
		}
	})

	t.Run("unregistered env namespace is rejected", func(t *testing.T) {
		// Plugin namespaces are strict even when the core has no env prefix:
		// a variable prefixed with an unregistered plugin-like namespace is
		// still rejected because the registry recognizes all plugin namespaces.
		os.Setenv("APP_CACHE_TOKN", "x")
		os.Setenv("APP_CACHE_TOKEN", "c")
		os.Setenv("APP_METRICS_TOKEN", "m")

		var core registryCoreConfig
		registry := newTestRegistry(t, &core, aconfig.Config{
			SkipDefaults: true,
			SkipFiles:    true,
			SkipFlags:    true,
			EnvPrefix:    "APP",
		})

		err := registry.Load()
		if err == nil || !strings.Contains(err.Error(), "APP_CACHE_TOKN") {
			t.Fatalf("want unknown env error, got: %v", err)
		}
	})
}

func TestRegistryPartialFailureIsAtomic(t *testing.T) {
	fsys := fstest.MapFS{
		"config.json": {Data: []byte(`{
			"port": 9000,
			"cache":  {"port": "not-a-number"},
			"metrics": {"port": 7070}
		}`)},
	}

	var core registryCoreConfig
	core.Port = 1111
	var cache fakeCacheConfig
	cache.Port = 2222
	var metrics fakeMetricsConfig
	metrics.Port = 3333

	registry := aconfig.NewRegistry(&core, aconfig.Config{
		SkipDefaults: true,
		SkipEnv:      true,
		SkipFlags:    true,
		Files:        []string{"config.json"},
		FileSystem:   fsys,
	})
	if _, err := registry.RegisterPlugin("cache", &cache); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RegisterPlugin("metrics", &metrics); err != nil {
		t.Fatal(err)
	}

	err := registry.Load()
	if err == nil {
		t.Fatal("want error from invalid cache port")
	}

	// Nothing is published: every destination keeps its pre-load values.
	if core.Port != 1111 {
		t.Fatalf("core changed on failed load: %+v", core)
	}
	if cache.Port != 2222 {
		t.Fatalf("cache changed on failed load: %+v", cache)
	}
	if metrics.Port != 3333 {
		t.Fatalf("metrics changed on failed load: %+v", metrics)
	}
}

func TestRegistrySourcePrecedence(t *testing.T) {
	fsys := fstest.MapFS{
		"config.json": {Data: []byte(`{
			"port": 8081,
			"cache": {"port": 6400, "address": "file-cache:6400"},
			"metrics": {"port": 9100}
		}`)},
	}

	os.Setenv("APP_CACHE_TOKEN", "env-token")
	os.Setenv("APP_METRICS_TOKEN", "metrics-token")
	os.Setenv("APP_CACHE_ADDRESS", "env-cache:7000")
	os.Setenv("APP_PORT", "8082")
	t.Cleanup(func() {
		for _, name := range []string{
			"APP_CACHE_TOKEN", "APP_METRICS_TOKEN", "APP_CACHE_ADDRESS", "APP_PORT",
		} {
			os.Unsetenv(name)
		}
	})

	var core registryCoreConfig
	var cache fakeCacheConfig
	var metrics fakeMetricsConfig

	registry := aconfig.NewRegistry(&core, aconfig.Config{
		EnvPrefix:  "APP",
		FlagPrefix: "app",
		Args: []string{
			"-app.cache.address=flag-cache:7100",
		},
		Files:      []string{"config.json"},
		FileSystem: fsys,
	})
	if _, err := registry.RegisterPlugin("cache", &cache); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RegisterPlugin("metrics", &metrics); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Flags(); err != nil {
		t.Fatal(err)
	}
	if err := registry.Load(); err != nil {
		t.Fatal(err)
	}

	// core: default 8080 < file 8081 < env 8082
	if core.Port != 8082 {
		t.Fatalf("core port = %d, want 8082", core.Port)
	}

	// cache: default < file < env < flag for each field, independently.
	if cache.Port != 6400 {
		t.Fatalf("cache port = %d, want 6400 (file)", cache.Port)
	}
	if cache.Address != "flag-cache:7100" {
		t.Fatalf("cache address = %q, want flag value", cache.Address)
	}
	if cache.Token != "env-token" {
		t.Fatalf("cache token = %q, want env value", cache.Token)
	}

	// metrics keeps its own values despite same field names as cache.
	if metrics.Port != 9100 {
		t.Fatalf("metrics port = %d, want 9100 (file)", metrics.Port)
	}
	if metrics.Address != "metrics.local:9090" {
		t.Fatalf("metrics address = %q, want default", metrics.Address)
	}
	if metrics.Token != "metrics-token" {
		t.Fatalf("metrics token = %q, want its own token", metrics.Token)
	}

	// Defaults from the other plugin did not leak.
	if cache.Enabled != true {
		t.Fatalf("cache enabled = %v, want true default", cache.Enabled)
	}
	if metrics.Enabled != false {
		t.Fatalf("metrics enabled = %v, want false default", metrics.Enabled)
	}
}

func TestRegistryDisabledPlugin(t *testing.T) {
	t.Run("config for disabled plugin fails by default", func(t *testing.T) {
		var core registryCoreConfig
		var cache fakeCacheConfig
		var metrics fakeMetricsConfig

		registry := aconfig.NewRegistry(&core, aconfig.Config{
			SkipDefaults: true,
			SkipEnv:      true,
			SkipFlags:    true,
			Files:        []string{"config.json"},
			FileSystem: fstest.MapFS{
				"config.json": {Data: []byte(`{"cache": {"port": 6400}}`)},
			},
		})
		if _, err := registry.RegisterPlugin("cache", &cache); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.RegisterPlugin("metrics", &metrics); err != nil {
			t.Fatal(err)
		}
		if err := registry.Disable("cache"); err != nil {
			t.Fatal(err)
		}

		err := registry.Load()
		if err == nil || !strings.Contains(err.Error(), `disabled plugin "cache"`) {
			t.Fatalf("want disabled plugin error, got: %v", err)
		}
	})

	t.Run("ignore policy skips disabled plugin and its required fields", func(t *testing.T) {
		os.Setenv("APP_CACHE_TOKEN", "should-be-ignored")
		t.Cleanup(func() {
			os.Unsetenv("APP_CACHE_TOKEN")
			os.Unsetenv("APP_METRICS_TOKEN")
		})

		var core registryCoreConfig
		var cache fakeCacheConfig
		var metrics fakeMetricsConfig

		registry := aconfig.NewRegistry(&core, aconfig.Config{
			SkipDefaults: true,
			SkipFiles:    true,
			SkipFlags:    true,
			EnvPrefix:    "APP",
		})
		if _, err := registry.RegisterPlugin("cache", &cache); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.RegisterPlugin("metrics", &metrics); err != nil {
			t.Fatal(err)
		}
		if err := registry.Disable("cache"); err != nil {
			t.Fatal(err)
		}
		os.Setenv("APP_METRICS_TOKEN", "m-token")
		registry.SetDisabledConfigPolicy(aconfig.DisabledConfigIgnore)

		if err := registry.Load(); err != nil {
			t.Fatal(err)
		}

		// Disabled plugin is not populated and required Token does not fail.
		if cache.Token != "" || cache.Port != 0 {
			t.Fatalf("disabled cache populated: %+v", cache)
		}
		// Enabled plugin still loads.
		if metrics.Token != "m-token" {
			t.Fatalf("metrics token = %q, want m-token", metrics.Token)
		}
	})

	t.Run("flag for disabled plugin fails by default", func(t *testing.T) {
		var core registryCoreConfig
		var cache fakeCacheConfig
		var metrics fakeMetricsConfig

		registry := aconfig.NewRegistry(&core, aconfig.Config{
			SkipDefaults: true,
			SkipFiles:    true,
			SkipEnv:      true,
			FlagPrefix:   "app",
			Args:         []string{"-app.cache.port=1234", "-app.metrics.token=m"},
		})
		if _, err := registry.RegisterPlugin("cache", &cache); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.RegisterPlugin("metrics", &metrics); err != nil {
			t.Fatal(err)
		}
		if err := registry.Disable("cache"); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Flags(); err != nil {
			t.Fatal(err)
		}

		err := registry.Load()
		if err == nil || !strings.Contains(err.Error(), `disabled plugin "cache"`) {
			t.Fatalf("want disabled plugin flag error, got: %v", err)
		}
	})
}
