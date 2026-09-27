package redcon

import (
	"strings"
	"testing"
)

func makeCmd(args ...string) Command {
	cmd := Command{Args: make([][]byte, len(args))}
	for i, arg := range args {
		cmd.Args[i] = []byte(arg)
	}
	return cmd
}

func keysToStrings(keys [][]byte) []string {
	strs := make([]string, len(keys))
	for i, key := range keys {
		strs[i] = string(key)
	}
	return strs
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustRegister(t *testing.T, reg *CommandRegistry, spec CommandSpec) {
	t.Helper()
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register %s: %v", spec.Name, err)
	}
}

func expectKeys(t *testing.T, reg *CommandRegistry, cmd Command, want []string) {
	t.Helper()
	keys, err := reg.Keys(cmd)
	if err != nil {
		t.Fatalf("keys for %v: %v", cmd.Args, err)
	}
	if !equalStrings(keysToStrings(keys), want) {
		t.Fatalf("keys for %v: got %v, want %v",
			cmd.Args, keysToStrings(keys), want)
	}
}

func expectKeysError(t *testing.T, reg *CommandRegistry, cmd Command) {
	t.Helper()
	if _, err := reg.Keys(cmd); err == nil {
		t.Fatalf("expected error for %v", cmd.Args)
	}
}

func TestRegistryMGetStyle(t *testing.T) {
	reg := NewCommandRegistry()
	mustRegister(t, reg, CommandSpec{
		Name:     "mget",
		Arity:    -2,
		ReadOnly: true,
		KeySpec:  AllKeys(),
	})
	expectKeys(t, reg, makeCmd("MGET", "a", "b", "c"), []string{"a", "b", "c"})
	expectKeys(t, reg, makeCmd("mget", "only"), []string{"only"})
	// arity violation: MGET requires at least one key
	expectKeysError(t, reg, makeCmd("mget"))
	// read-only metadata
	spec, ok := reg.Lookup("MGET")
	if !ok || !spec.ReadOnly {
		t.Fatalf("expected mget to be registered read-only")
	}
}

func TestRegistryMSetStyle(t *testing.T) {
	reg := NewCommandRegistry()
	mustRegister(t, reg, CommandSpec{
		Name:    "mset",
		Arity:   -3,
		KeySpec: OddKeys(),
	})
	expectKeys(t, reg,
		makeCmd("mset", "k1", "v1", "k2", "v2", "k3", "v3"),
		[]string{"k1", "k2", "k3"})
	// dangling value without a key
	expectKeysError(t, reg, makeCmd("mset", "k1", "v1", "k2"))
	spec, ok := reg.Lookup("mset")
	if !ok || spec.ReadOnly {
		t.Fatalf("expected mset to be registered as a write command")
	}
}

func TestRegistryNumKeysStyle(t *testing.T) {
	reg := NewCommandRegistry()
	// Custom command: XMERGE numkeys key... dest
	mustRegister(t, reg, CommandSpec{
		Name:    "xmerge",
		Aliases: []string{"xcombine"},
		Arity:   -3,
		KeySpec: NumKeys(0),
	})
	expectKeys(t, reg,
		makeCmd("xmerge", "2", "src1", "src2", "dst"),
		[]string{"src1", "src2"})
	// alias resolves to the same spec
	expectKeys(t, reg,
		makeCmd("XCOMBINE", "1", "src", "dst"), []string{"src"})
	// zero keys is a valid declaration
	expectKeys(t, reg, makeCmd("xmerge", "0", "dst"), nil)

	// negative numkeys
	expectKeysError(t, reg, makeCmd("xmerge", "-1", "src", "dst"))
	// non-numeric numkeys
	expectKeysError(t, reg, makeCmd("xmerge", "two", "src", "dst"))
	// overflowing numkeys
	expectKeysError(t, reg,
		makeCmd("xmerge", "99999999999999999999999999", "src", "dst"))
	// numkeys exceeding the remaining arguments
	expectKeysError(t, reg, makeCmd("xmerge", "5", "src", "dst"))
	// numkeys argument missing entirely
	expectKeysError(t, reg, makeCmd("xmerge"))
}

func TestRegistryRangeKeys(t *testing.T) {
	reg := NewCommandRegistry()
	// GET-style: exactly one key at a fixed position.
	mustRegister(t, reg, CommandSpec{
		Name:     "get",
		Arity:    2,
		ReadOnly: true,
		KeySpec:  RangeKeys(0, 0, 1),
	})
	expectKeys(t, reg, makeCmd("get", "k"), []string{"k"})
	expectKeysError(t, reg, makeCmd("get"))
	expectKeysError(t, reg, makeCmd("get", "k", "extra"))

	// Custom command: pairs of key/value like MSET via a range spec.
	mustRegister(t, reg, CommandSpec{
		Name:    "kvpairs",
		Arity:   -3,
		KeySpec: RangeKeys(0, -1, 2),
	})
	expectKeys(t, reg,
		makeCmd("kvpairs", "k1", "v1", "k2", "v2"), []string{"k1", "k2"})
	// declared range runs past the actual arguments
	mustRegister(t, reg, CommandSpec{
		Name:    "fixed3",
		Arity:   -2,
		KeySpec: RangeKeys(0, 2, 1),
	})
	expectKeysError(t, reg, makeCmd("fixed3", "a", "b"))
}

func TestRegistryMetadata(t *testing.T) {
	reg := NewCommandRegistry()
	mustRegister(t, reg, CommandSpec{
		Name:     "get",
		Aliases:  []string{"fetch"},
		Arity:    2,
		ReadOnly: true,
		KeySpec:  RangeKeys(0, 0, 1),
	})
	mustRegister(t, reg, CommandSpec{
		Name:    "set",
		Arity:   -3,
		KeySpec: RangeKeys(0, 0, 1),
	})
	spec, ok := reg.Lookup("FETCH")
	if !ok || spec.Name != "get" || !spec.ReadOnly {
		t.Fatalf("alias lookup failed: %+v", spec)
	}
	if _, ok := reg.Lookup("nosuch"); ok {
		t.Fatalf("unexpected lookup hit")
	}
	specs := reg.Specs()
	if len(specs) != 2 || specs[0].Name != "get" || specs[1].Name != "set" {
		t.Fatalf("unexpected specs listing: %+v", specs)
	}
	// duplicate registration, including through aliases
	if err := reg.Register(CommandSpec{Name: "GET", Arity: 2}); err == nil {
		t.Fatalf("expected duplicate name error")
	}
	if err := reg.Register(CommandSpec{Name: "new", Aliases: []string{"fetch"}, Arity: 1}); err == nil {
		t.Fatalf("expected duplicate alias error")
	}
	if err := reg.Register(CommandSpec{Name: "bad", Arity: 0}); err == nil {
		t.Fatalf("expected zero arity error")
	}
}

type fakeConn struct {
	Conn
	errs []string
}

func (c *fakeConn) WriteError(msg string) { c.errs = append(c.errs, msg) }

func TestRegistryWrap(t *testing.T) {
	reg := NewCommandRegistry()
	mustRegister(t, reg, CommandSpec{
		Name:     "mget",
		Arity:    -2,
		ReadOnly: true,
		KeySpec:  AllKeys(),
	})
	mustRegister(t, reg, CommandSpec{
		Name:    "xmerge",
		Arity:   -3,
		KeySpec: NumKeys(0),
	})
	var handled []string
	handler := reg.Wrap(func(conn Conn, cmd Command) {
		handled = append(handled, string(cmd.Args[0]))
	})

	dispatch := func(args ...string) *fakeConn {
		conn := &fakeConn{}
		handler.ServeRESP(conn, makeCmd(args...))
		return conn
	}

	// valid command reaches the business handler
	if conn := dispatch("mget", "a"); len(conn.errs) != 0 {
		t.Fatalf("unexpected error: %v", conn.errs)
	}
	// unknown command rejected
	if conn := dispatch("nosuch", "a"); len(conn.errs) != 1 {
		t.Fatalf("expected rejection, got %v", conn.errs)
	}
	// arity violation rejected
	if conn := dispatch("mget"); len(conn.errs) != 1 ||
		!strings.Contains(conn.errs[0], "wrong number of arguments") {
		t.Fatalf("expected arity rejection, got %v", conn.errs)
	}
	// out-of-range numkeys rejected before the handler
	if conn := dispatch("xmerge", "9", "a"); len(conn.errs) != 1 {
		t.Fatalf("expected numkeys rejection, got %v", conn.errs)
	}
	if !equalStrings(handled, []string{"mget"}) {
		t.Fatalf("handler invoked for rejected commands: %v", handled)
	}
}
