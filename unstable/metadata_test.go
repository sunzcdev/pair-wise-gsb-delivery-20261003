package unstable

import "testing"

func TestMetaSegmentString(t *testing.T) {
	if got := (MetaSegment{Name: "Field", Index: -1}).String(); got != "Field" {
		t.Errorf("name segment = %q", got)
	}
	if got := (MetaSegment{Name: "", Index: 3}).String(); got != "[3]" {
		t.Errorf("index segment = %q", got)
	}
	if (MetaSegment{Index: 0}).IsIndex() != true {
		t.Error("index 0 should be an index segment")
	}
	if (MetaSegment{Name: "x", Index: -1}).IsIndex() != false {
		t.Error("named segment should not be an index")
	}
}

func TestKeyPathJoin(t *testing.T) {
	if got := (KeyPath{"a", "b.c"}).Join(); got != "a.b.c" {
		t.Errorf("join = %q", got)
	}
}

func TestMetadataLookup(t *testing.T) {
	m := NewMetadata()
	m.Add(Location{
		Field: []MetaSegment{{Name: "Server", Index: -1}, {Name: "Host", Index: -1}},
		Key:   KeyPath{"server", "host"},
		Range: Range{Offset: 10, Length: 4},
	})
	m.Add(Location{
		Field: []MetaSegment{{Name: "Items", Index: -1}, {Name: "", Index: 2}},
		Key:   KeyPath{"items"},
		Range: Range{Offset: 20, Length: 1},
	})

	if m.Len() != 2 {
		t.Fatalf("len = %d", m.Len())
	}
	loc, ok := m.LookupField("Server", "Host")
	if !ok || loc.Range.Offset != 10 {
		t.Errorf("LookupField = %+v %v", loc, ok)
	}
	if got := loc.FieldPath(); got != "Server.Host" {
		t.Errorf("FieldPath = %q", got)
	}

	loc, ok = m.LookupPath([]MetaSegment{{Name: "Items", Index: -1}, {Index: 2}})
	if !ok || loc.FieldPath() != "Items[2]" {
		t.Errorf("LookupPath = %q %v", loc.FieldPath(), ok)
	}
	if _, ok := m.LookupField("Items"); ok {
		t.Error("LookupField must not match paths containing indexes")
	}

	if _, ok := m.LookupKey("server", "host"); !ok {
		t.Error("LookupKey failed")
	}

	locs := m.Locations()
	locs[0].Range.Offset = 999
	if m.Locations()[0].Range.Offset == 999 {
		t.Error("Locations should return a copy")
	}

	// Extra ranges.
	loc.Extra = []Range{{Offset: 30, Length: 2}}
	if len(loc.Ranges()) != 2 {
		t.Errorf("Ranges = %d", len(loc.Ranges()))
	}

	m.Reset()
	if m.Len() != 0 {
		t.Fatalf("len after reset = %d", m.Len())
	}

	// Nil metadata must not panic.
	var nilm *Metadata
	nilm.Reset()
	if nilm.Len() != 0 || nilm.Locations() != nil {
		t.Error("nil metadata misbehaved")
	}
	nilm.Add(Location{})
	if nilm.Len() != 0 {
		t.Error("Add on nil metadata should be ignored")
	}
}
