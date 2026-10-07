package vcard

import (
	"reflect"
	"testing"
)

func TestParsePID(t *testing.T) {
	tests := []struct {
		s       string
		want    PID
		wantErr bool
	}{
		{s: "1.1", want: PID{Source: 1, ID: 1}},
		{s: "2", want: PID{ID: 2}},
		{s: "12.345", want: PID{Source: 12, ID: 345}},
		{s: "", wantErr: true},
		{s: "a", wantErr: true},
		{s: "1.", wantErr: true},
		{s: ".1", wantErr: true},
		{s: "1.2.3", wantErr: true},
		{s: "0", wantErr: true},
		{s: "0.1", wantErr: true},
		{s: "-1", wantErr: true},
		{s: "+1", wantErr: true},
	}
	for _, test := range tests {
		pid, err := ParsePID(test.s)
		if test.wantErr {
			if err == nil {
				t.Errorf("ParsePID(%q) = %v, expected error", test.s, pid)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePID(%q) returned error: %v", test.s, err)
		} else if pid != test.want {
			t.Errorf("ParsePID(%q) = %v, want %v", test.s, pid, test.want)
		} else if got := pid.String(); got != test.s {
			t.Errorf("PID.String() = %q, want %q", got, test.s)
		}
	}
}

func TestParseClientPIDMap(t *testing.T) {
	m, err := ParseClientPIDMap("1;urn:uuid:53e374d9-337e-4727-8803-a1e9c14e0556")
	if err != nil {
		t.Fatalf("ParseClientPIDMap() returned error: %v", err)
	}
	want := ClientPIDMap{SourceID: 1, URI: "urn:uuid:53e374d9-337e-4727-8803-a1e9c14e0556"}
	if m != want {
		t.Errorf("ParseClientPIDMap() = %v, want %v", m, want)
	}
	if got := m.String(); got != "1;urn:uuid:53e374d9-337e-4727-8803-a1e9c14e0556" {
		t.Errorf("ClientPIDMap.String() = %q", got)
	}

	for _, s := range []string{"", "1", "x;urn:uuid:abc", "0;urn:uuid:abc", "1;"} {
		if _, err := ParseClientPIDMap(s); err == nil {
			t.Errorf("ParseClientPIDMap(%q) expected error", s)
		}
	}
}

func TestFieldPIDs(t *testing.T) {
	f := &Field{
		Value:  "jdoe@example.com",
		Params: Params{ParamPID: {"1.1", "2.3", "4"}},
	}
	pids, err := f.PIDs()
	if err != nil {
		t.Fatalf("Field.PIDs() returned error: %v", err)
	}
	want := []PID{{Source: 1, ID: 1}, {Source: 2, ID: 3}, {ID: 4}}
	if !reflect.DeepEqual(pids, want) {
		t.Errorf("Field.PIDs() = %v, want %v", pids, want)
	}

	g := &Field{Value: "x"}
	g.SetPIDs(want)
	if got := g.Params[ParamPID]; !reflect.DeepEqual(got, []string{"1.1", "2.3", "4"}) {
		t.Errorf("Field.SetPIDs() set params %v", got)
	}
	g.SetPIDs(nil)
	if _, ok := g.Params[ParamPID]; ok {
		t.Errorf("Field.SetPIDs(nil) did not clear PID params")
	}
}

func TestCardGlobalPIDs(t *testing.T) {
	c := Card{
		"CLIENTPIDMAP": []*Field{{Value: "1;urn:uuid:phone"}},
		"EMAIL": []*Field{{
			Value:  "jdoe@example.com",
			Params: Params{ParamPID: {"1.1"}},
		}},
	}
	globals, err := c.GlobalPIDs(c.Get("EMAIL"))
	if err != nil {
		t.Fatalf("Card.GlobalPIDs() returned error: %v", err)
	}
	want := []GlobalPID{{ClientURI: "urn:uuid:phone", LocalID: 1}}
	if !reflect.DeepEqual(globals, want) {
		t.Errorf("Card.GlobalPIDs() = %v, want %v", globals, want)
	}
}
