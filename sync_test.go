package vcard

import (
	"bytes"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	phoneURI   = "urn:uuid:11111111-1111-1111-1111-111111111111"
	desktopURI = "urn:uuid:22222222-2222-2222-2222-222222222222"
	serverURI  = "urn:uuid:33333333-3333-3333-3333-333333333333"
)

// The phone and the desktop both synced against a common server. Both use
// local source ID 1 for themselves and 2 for the server, so the same local
// PID (e.g. 1.1) refers to different global identities on each card.
var testSyncCardPhone = Card{
	"VERSION": []*Field{{Value: "4.0"}},
	"FN":      []*Field{{Value: "J. Doe"}},
	"EMAIL": []*Field{
		{Value: "jdoe@example.com", Params: Params{"TYPE": {"home"}, "PID": {"2.1"}}},
		{Value: "jdoe@phone.example.com", Params: Params{"TYPE": {"work"}, "PID": {"1.1"}}},
	},
	"TEL":    []*Field{{Value: "tel:+1-555-0100", Params: Params{"PID": {"1.5", "2.7"}}}},
	"NOTE":   []*Field{{Value: "phone note", Params: Params{"PID": {"2.2"}}}},
	"X-MOOD": []*Field{{Value: "happy", Params: Params{"PID": {"1.9"}}}},
	"CLIENTPIDMAP": []*Field{
		{Value: "1;" + phoneURI},
		{Value: "2;" + serverURI},
	},
}

var testSyncCardDesktop = Card{
	"VERSION": []*Field{{Value: "4.0"}},
	"FN":      []*Field{{Value: "J. Doe"}},
	"EMAIL": []*Field{
		{Value: "jdoe@example.com", Params: Params{"TYPE": {"home"}, "PID": {"2.1"}}},
		{Value: "jdoe@desktop.example.com", Params: Params{"TYPE": {"work"}, "PID": {"1.1"}}},
	},
	"NOTE":   []*Field{{Value: "desktop note", Params: Params{"PID": {"2.2"}}}},
	"X-MOOD": []*Field{{Value: "happy", Params: Params{"PID": {"1.9"}}}},
	"CLIENTPIDMAP": []*Field{
		{Value: "1;" + desktopURI},
		{Value: "2;" + serverURI},
	},
}

func TestParsePID(t *testing.T) {
	pid, err := ParsePID("1.1")
	if err != nil {
		t.Fatal("Expected no error parsing PID, got:", err)
	}
	if expected := (PID{Source: 1, ID: "1"}); pid != expected {
		t.Errorf("Expected PID %+v, got %+v", expected, pid)
	}
	if s := pid.String(); s != "1.1" {
		t.Errorf("Expected PID string %q, got %q", "1.1", s)
	}

	pid, err = ParsePID("12.abc.def")
	if err != nil {
		t.Fatal("Expected no error parsing PID, got:", err)
	}
	if expected := (PID{Source: 12, ID: "abc.def"}); pid != expected {
		t.Errorf("Expected PID %+v, got %+v", expected, pid)
	}

	for _, s := range []string{"", "1", ".1", "1.", "x.1"} {
		if _, err := ParsePID(s); err == nil {
			t.Errorf("Expected error parsing PID %q, got nil", s)
		}
	}
}

func TestParseClientPIDMap(t *testing.T) {
	m, err := ParseClientPIDMap("1;" + phoneURI)
	if err != nil {
		t.Fatal("Expected no error parsing CLIENTPIDMAP, got:", err)
	}
	if expected := (ClientPIDMap{Source: 1, URI: phoneURI}); m != expected {
		t.Errorf("Expected CLIENTPIDMAP %+v, got %+v", expected, m)
	}
	if s := m.String(); s != "1;"+phoneURI {
		t.Errorf("Expected CLIENTPIDMAP string %q, got %q", "1;"+phoneURI, s)
	}

	for _, v := range []string{"", "1", ";uri", "x;uri"} {
		if _, err := ParseClientPIDMap(v); err == nil {
			t.Errorf("Expected error parsing CLIENTPIDMAP %q, got nil", v)
		}
	}
}

func TestFieldPIDs(t *testing.T) {
	f := &Field{Params: Params{ParamPID: {"1.1", "2.3"}}}
	pids, err := f.PIDs()
	if err != nil {
		t.Fatal("Expected no error getting PIDs, got:", err)
	}
	expected := []PID{{Source: 1, ID: "1"}, {Source: 2, ID: "3"}}
	if !reflect.DeepEqual(expected, pids) {
		t.Errorf("Expected PIDs %+v, got %+v", expected, pids)
	}

	f.SetPIDs([]PID{{Source: 4, ID: "2"}})
	if values := f.Params[ParamPID]; !reflect.DeepEqual(values, []string{"4.2"}) {
		t.Errorf("Expected PID param %v, got %v", []string{"4.2"}, values)
	}
	f.SetPIDs(nil)
	if _, ok := f.Params[ParamPID]; ok {
		t.Error("Expected PID param to be removed")
	}
}

func TestCardGlobalPIDs(t *testing.T) {
	tel := testSyncCardPhone.Get(FieldTelephone)
	gpids, err := testSyncCardPhone.GlobalPIDs(tel)
	if err != nil {
		t.Fatal("Expected no error resolving PIDs, got:", err)
	}
	expected := []GlobalPID{
		{Client: phoneURI, ID: "5"},
		{Client: serverURI, ID: "7"},
	}
	if !reflect.DeepEqual(expected, gpids) {
		t.Errorf("Expected global PIDs %+v, got %+v", expected, gpids)
	}

	broken := Card{
		"EMAIL":        []*Field{{Value: "x@example.com", Params: Params{ParamPID: {"3.1"}}}},
		"CLIENTPIDMAP": []*Field{{Value: "1;" + phoneURI}},
	}
	_, err = broken.GlobalPIDs(broken.Get(FieldEmail))
	var missing *MissingClientPIDMapError
	if !errors.As(err, &missing) {
		t.Fatalf("Expected MissingClientPIDMapError, got: %v", err)
	}
	if missing.Source != 3 {
		t.Errorf("Expected missing source ID 3, got %d", missing.Source)
	}
}

func fieldGlobalPIDs(t *testing.T, c Card, f *Field) []string {
	t.Helper()
	gpids, err := c.GlobalPIDs(f)
	if err != nil {
		t.Fatalf("Expected no error resolving PIDs of %+v, got: %v", f, err)
	}
	strs := make([]string, len(gpids))
	for i, g := range gpids {
		strs[i] = g.Client + "|" + g.ID
	}
	sort.Strings(strs)
	return strs
}

func canonicalFields(t *testing.T, c Card) []string {
	t.Helper()
	var lines []string
	for name := range c {
		if name == FieldClientPIDMap {
			continue
		}
		for _, f := range c[name] {
			lines = append(lines, name+"|"+f.Value+"|"+strings.Join(fieldGlobalPIDs(t, c, f), ","))
		}
	}
	sort.Strings(lines)
	return lines
}

func TestMergeCards(t *testing.T) {
	merged, conflicts, err := MergeCards(testSyncCardPhone, testSyncCardDesktop)
	if err != nil {
		t.Fatal("Expected no error merging cards, got:", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("Expected 1 conflict, got %d: %+v", len(conflicts), conflicts)
	}
	conflict := conflicts[0]
	if expected := (GlobalPID{Client: serverURI, ID: "2"}); conflict.PID != expected {
		t.Errorf("Expected conflict PID %+v, got %+v", expected, conflict.PID)
	}
	if conflict.First.Value != "phone note" || conflict.Second.Value != "desktop note" {
		t.Errorf("Unexpected conflict fields: %+v vs %+v", conflict.First, conflict.Second)
	}

	maps, err := merged.ClientPIDMaps()
	if err != nil {
		t.Fatal("Expected no error parsing merged CLIENTPIDMAP, got:", err)
	}
	uris := make(map[string]bool)
	for _, m := range maps {
		uris[m.URI] = true
	}
	for _, uri := range []string{phoneURI, desktopURI, serverURI} {
		if !uris[uri] {
			t.Errorf("Expected merged CLIENTPIDMAP to contain %q, got %+v", uri, maps)
		}
	}

	// Every PID reference in the output must resolve back to a client.
	for name := range merged {
		for _, f := range merged[name] {
			if _, err := merged.GlobalPIDs(f); err != nil {
				t.Errorf("Unresolvable PID on merged %v field %+v: %v", name, f, err)
			}
		}
	}

	// The home email is the same sync object on both cards (server
	// identity): merged into a single field. The work emails share local
	// PID 1.1 but belong to different clients: kept separate.
	emails := merged[FieldEmail]
	if len(emails) != 3 {
		t.Fatalf("Expected 3 merged emails, got %d: %+v", len(emails), emails)
	}
	expectedEmails := map[string][]string{
		"jdoe@example.com":         {serverURI + "|1"},
		"jdoe@phone.example.com":   {phoneURI + "|1"},
		"jdoe@desktop.example.com": {desktopURI + "|1"},
	}
	for _, f := range emails {
		expected, ok := expectedEmails[f.Value]
		if !ok {
			t.Errorf("Unexpected merged email %q", f.Value)
			continue
		}
		if gpids := fieldGlobalPIDs(t, merged, f); !reflect.DeepEqual(expected, gpids) {
			t.Errorf("Expected email %q global PIDs %v, got %v", f.Value, expected, gpids)
		}
	}

	// A field can carry multiple PIDs; all identities must survive.
	tel := merged.Get(FieldTelephone)
	if tel == nil || len(merged[FieldTelephone]) != 1 {
		t.Fatalf("Expected a single merged TEL field, got %+v", merged[FieldTelephone])
	}
	expectedTEL := []string{phoneURI + "|5", serverURI + "|7"}
	if gpids := fieldGlobalPIDs(t, merged, tel); !reflect.DeepEqual(expectedTEL, gpids) {
		t.Errorf("Expected TEL global PIDs %v, got %v", expectedTEL, gpids)
	}

	// Identical text but different sync identities: not the same object.
	if moods := merged["X-MOOD"]; len(moods) != 2 {
		t.Errorf("Expected 2 X-MOOD fields, got %+v", moods)
	}

	// On conflict the first card wins in the merged output.
	if v := merged.Value("NOTE"); v != "phone note" {
		t.Errorf("Expected merged NOTE %q, got %q", "phone note", v)
	}

	// Fields without PID are deduplicated.
	if n := len(merged[FieldVersion]); n != 1 {
		t.Errorf("Expected 1 VERSION field, got %d", n)
	}
	if n := len(merged[FieldFormattedName]); n != 1 {
		t.Errorf("Expected 1 FN field, got %d", n)
	}

	// The merged card must survive an encode/decode round-trip with all
	// references still resolving to the correct clients.
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(merged); err != nil {
		t.Fatal("Expected no error encoding merged card, got:", err)
	}
	decoded, err := NewDecoder(&buf).Decode()
	if err != nil {
		t.Fatal("Expected no error decoding merged card, got:", err)
	}
	if expected, got := canonicalFields(t, merged), canonicalFields(t, decoded); !reflect.DeepEqual(expected, got) {
		t.Errorf("Round-trip mismatch:\nexpected: %v\ngot:      %v", expected, got)
	}
}

func TestMergeCardsOrder(t *testing.T) {
	forward, _, err := MergeCards(testSyncCardPhone, testSyncCardDesktop)
	if err != nil {
		t.Fatal("Expected no error merging cards, got:", err)
	}
	reverse, revConflicts, err := MergeCards(testSyncCardDesktop, testSyncCardPhone)
	if err != nil {
		t.Fatal("Expected no error merging cards, got:", err)
	}

	if len(revConflicts) != 1 {
		t.Fatalf("Expected 1 conflict, got %d: %+v", len(revConflicts), revConflicts)
	}
	if revConflicts[0].First.Value != "desktop note" || revConflicts[0].Second.Value != "phone note" {
		t.Errorf("Unexpected reversed conflict fields: %+v", revConflicts[0])
	}

	// Apart from the conflicted NOTE (first card wins), both merge orders
	// must produce the same fields with the same global identities.
	withoutNote := func(lines []string) []string {
		var kept []string
		for _, l := range lines {
			if !strings.HasPrefix(l, "NOTE|") {
				kept = append(kept, l)
			}
		}
		return kept
	}
	fwd := withoutNote(canonicalFields(t, forward))
	rev := withoutNote(canonicalFields(t, reverse))
	if !reflect.DeepEqual(fwd, rev) {
		t.Errorf("Merge order changed the result:\nforward: %v\nreverse: %v", fwd, rev)
	}
}

func TestMergeCardsMissingClientPIDMap(t *testing.T) {
	broken := Card{
		"EMAIL":        []*Field{{Value: "x@example.com", Params: Params{ParamPID: {"3.1"}}}},
		"CLIENTPIDMAP": []*Field{{Value: "1;" + desktopURI}},
	}
	_, _, err := MergeCards(testSyncCardPhone, broken)
	var missing *MissingClientPIDMapError
	if !errors.As(err, &missing) {
		t.Fatalf("Expected MissingClientPIDMapError, got: %v", err)
	}
	if missing.Source != 3 {
		t.Errorf("Expected missing source ID 3, got %d", missing.Source)
	}
}
