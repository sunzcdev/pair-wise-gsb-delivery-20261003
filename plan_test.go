package aconfig

import (
	"errors"
	"io/fs"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

type planTestConfig struct {
	Host string `default:"localhost" json:"host" env:"HOST_NAME" flag:"host-name"`
	Port int    `default:"8080" json:"port"`
	Auth struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
}

func newTestPlan(t *testing.T) *Plan {
	t.Helper()
	plan, err := PlanFor(&planTestConfig{}, PlanConfig{
		EnvPrefix:  "APP",
		FlagPrefix: "app",
	})
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	return plan
}

func TestPlan_DoesNotRetainTarget(t *testing.T) {
	cfg := &planTestConfig{}
	plan, err := PlanFor(cfg, PlanConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Type() != reflect.TypeOf(planTestConfig{}) {
		t.Fatalf("unexpected plan type %s", plan.Type())
	}
	// Mutating the value handed to PlanFor must not affect later sessions.
	cfg.Host = "mutated"
	cfg.Port = 4242

	session, err := plan.NewSession(&planTestConfig{}, SessionConfig{
		SkipFiles: true,
		Envs:      []string{},
		Args:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Load(); err != nil {
		t.Fatal(err)
	}
}

func TestPlan_StructScanHappensOnce(t *testing.T) {
	before := StructuralScans()
	plan := newTestPlan(t)

	const n = 25
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			cfg := &planTestConfig{}
			data := `{"host":"h","port":1000,"auth":{"user":"u","pass":"p"}}`
			fsys := fstest.MapFS{
				"config.json": &fstest.MapFile{
					Data: []byte(data),
				},
			}
			session, err := plan.NewSession(cfg, SessionConfig{
				FileSystem: fsys,
				Files:      []string{"config.json"},
				Envs:       []string{"APP_HOST_NAME=" + "envhost" + string(rune('0'+i%10))},
				Args:       []string{"-app.port", strconv.Itoa(2000 + i)},
			})
			if err != nil {
				t.Errorf("session %d: %v", i, err)
				return
			}
			if err := session.Load(); err != nil {
				t.Errorf("load %d: %v", i, err)
				return
			}
			if cfg.Host != "envhost"+string(rune('0'+i%10)) {
				t.Errorf("session %d host = %q", i, cfg.Host)
			}
			if cfg.Port != 2000+i {
				t.Errorf("session %d port = %d", i, cfg.Port)
			}
			if cfg.Auth.User != "u" || cfg.Auth.Pass != "p" {
				t.Errorf("session %d auth = %+v", i, cfg.Auth)
			}
		}()
	}
	wg.Wait()

	// Many sessions, zero additional reflection scans.
	if got := StructuralScans() - before; got != 1 {
		t.Fatalf("structural scans = %d, want 1", got)
	}
}

func TestPlan_FailureDoesNotPolluteNextLoad(t *testing.T) {
	type requiredCfg struct {
		Must string `required:"true"`
	}
	rplan, err := PlanFor(&requiredCfg{}, PlanConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// First session fails (required field missing and missing file flagged).
	bad := &requiredCfg{}
	badSession, err := rplan.NewSession(bad, SessionConfig{
		SkipFiles: true,
		Envs:      []string{},
		Args:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := badSession.Load(); err == nil {
		t.Fatal("expected first load to fail")
	}

	// Same session cannot be replayed.
	if err := badSession.Load(); !errors.Is(err, ErrSessionLoaded) {
		t.Fatalf("replay error = %v, want ErrSessionLoaded", err)
	}

	// A fresh session over the plan succeeds: no flags set, no error cached.
	good := &requiredCfg{}
	goodSession, err := rplan.NewSession(good, SessionConfig{
		SkipFiles: true,
		Envs:      []string{"MUST=present"},
		Args:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := goodSession.Load(); err != nil {
		t.Fatalf("second load should succeed, got %v", err)
	}
	if good.Must != "present" {
		t.Fatalf("must = %q", good.Must)
	}
}

func TestPlan_SessionFlagsAreIndependent(t *testing.T) {
	plan := newTestPlan(t)

	s1, err := plan.NewSession(&planTestConfig{}, SessionConfig{
		SkipFiles: true,
		Envs:      []string{},
		Args:      []string{"-app.port", "11"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := plan.NewSession(&planTestConfig{}, SessionConfig{
		SkipFiles: true,
		Envs:      []string{},
		Args:      []string{"-app.port", "22"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if s1.Flags() == s2.Flags() {
		t.Fatal("sessions share the same flag.FlagSet")
	}

	if err := s1.Load(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
}

// statefulDecoder is a CloneableFileDecoder carrying mutable state.
type statefulDecoder struct {
	clones *int64
	fsys   fs.FS
}

func (d *statefulDecoder) Format() string { return "cnf" }

func (d *statefulDecoder) DecodeFile(filename string) (map[string]any, error) {
	return map[string]any{"host": "decoded", "port": 9999}, nil
}

func (d *statefulDecoder) Init(fsys fs.FS) { d.fsys = fsys }

func (d *statefulDecoder) Clone() FileDecoder {
	atomic.AddInt64(d.clones, 1)
	return &statefulDecoder{clones: d.clones}
}

func TestPlan_CloneableDecoderIsPerSession(t *testing.T) {
	var clones int64
	dec := &statefulDecoder{clones: &clones}
	plan, err := PlanFor(&planTestConfig{}, PlanConfig{
		FileDecoders: map[string]FileDecoder{".cnf": dec},
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := &planTestConfig{}
			session, err := plan.NewSession(cfg, SessionConfig{
				FileSystem: fstest.MapFS{"x.cnf": &fstest.MapFile{Data: []byte("ignored")}},
				Files:      []string{"x.cnf"},
				Envs:       []string{},
				Args:       []string{},
			})
			if err != nil {
				t.Errorf("session: %v", err)
				return
			}
			if err := session.Load(); err != nil {
				t.Errorf("load: %v", err)
				return
			}
			if cfg.Port != 9999 || cfg.Host != "decoded" {
				t.Errorf("cfg = %+v", cfg)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&clones); got != 10 {
		t.Fatalf("Clone calls = %d, want 10", got)
	}
}

// unsafeDecoder is a pointer to a non-struct and does not implement
// CloneableFileDecoder, so it cannot be copied nor safely shared.
type unsafeDecoder int

func (*unsafeDecoder) Format() string                            { return "bad" }
func (*unsafeDecoder) DecodeFile(string) (map[string]any, error) { return nil, nil }

func TestPlan_UnsafeDecoderIsRejected(t *testing.T) {
	_, err := PlanFor(&planTestConfig{}, PlanConfig{
		FileDecoders: map[string]FileDecoder{".bad": new(unsafeDecoder)},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be safely reused") {
		t.Fatalf("err = %v, want rejection of unsafe decoder", err)
	}
}

func TestPlan_IndependentSessionsDifferentValues(t *testing.T) {
	plan := newTestPlan(t)

	type result struct {
		host string
		port int
	}
	results := make([]result, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			cfg := &planTestConfig{}
			session, err := plan.NewSession(cfg, SessionConfig{
				SkipFiles: true,
				Envs:      []string{"APP_HOST_NAME=" + "h" + string(rune('a'+i))},
				Args:      []string{"-app.port", strconv.Itoa(3000 + i)},
			})
			if err != nil {
				t.Error(err)
				return
			}
			if err := session.Load(); err != nil {
				t.Error(err)
				return
			}
			results[i] = result{host: cfg.Host, port: cfg.Port}
		}()
	}
	wg.Wait()

	for i, r := range results {
		wantHost := "h" + string(rune('a'+i))
		if r.host != wantHost || r.port != 3000+i {
			t.Fatalf("result %d = %+v, want host %s port %d", i, r, wantHost, 3000+i)
		}
	}
}

func TestPlan_TypeMismatchRejected(t *testing.T) {
	plan := newTestPlan(t)

	type other struct{ X int }
	if _, err := plan.NewSession(&other{}, SessionConfig{}); err == nil {
		t.Fatal("expected type mismatch error")
	}
}

func TestPlan_BadDuplicateNamesRejectedAtBuild(t *testing.T) {
	type dup struct {
		A string `env:"SAME"`
		B string `env:"SAME"`
	}
	if _, err := PlanFor(&dup{}, PlanConfig{SkipFlags: true}); err == nil {
		t.Fatal("expected duplicate env error")
	}
}

type embeddedBase struct {
	Shared string `default:"base" json:"shared" env:"SHARED" flag:"shared"`
}

type embeddedPlanCfg struct {
	embeddedBase
	Nested *struct {
		Deep int `default:"7" json:"deep" env:"DEEP" flag:"deep"`
	}
	Top string `default:"top" json:"top" env:"TOP" flag:"top"`
}

func TestPlan_EmbeddedAndPointerStruct(t *testing.T) {
	plan, err := PlanFor(&embeddedPlanCfg{}, PlanConfig{})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &embeddedPlanCfg{}
	session, err := plan.NewSession(cfg, SessionConfig{
		SkipFiles: true,
		Envs:      []string{"NESTED_DEEP=13"},
		Args:      []string{"-top", "fromflag"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	session.WalkFields(func(f Field) bool {
		names = append(names, f.Name())
		return true
	})

	if err := session.Load(); err != nil {
		t.Fatal(err)
	}
	if cfg.Shared != "base" {
		t.Fatalf("embedded default = %q", cfg.Shared)
	}
	if cfg.Nested == nil || cfg.Nested.Deep != 13 {
		t.Fatalf("pointer struct deep = %+v", cfg.Nested)
	}
	if cfg.Top != "fromflag" {
		t.Fatalf("top = %q", cfg.Top)
	}
	if len(names) != 3 {
		t.Fatalf("walked names = %v", names)
	}
}

func TestPlan_ParityWithLoader(t *testing.T) {
	data := `{"host":"filehost","port":4321,"auth":{"user":"fu","pass":"fp"}}`
	fsys := fstest.MapFS{"config.json": &fstest.MapFile{Data: []byte(data)}}

	loadWithPlan := func(envs, args []string) *planTestConfig {
		t.Helper()
		plan := newTestPlan(t)
		cfg := &planTestConfig{}
		session, err := plan.NewSession(cfg, SessionConfig{
			FileSystem: fsys,
			Files:      []string{"config.json"},
			Envs:       envs,
			Args:       args,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Load(); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	envs := []string{"APP_HOST_NAME=envhost"}
	args := []string{"-app.port", "5555"}
	planCfg := loadWithPlan(envs, args)

	loaderCfg := &planTestConfig{}
	loader := LoaderFor(loaderCfg, Config{
		EnvPrefix:  "APP",
		FlagPrefix: "app",
		FileSystem: fsys,
		Files:      []string{"config.json"},
		Envs:       envs,
		Args:       args,
	})
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(planCfg, loaderCfg) {
		t.Fatalf("plan = %+v\nloader = %+v", planCfg, loaderCfg)
	}
}

// counterValue is a stateful flag.Value registered on a session flag set.
type counterValue struct{ n int }

func (c *counterValue) String() string { return strconv.Itoa(c.n) }
func (c *counterValue) Set(s string) error {
	v, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	c.n = v
	return nil
}

func TestPlan_FlagValueStateIsScopedToSession(t *testing.T) {
	plan := newTestPlan(t)

	makeSession := func(arg string) (*Session, *counterValue) {
		cfg := &planTestConfig{}
		session, err := plan.NewSession(cfg, SessionConfig{
			SkipFiles: true,
			Envs:      []string{},
			Args:      []string{"-count", arg},
		})
		if err != nil {
			t.Fatal(err)
		}
		value := &counterValue{}
		session.Flags().Var(value, "count", "session-local counter")
		return session, value
	}

	s1, v1 := makeSession("100")
	s2, v2 := makeSession("200")

	if err := s1.Load(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}

	if v1.n != 100 || v2.n != 200 {
		t.Fatalf("flag values leaked: v1=%d v2=%d", v1.n, v2.n)
	}
}
