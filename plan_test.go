package aconfig

import (
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

type planTestConfig struct {
	Host string `default:"localhost" env:"HOST" flag:"host" usage:"host to connect"`
	Port int    `default:"8080" env:"PORT" flag:"port"`
	Auth struct {
		User string `env:"USER" flag:"user"`
		Pass string `env:"PASS" flag:"pass"`
	}
	Tags []string `default:"a,b" env:"TAGS"`
}

func TestPlanSessions(t *testing.T) {
	plan, err := NewPlan(&planTestConfig{}, Config{EnvPrefix: "APP"})
	if err != nil {
		t.Fatal(err)
	}

	var first planTestConfig
	l1, err := plan.LoaderFor(&first, Config{
		SkipFiles: true,
		Envs:      []string{"APP_PORT=9090", "APP_AUTH_USER=alice"},
		Args:      []string{"-auth.pass=s3cret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Load(); err != nil {
		t.Fatal(err)
	}

	var second planTestConfig
	l2, err := plan.LoaderFor(&second, Config{
		SkipFiles: true,
		Envs:      []string{"APP_HOST=example.com"},
		Args:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Load(); err != nil {
		t.Fatal(err)
	}

	if first.Host != "localhost" || first.Port != 9090 ||
		first.Auth.User != "alice" || first.Auth.Pass != "s3cret" {
		t.Fatalf("unexpected first config: %+v", first)
	}
	if second.Host != "example.com" || second.Port != 8080 ||
		second.Auth.User != "" || second.Auth.Pass != "" {
		t.Fatalf("unexpected second config: %+v", second)
	}
	if want := []string{"a", "b"}; fmt.Sprint(first.Tags) != fmt.Sprint(want) {
		t.Fatalf("unexpected tags: %v", first.Tags)
	}
}

func TestPlanScansStructOnlyOnce(t *testing.T) {
	before := atomic.LoadInt64(&structScanCount)

	plan, err := NewPlan(&planTestConfig{}, Config{EnvPrefix: "APP"})
	if err != nil {
		t.Fatal(err)
	}

	after := atomic.LoadInt64(&structScanCount)
	if after == before {
		t.Fatal("NewPlan did not scan the struct")
	}

	for i := 0; i < 10; i++ {
		var cfg planTestConfig
		l, err := plan.LoaderFor(&cfg, Config{
			SkipFiles: true,
			Envs:      []string{fmt.Sprintf("APP_PORT=%d", 2000+i)},
			Args:      []string{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Load(); err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 2000+i {
			t.Fatalf("session %d: got port %d", i, cfg.Port)
		}
	}

	if got := atomic.LoadInt64(&structScanCount); got != after {
		t.Fatalf("struct was rescanned by sessions: scanned %d fields, want %d", got, after)
	}
}

func TestPlanConcurrentSessions(t *testing.T) {
	plan, err := NewPlan(&planTestConfig{}, Config{EnvPrefix: "APP"})
	if err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var cfg planTestConfig
			l, err := plan.LoaderFor(&cfg, Config{
				SkipFiles: true,
				Envs: []string{
					fmt.Sprintf("APP_PORT=%d", 3000+i),
					fmt.Sprintf("APP_AUTH_USER=user-%d", i),
				},
				Args: []string{fmt.Sprintf("-auth.pass=pass-%d", i)},
			})
			if err != nil {
				errs <- err
				return
			}
			if err := l.Load(); err != nil {
				errs <- err
				return
			}
			if cfg.Port != 3000+i || cfg.Auth.User != fmt.Sprintf("user-%d", i) ||
				cfg.Auth.Pass != fmt.Sprintf("pass-%d", i) {
				errs <- fmt.Errorf("session %d loaded wrong values: %+v", i, cfg)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPlanFailureDoesNotPollute(t *testing.T) {
	plan, err := NewPlan(&planTestConfig{}, Config{EnvPrefix: "APP"})
	if err != nil {
		t.Fatal(err)
	}

	var bad planTestConfig
	l1, err := plan.LoaderFor(&bad, Config{
		SkipFiles: true,
		Envs:      []string{"APP_PORT=not-a-number"},
		Args:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Load(); err == nil {
		t.Fatal("expected load error for a broken env value")
	}

	var good planTestConfig
	l2, err := plan.LoaderFor(&good, Config{
		SkipFiles: true,
		Envs:      []string{"APP_PORT=1234"},
		Args:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Load(); err != nil {
		t.Fatalf("failed session polluted the plan: %v", err)
	}
	if good.Port != 1234 {
		t.Fatalf("got port %d, want 1234", good.Port)
	}
}

func TestPlanFlagStateIsSessionLocal(t *testing.T) {
	plan, err := NewPlan(&planTestConfig{}, Config{})
	if err != nil {
		t.Fatal(err)
	}

	var c1, c2 planTestConfig
	l1, err := plan.LoaderFor(&c1, Config{
		SkipFiles: true,
		Envs:      []string{},
		Args:      []string{"-port=5555"},
	})
	if err != nil {
		t.Fatal(err)
	}
	l2, err := plan.LoaderFor(&c2, Config{
		SkipFiles: true,
		Envs:      []string{},
		Args:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	if l1.Flags() == l2.Flags() {
		t.Fatal("sessions share a flag.FlagSet")
	}
	if err := l1.Load(); err != nil {
		t.Fatal(err)
	}
	if l2.Flags().Parsed() {
		t.Fatal("second session flags were parsed by the first session")
	}
	if err := l2.Load(); err != nil {
		t.Fatal(err)
	}

	if !l1.Flags().Parsed() {
		t.Fatal("first session flags were not parsed")
	}
	if c1.Port != 5555 {
		t.Fatalf("first session port = %d, want 5555", c1.Port)
	}
	if c2.Port != 8080 {
		t.Fatalf("second session port = %d, want default 8080", c2.Port)
	}
}

func TestPlanRejectsStatefulDecoder(t *testing.T) {
	_, err := NewPlan(&planTestConfig{}, Config{
		FileDecoders: map[string]FileDecoder{
			".stateful": &statefulOnlyDecoder{},
		},
	})
	if err == nil {
		t.Fatal("expected error for a stateful decoder without FileDecoderCloner")
	}
	if !strings.Contains(err.Error(), "FileDecoderCloner") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlanDecoderStateIsSessionLocal(t *testing.T) {
	var clones, inits int32
	dec := &countingDecoder{clones: &clones, inits: &inits}

	plan, err := NewPlan(&planFileConfig{}, Config{
		FileDecoders: map[string]FileDecoder{".kv": dec},
	})
	if err != nil {
		t.Fatal(err)
	}

	load := func(fsys fs.FS) (planFileConfig, error) {
		var cfg planFileConfig
		l, err := plan.LoaderFor(&cfg, Config{
			FileSystem: fsys,
			Files:      []string{"config.kv"},
			Envs:       []string{},
			Args:       []string{},
		})
		if err != nil {
			return cfg, err
		}
		return cfg, l.Load()
	}

	fsys1 := fstest.MapFS{"config.kv": {Data: []byte("name=one\nnum=1")}}
	fsys2 := fstest.MapFS{"config.kv": {Data: []byte("name=two\nnum=2")}}

	c1, err := load(fsys1)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := load(fsys2)
	if err != nil {
		t.Fatal(err)
	}

	if c1.Name != "one" || c1.Num != 1 {
		t.Fatalf("unexpected first config: %+v", c1)
	}
	if c2.Name != "two" || c2.Num != 2 {
		t.Fatalf("unexpected second config: %+v", c2)
	}
	if got := atomic.LoadInt32(&clones); got != 2 {
		t.Fatalf("decoder was cloned %d times, want 2 (one per session)", got)
	}
	if got := atomic.LoadInt32(&inits); got != 2 {
		t.Fatalf("decoder was initialized %d times, want 2 (one per session)", got)
	}
}

func TestPlanSessionCannotOverrideNames(t *testing.T) {
	plan, err := NewPlan(&planTestConfig{}, Config{EnvPrefix: "APP"})
	if err != nil {
		t.Fatal(err)
	}

	var cfg planTestConfig
	_, err = plan.LoaderFor(&cfg, Config{EnvPrefix: "OTHER"})
	if err == nil {
		t.Fatal("expected error when overriding a name-affecting option")
	}
	if !strings.Contains(err.Error(), "NewPlan") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlanBoundToType(t *testing.T) {
	plan, err := NewPlan((*planTestConfig)(nil), Config{})
	if err != nil {
		t.Fatal(err)
	}

	var other planFileConfig
	if _, err := plan.LoaderFor(&other, Config{}); err == nil {
		t.Fatal("expected error for a destination of another type")
	}

	var nilCfg *planTestConfig
	if _, err := plan.LoaderFor(nilCfg, Config{}); err == nil {
		t.Fatal("expected error for a nil destination")
	}
}

func TestPlanValidatesFlagNamesOnce(t *testing.T) {
	type dupFlags struct {
		A string `flag:"same"`
		B string `flag:"same"`
	}
	if _, err := NewPlan(&dupFlags{}, Config{}); err == nil {
		t.Fatal("expected duplicate flag error from NewPlan")
	}

	// Duplicate flags are fine when flags are skipped entirely.
	plan, err := NewPlan(&dupFlags{}, Config{SkipFlags: true})
	if err != nil {
		t.Fatal(err)
	}
	var cfg dupFlags
	if _, err := plan.LoaderFor(&cfg, Config{SkipFiles: true, Envs: []string{}, Args: []string{}}); err != nil {
		t.Fatal(err)
	}
}

type planFileConfig struct {
	Name string `default:"anon"`
	Num  int    `default:"1"`
}

// statefulOnlyDecoder keeps mutable state (implements Init) but cannot be
// cloned, so NewPlan must reject it.
type statefulOnlyDecoder struct {
	fsys fs.FS
}

func (d *statefulOnlyDecoder) Format() string { return "stateful" }
func (d *statefulOnlyDecoder) Init(fsys fs.FS) {
	d.fsys = fsys
}
func (d *statefulOnlyDecoder) DecodeFile(string) (map[string]any, error) {
	return map[string]any{}, nil
}

// countingDecoder is a stateful decoder that can be cloned; it records how
// many times it was cloned and initialized, and reads key=value files from
// the filesystem of its session.
type countingDecoder struct {
	fsys   fs.FS
	clones *int32
	inits  *int32
}

func (d *countingDecoder) Format() string { return "kv" }

func (d *countingDecoder) Init(fsys fs.FS) {
	d.fsys = fsys
	atomic.AddInt32(d.inits, 1)
}

func (d *countingDecoder) CloneFileDecoder() FileDecoder {
	atomic.AddInt32(d.clones, 1)
	return &countingDecoder{clones: d.clones, inits: d.inits}
}

func (d *countingDecoder) DecodeFile(filename string) (map[string]any, error) {
	data, err := fs.ReadFile(d.fsys, filename)
	if err != nil {
		return nil, err
	}
	res := map[string]any{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			res[key] = value
		}
	}
	return res, nil
}
