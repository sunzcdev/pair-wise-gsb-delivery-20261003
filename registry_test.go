package redcon

import (
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func makeCommand(args ...string) Command {
	cmd := Command{}
	for _, arg := range args {
		cmd.Args = append(cmd.Args, []byte(arg))
	}
	return cmd
}

func keysToStrings(keys [][]byte) []string {
	var out []string
	for _, key := range keys {
		out = append(out, string(key))
	}
	return out
}

// testRegistry builds a registry with an MGET-style command, an
// MSET-style command, and a custom numkeys command.
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	specs := []CommandSpec{
		{
			Name:     "MGET",
			Aliases:  []string{"MGETRO"},
			Arity:    -2,
			ReadOnly: true,
			Keys:     KeySpec{Kind: KeySpecRange, First: 1, Last: -1, Step: 1},
		},
		{
			Name:  "MSET",
			Arity: -4,
			Keys:  KeySpec{Kind: KeySpecRange, First: 1, Last: -1, Step: 2},
		},
		{
			// Custom command: XROUTE numkeys key... payload...
			Name:  "XROUTE",
			Arity: -3,
			Keys:  KeySpec{Kind: KeySpecNumKeys, NumKeysAt: 1, Step: 1},
		},
	}
	for _, spec := range specs {
		if err := reg.Register(spec); err != nil {
			t.Fatalf("register %s: %v", spec.Name, err)
		}
	}
	return reg
}

func TestRegistryMGetStyle(t *testing.T) {
	reg := testRegistry(t)
	keys, err := reg.Keys(makeCommand("MGET", "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(keysToStrings(keys), want) {
		t.Fatalf("got %v, want %v", keysToStrings(keys), want)
	}
	if !reg.IsReadOnly("mget") {
		t.Fatal("MGET should be read-only")
	}
	if err := reg.Validate(makeCommand("MGET")); err == nil {
		t.Fatal("expected arity error for bare MGET")
	}
}

func TestRegistryMSetStyle(t *testing.T) {
	reg := testRegistry(t)
	keys, err := reg.Keys(makeCommand("MSET", "k1", "v1", "k2", "v2"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"k1", "k2"}; !reflect.DeepEqual(keysToStrings(keys), want) {
		t.Fatalf("got %v, want %v", keysToStrings(keys), want)
	}
	if reg.IsReadOnly("mset") {
		t.Fatal("MSET should not be read-only")
	}
	// Dangling value: the key group is incomplete.
	if _, err := reg.Keys(makeCommand("MSET", "k1", "v1", "k2")); err == nil {
		t.Fatal("expected error for incomplete key/value pair")
	}
}

func TestRegistryNumKeysStyle(t *testing.T) {
	reg := testRegistry(t)
	keys, err := reg.Keys(makeCommand("XROUTE", "2", "k1", "k2", "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"k1", "k2"}; !reflect.DeepEqual(keysToStrings(keys), want) {
		t.Fatalf("got %v, want %v", keysToStrings(keys), want)
	}

	rejected := [][]string{
		{"XROUTE", "-1", "k1"},                  // negative numkeys
		{"XROUTE", "9223372036854775808", "k1"}, // overflows int64
		{"XROUTE", "5", "k1", "k2"},             // declares more keys than provided
		{"XROUTE", "notanumber", "k1"},          // not an integer
		{"XROUTE", "1"},                         // arity violation
	}
	for _, args := range rejected {
		if _, err := reg.Keys(makeCommand(args...)); err == nil {
			t.Fatalf("expected %v to be rejected", args)
		}
	}
}

func TestRegistryAliasAndMetadata(t *testing.T) {
	reg := testRegistry(t)
	spec, ok := reg.Lookup("mgetro")
	if !ok {
		t.Fatal("alias MGETRO not registered")
	}
	if spec.Name != "mget" {
		t.Fatalf("expected canonical name mget, got %q", spec.Name)
	}
	keys, err := reg.Keys(makeCommand("MGETRO", "x", "y"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"x", "y"}; !reflect.DeepEqual(keysToStrings(keys), want) {
		t.Fatalf("got %v, want %v", keysToStrings(keys), want)
	}
	if !reg.IsReadOnly("MGETRO") {
		t.Fatal("alias should resolve read-only metadata")
	}
	if _, ok := reg.Lookup("nope"); ok {
		t.Fatal("unexpected lookup hit")
	}
}

func TestRegistryRegisterValidation(t *testing.T) {
	reg := NewRegistry()
	bad := []CommandSpec{
		{Name: "", Arity: 1},
		{Name: "A", Arity: 0},
		{Name: "B", Arity: 1, Keys: KeySpec{Kind: KeySpecRange, First: 0, Last: -1, Step: 1}},
		{Name: "C", Arity: 1, Keys: KeySpec{Kind: KeySpecRange, First: 2, Last: 1, Step: 1}},
		{Name: "D", Arity: 1, Keys: KeySpec{Kind: KeySpecRange, First: 1, Last: -1, Step: 0}},
		{Name: "E", Arity: 1, Keys: KeySpec{Kind: KeySpecNumKeys, NumKeysAt: 0, Step: 1}},
		{Name: "F", Arity: 1, Keys: KeySpec{Kind: KeySpecKind(99)}},
	}
	for _, spec := range bad {
		if err := reg.Register(spec); err == nil {
			t.Fatalf("expected %+v to be rejected", spec)
		}
	}
	if err := reg.Register(CommandSpec{Name: "G", Arity: 1, Aliases: []string{"G2"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(CommandSpec{Name: "g", Arity: 1}); err == nil {
		t.Fatal("expected duplicate name to be rejected")
	}
	if err := reg.Register(CommandSpec{Name: "H", Arity: 1, Aliases: []string{"g2"}}); err == nil {
		t.Fatal("expected duplicate alias to be rejected")
	}
}

// TestRegistryWrapRejectsBeforeHandler runs a real server behind the
// registry wrapper and verifies malformed commands are answered with an
// error without ever reaching the business handler.
func TestRegistryWrapRejectsBeforeHandler(t *testing.T) {
	reg := testRegistry(t)
	var handled []string
	mux := NewServeMux()
	mux.HandleFunc("xroute", func(conn Conn, cmd Command) {
		handled = append(handled, string(cmd.Args[0]))
		conn.WriteString("OK")
	})
	s := NewServer("127.0.0.1:0", reg.Wrap(mux).ServeRESP, nil, nil)
	signal := make(chan error)
	go s.ListenServeAndSignal(signal)
	defer s.Close()
	if err := <-signal; err != nil {
		t.Fatal(err)
	}
	addr := s.Addr().String()

	send := func(args ...string) string {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		wr := NewWriter(conn)
		wr.WriteArray(len(args))
		for _, arg := range args {
			wr.WriteBulkString(arg)
		}
		if err := wr.Flush(); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 256)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return string(buf[:n])
	}

	if resp := send("XROUTE", "2", "k1", "k2", "p"); !strings.Contains(resp, "OK") {
		t.Fatalf("valid command failed: %q", resp)
	}
	if len(handled) != 1 {
		t.Fatalf("handler should have run once, ran %d times", len(handled))
	}
	for _, args := range [][]string{
		{"XROUTE", "-1", "k1"},
		{"XROUTE", "99999999999999999999999", "k1"},
		{"XROUTE", "7", "k1"},
	} {
		resp := send(args...)
		if !strings.HasPrefix(resp, "-ERR") {
			t.Fatalf("expected error for %v, got %q", args, resp)
		}
	}
	if len(handled) != 1 {
		t.Fatalf("rejected commands reached the handler: %d invocations", len(handled))
	}
}
