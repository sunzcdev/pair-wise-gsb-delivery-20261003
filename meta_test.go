package toml

import (
	"strings"
	"testing"
)

func metaLookup(t *testing.T, m *MetaData, key ...string) Origin {
	t.Helper()
	o, ok := m.Lookup(key...)
	if !ok {
		t.Fatalf("no origin for key %v", key)
	}
	return o
}

func TestMetaBasic(t *testing.T) {
	doc := "version = 2\nname = \"go-toml\"\n\n[server]\naddr = \"127.0.0.1\"\nport = 8080\n"
	var cfg struct {
		Version int
		Name    string
		Server  struct {
			Addr string
			Port int
		}
	}
	m, err := UnmarshalWithMeta([]byte(doc), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != "127.0.0.1" {
		t.Fatalf("unexpected addr: %v", cfg.Server.Addr)
	}

	o := metaLookup(t, m, "version")
	if got := doc[o.Range.Start:o.Range.End]; got != "2" {
		t.Errorf("version range = %q, want %q", got, "2")
	}
	if o.Path.String() != "Version" {
		t.Errorf("version path = %q, want Version", o.Path.String())
	}

	o = metaLookup(t, m, "server", "addr")
	if got := doc[o.Range.Start:o.Range.End]; got != `"127.0.0.1"` {
		t.Errorf("addr range = %q, want %q", got, `"127.0.0.1"`)
	}
	if o.Path.String() != "Server.Addr" {
		t.Errorf("addr path = %q, want Server.Addr", o.Path.String())
	}

	o = metaLookup(t, m, "server", "port")
	if got := doc[o.Range.Start:o.Range.End]; got != "8080" {
		t.Errorf("port range = %q, want %q", got, "8080")
	}
	if o.Path.String() != "Server.Port" {
		t.Errorf("port path = %q, want Server.Port", o.Path.String())
	}

	// Lookup by Go field path.
	of, ok := m.LookupField("Server", "Addr")
	if !ok {
		t.Fatalf("no origin for field Server.Addr")
	}
	if got := doc[of.Range.Start:of.Range.End]; got != `"127.0.0.1"` {
		t.Errorf("field addr range = %q", got)
	}

	if _, ok := m.Lookup("server", "missing"); ok {
		t.Errorf("unexpected origin for missing field")
	}
	if _, ok := m.LookupField("Server", "Missing"); ok {
		t.Errorf("unexpected origin for missing Go field")
	}
}

func TestMetaArrayTables(t *testing.T) {
	doc := "[[servers]]\naddr = \"a\"\n\n[[servers]]\naddr = \"b\"\n"
	var cfg struct {
		Servers []struct {
			Addr string
		}
	}
	m, err := UnmarshalWithMeta([]byte(doc), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	all := m.LookupAll("servers", "addr")
	if len(all) != 2 {
		t.Fatalf("expected 2 origins, got %d", len(all))
	}
	allf := m.LookupFieldAll("Servers", "Addr")
	if len(allf) != 2 {
		t.Fatalf("expected 2 field origins, got %d", len(allf))
	}
	if all[0].Path.String() != "Servers[0].Addr" {
		t.Errorf("path[0] = %q, want Servers[0].Addr", all[0].Path.String())
	}
	if all[1].Path.String() != "Servers[1].Addr" {
		t.Errorf("path[1] = %q, want Servers[1].Addr", all[1].Path.String())
	}
	if got := doc[all[0].Range.Start:all[0].Range.End]; got != `"a"` {
		t.Errorf("range[0] = %q, want %q", got, `"a"`)
	}
	if got := doc[all[1].Range.Start:all[1].Range.End]; got != `"b"` {
		t.Errorf("range[1] = %q, want %q", got, `"b"`)
	}
}

func TestMetaUnifiedPaths(t *testing.T) {
	// The same value expressed as a table, a dotted key, and an inline
	// table must produce the same path.
	for _, doc := range []string{
		"[a.b]\nc = 1\n",
		"a.b.c = 1\n",
		"a = {b = {c = 1}}\n",
	} {
		var v map[string]interface{}
		m, err := UnmarshalWithMeta([]byte(doc), &v)
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		o := metaLookup(t, m, "a", "b", "c")
		if got := doc[o.Range.Start:o.Range.End]; got != "1" {
			t.Errorf("%q: range = %q, want %q", doc, got, "1")
		}
		if o.Path.String() != "a.b.c" {
			t.Errorf("%q: path = %q, want a.b.c", doc, o.Path.String())
		}
	}
}

func TestMetaQuotedKeys(t *testing.T) {
	doc := "a.\"b.c\" = 1\n\"x.y\".z = 2\n"
	var v map[string]interface{}
	m, err := UnmarshalWithMeta([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "a", "b.c")
	if o.Path.String() != `a."b.c"` {
		t.Errorf("path = %q, want %q", o.Path.String(), `a."b.c"`)
	}
	o = metaLookup(t, m, "x.y", "z")
	if o.Path.String() != `"x.y".z` {
		t.Errorf("path = %q, want %q", o.Path.String(), `"x.y".z`)
	}
}

func TestMetaStructMapComparable(t *testing.T) {
	doc := "[server]\naddr = \"x\"\nport = 1\n"
	var s struct {
		Server struct {
			Addr string
			Port int
		}
	}
	ms, err := UnmarshalWithMeta([]byte(doc), &s)
	if err != nil {
		t.Fatal(err)
	}
	var mp map[string]interface{}
	mm, err := UnmarshalWithMeta([]byte(doc), &mp)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range [][]string{{"server", "addr"}, {"server", "port"}} {
		os := metaLookup(t, ms, key...)
		om := metaLookup(t, mm, key...)
		if os.Range != om.Range {
			t.Errorf("key %v: struct range %v != map range %v", key, os.Range, om.Range)
		}
		if strings.Join(os.Key, ".") != strings.Join(om.Key, ".") {
			t.Errorf("key %v: keys differ: %v vs %v", key, os.Key, om.Key)
		}
	}
}

type metaRawMessage struct{ raw []byte }

func (m *metaRawMessage) UnmarshalTOML(b []byte) error {
	m.raw = append(m.raw[:0], b...)
	return nil
}

func TestMetaUnmarshaler(t *testing.T) {
	doc := "[whole]\na = 1\nb = 2\n"
	var cfg struct {
		Whole metaRawMessage `toml:"whole"`
	}
	dec := NewDecoder(strings.NewReader(doc))
	dec.EnableUnmarshalerInterface()
	m := &MetaData{}
	dec.RecordOrigins(m)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "whole")
	if !o.Unmarshaled {
		t.Errorf("expected Unmarshaled origin")
	}
	if got := doc[o.Range.Start:o.Range.End]; !strings.Contains(got, "[whole]") || !strings.Contains(got, "b = 2") {
		t.Errorf("range = %q, want the whole table", got)
	}
}

func TestMetaUnmarshalerArrayTable(t *testing.T) {
	doc := "[[whole]]\na = 1\n\n[[whole]]\na = 2\n"
	var cfg struct {
		Whole []metaRawMessage `toml:"whole"`
	}
	dec := NewDecoder(strings.NewReader(doc))
	dec.EnableUnmarshalerInterface()
	m := &MetaData{}
	dec.RecordOrigins(m)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	all := m.LookupAll("whole")
	if len(all) != 2 {
		t.Fatalf("expected 2 origins, got %d", len(all))
	}
	if all[0].Path.String() != "Whole[0]" {
		t.Errorf("path[0] = %q, want Whole[0]", all[0].Path.String())
	}
	if all[1].Path.String() != "Whole[1]" {
		t.Errorf("path[1] = %q, want Whole[1]", all[1].Path.String())
	}
	if got := doc[all[0].Range.Start:all[0].Range.End]; !strings.HasPrefix(got, "[[whole]]") {
		t.Errorf("range[0] = %q, want it to start with [[whole]]", got)
	}
	if !all[0].Unmarshaled || !all[1].Unmarshaled {
		t.Errorf("expected Unmarshaled origins")
	}
}

func TestMetaUnmarshalerSpacedHeader(t *testing.T) {
	doc := "[ whole ]\na = 1\n"
	var cfg struct {
		Whole metaRawMessage `toml:"whole"`
	}
	dec := NewDecoder(strings.NewReader(doc))
	dec.EnableUnmarshalerInterface()
	m := &MetaData{}
	dec.RecordOrigins(m)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "whole")
	if got := doc[o.Range.Start:o.Range.End]; !strings.HasPrefix(got, "[ whole ]") {
		t.Errorf("range = %q, want it to start with [ whole ]", got)
	}
}

func TestMetaNestedInlineTables(t *testing.T) {
	doc := "point = {x = 1, y = {z = 2}}\n"
	var v map[string]interface{}
	m, err := UnmarshalWithMeta([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "point", "x")
	if got := doc[o.Range.Start:o.Range.End]; got != "1" {
		t.Errorf("point.x range = %q, want %q", got, "1")
	}
	o = metaLookup(t, m, "point", "y", "z")
	if got := doc[o.Range.Start:o.Range.End]; got != "2" {
		t.Errorf("point.y.z range = %q, want %q", got, "2")
	}
	if o.Path.String() != "point.y.z" {
		t.Errorf("path = %q, want point.y.z", o.Path.String())
	}
	// The inline tables themselves are containers: no origin.
	if _, ok := m.Lookup("point"); ok {
		t.Errorf("unexpected origin for the inline table itself")
	}
	if _, ok := m.Lookup("point", "y"); ok {
		t.Errorf("unexpected origin for the nested inline table itself")
	}
}

func TestMetaArrayOfInlineTables(t *testing.T) {
	doc := "points = [{x = 1}, {x = 2}]\n"
	var v map[string]interface{}
	m, err := UnmarshalWithMeta([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}
	// The whole array is a single value; its elements are not fields.
	o := metaLookup(t, m, "points")
	if got := doc[o.Range.Start:o.Range.End]; got != "[{x = 1}, {x = 2}]" {
		t.Errorf("points range = %q, want the whole array", got)
	}
	if _, ok := m.Lookup("points", "x"); ok {
		t.Errorf("unexpected origin for array element key")
	}
	if len(m.Origins()) != 1 {
		t.Errorf("expected 1 origin, got %d", len(m.Origins()))
	}
}

func TestMetaArrayOfTablesInMap(t *testing.T) {
	doc := "[[servers]]\naddr = \"a\"\n\n[[servers]]\naddr = \"b\"\n"
	var v map[string]interface{}
	m, err := UnmarshalWithMeta([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}
	all := m.LookupAll("servers", "addr")
	if len(all) != 2 {
		t.Fatalf("expected 2 origins, got %d", len(all))
	}
	if all[0].Path.String() != "servers[0].addr" {
		t.Errorf("path[0] = %q, want servers[0].addr", all[0].Path.String())
	}
	if all[1].Path.String() != "servers[1].addr" {
		t.Errorf("path[1] = %q, want servers[1].addr", all[1].Path.String())
	}
}

func TestMetaNestedArrayTables(t *testing.T) {
	doc := "[[a]]\n[[a.b]]\nc = 1\n[[a.b]]\nc = 2\n[[a]]\n[[a.b]]\nc = 3\n"
	var v map[string]interface{}
	m, err := UnmarshalWithMeta([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}
	all := m.LookupAll("a", "b", "c")
	if len(all) != 3 {
		t.Fatalf("expected 3 origins, got %d", len(all))
	}
	want := []string{"a[0].b[0].c", "a[0].b[1].c", "a[1].b[0].c"}
	for i, w := range want {
		if all[i].Path.String() != w {
			t.Errorf("path[%d] = %q, want %q", i, all[i].Path.String(), w)
		}
	}
}

func TestMetaInlineTableInStruct(t *testing.T) {
	doc := "point = {x = 1, y = 2}\n"
	var cfg struct {
		Point struct {
			X int
			Y int
		}
	}
	m, err := UnmarshalWithMeta([]byte(doc), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "point", "x")
	if o.Path.String() != "Point.X" {
		t.Errorf("path = %q, want Point.X", o.Path.String())
	}
	if got := doc[o.Range.Start:o.Range.End]; got != "1" {
		t.Errorf("range = %q, want %q", got, "1")
	}
	o = metaLookup(t, m, "point", "y")
	if o.Path.String() != "Point.Y" {
		t.Errorf("path = %q, want Point.Y", o.Path.String())
	}
}

func TestMetaDottedKeyThroughArrayTable(t *testing.T) {
	doc := "[[a]]\nb.c = 1\n[[a]]\nb.c = 2\n"
	var v map[string]interface{}
	m, err := UnmarshalWithMeta([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}
	all := m.LookupAll("a", "b", "c")
	if len(all) != 2 {
		t.Fatalf("expected 2 origins, got %d", len(all))
	}
	if all[0].Path.String() != "a[0].b.c" {
		t.Errorf("path[0] = %q, want a[0].b.c", all[0].Path.String())
	}
	if all[1].Path.String() != "a[1].b.c" {
		t.Errorf("path[1] = %q, want a[1].b.c", all[1].Path.String())
	}
}

func TestMetaUnmarshalNoMeta(t *testing.T) {
	// The plain Unmarshal path does not record anything and must not be
	// affected by the metadata machinery.
	doc := "a = 1\n[b]\nc = 2\n"
	var v map[string]interface{}
	if err := Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	if v["a"] != int64(1) {
		t.Errorf("unexpected value: %v", v)
	}
}

func TestMetaInterfaceRoot(t *testing.T) {
	doc := "a = 1\n[b]\nc = 2\n"
	var v interface{}
	m, err := UnmarshalWithMeta([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "a")
	if got := doc[o.Range.Start:o.Range.End]; got != "1" {
		t.Errorf("a range = %q, want %q", got, "1")
	}
	o = metaLookup(t, m, "b", "c")
	if got := doc[o.Range.Start:o.Range.End]; got != "2" {
		t.Errorf("b.c range = %q, want %q", got, "2")
	}
}

func TestMetaDateTime(t *testing.T) {
	doc := "when = 1979-05-27T07:32:00Z\nday = 1979-05-27\n"
	var v map[string]interface{}
	m, err := UnmarshalWithMeta([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "when")
	if got := doc[o.Range.Start:o.Range.End]; got != "1979-05-27T07:32:00Z" {
		t.Errorf("when range = %q", got)
	}
	o = metaLookup(t, m, "day")
	if got := doc[o.Range.Start:o.Range.End]; got != "1979-05-27" {
		t.Errorf("day range = %q", got)
	}
}

func TestMetaStrictMode(t *testing.T) {
	doc := "a = 1\nunknown = 2\n"
	var cfg struct {
		A int
	}
	dec := NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	m := &MetaData{}
	dec.RecordOrigins(m)
	err := dec.Decode(&cfg)
	if err == nil {
		t.Fatal("expected a strict mode error")
	}
	// The known field is still recorded.
	o := metaLookup(t, m, "a")
	if got := doc[o.Range.Start:o.Range.End]; got != "1" {
		t.Errorf("a range = %q, want %q", got, "1")
	}
}

func TestMetaDecoderReuse(t *testing.T) {
	dec := NewDecoder(strings.NewReader("a = 1\n"))
	m := &MetaData{}
	dec.RecordOrigins(m)
	var v1 map[string]interface{}
	if err := dec.Decode(&v1); err != nil {
		t.Fatal(err)
	}
	if len(m.Origins()) != 1 {
		t.Fatalf("expected 1 origin, got %d", len(m.Origins()))
	}
	// A second decode with a fresh document resets the metadata.
	dec = NewDecoder(strings.NewReader("b = 2\nc = 3\n"))
	dec.RecordOrigins(m)
	var v2 map[string]interface{}
	if err := dec.Decode(&v2); err != nil {
		t.Fatal(err)
	}
	if len(m.Origins()) != 2 {
		t.Fatalf("expected 2 origins after reuse, got %d", len(m.Origins()))
	}
	if _, ok := m.Lookup("b"); !ok {
		t.Errorf("missing origin for b")
	}
}

func TestMetaUnmarshalWithMetaError(t *testing.T) {
	m, err := UnmarshalWithMeta([]byte("a = \n"), nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if m != nil {
		t.Errorf("expected nil metadata on error, got %v", m)
	}
}

func TestMetaEmptyDocument(t *testing.T) {
	var v map[string]interface{}
	m, err := UnmarshalWithMeta([]byte(""), &v)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Origins()) != 0 {
		t.Errorf("expected no origins, got %d", len(m.Origins()))
	}
}

func TestMetaRootUnmarshaler(t *testing.T) {
	doc := "a = 1\n[b]\nc = 2\n"
	var v metaRawMessage
	dec := NewDecoder(strings.NewReader(doc))
	dec.EnableUnmarshalerInterface()
	m := &MetaData{}
	dec.RecordOrigins(m)
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if len(m.Origins()) != 1 {
		t.Fatalf("expected 1 origin, got %d", len(m.Origins()))
	}
	o := m.Origins()[0]
	if !o.Unmarshaled {
		t.Errorf("expected Unmarshaled origin")
	}
	if len(o.Path) != 0 {
		t.Errorf("expected empty path, got %v", o.Path)
	}
	if got := doc[o.Range.Start:o.Range.End]; !strings.Contains(got, "c = 2") {
		t.Errorf("range = %q, want the whole document", got)
	}
}

func TestMetaValueUnmarshaler(t *testing.T) {
	doc := "[server]\naddr = \"x\"\n"
	var cfg struct {
		Server struct {
			Addr metaRawMessage
		}
	}
	dec := NewDecoder(strings.NewReader(doc))
	dec.EnableUnmarshalerInterface()
	m := &MetaData{}
	dec.RecordOrigins(m)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "server", "addr")
	if !o.Unmarshaled {
		t.Errorf("expected Unmarshaled origin")
	}
	if got := doc[o.Range.Start:o.Range.End]; got != `"x"` {
		t.Errorf("range = %q, want %q", got, `"x"`)
	}
	if o.Path.String() != "Server.Addr" {
		t.Errorf("path = %q, want Server.Addr", o.Path.String())
	}
}

func TestMetaPrefixUnmarshaler(t *testing.T) {
	// The "a" field implements Unmarshaler: it consumes the value of the
	// a.b key-value, so the origin is recorded for "a", not "a.b".
	doc := "a.b = 1\n"
	var cfg struct {
		A metaRawMessage `toml:"a"`
	}
	dec := NewDecoder(strings.NewReader(doc))
	dec.EnableUnmarshalerInterface()
	m := &MetaData{}
	dec.RecordOrigins(m)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	o := metaLookup(t, m, "a")
	if !o.Unmarshaled {
		t.Errorf("expected Unmarshaled origin")
	}
	if got := doc[o.Range.Start:o.Range.End]; got != "1" {
		t.Errorf("range = %q, want %q", got, "1")
	}
	if _, ok := m.Lookup("a", "b"); ok {
		t.Errorf("unexpected origin for a.b")
	}
}
