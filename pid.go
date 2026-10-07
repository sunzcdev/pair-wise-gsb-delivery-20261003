package vcard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A PID is a parsed PID parameter value, as defined in RFC 6350 section 5.5.
// A PID is either a client-local identifier or a compound identifier made of
// a CLIENTPIDMAP source ID and a client-local ID.
type PID struct {
	// Source is the CLIENTPIDMAP source ID. It is zero for local-only PIDs.
	Source int
	// ID is the client-local identifier.
	ID int
}

// ParsePID parses a PID parameter value such as "1.1" or "2".
func ParsePID(s string) (PID, error) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		source, ok := parsePIDNumber(s[:i])
		if !ok {
			return PID{}, fmt.Errorf("vcard: invalid PID %q", s)
		}
		id, ok := parsePIDNumber(s[i+1:])
		if !ok {
			return PID{}, fmt.Errorf("vcard: invalid PID %q", s)
		}
		return PID{Source: source, ID: id}, nil
	}
	id, ok := parsePIDNumber(s)
	if !ok {
		return PID{}, fmt.Errorf("vcard: invalid PID %q", s)
	}
	return PID{ID: id}, nil
}

func parsePIDNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// String formats the PID as a PID parameter value.
func (pid PID) String() string {
	if pid.Source == 0 {
		return strconv.Itoa(pid.ID)
	}
	return strconv.Itoa(pid.Source) + "." + strconv.Itoa(pid.ID)
}

// PIDs parses the field's PID parameters. A field may carry multiple PIDs,
// one for each client that knows about it.
func (f *Field) PIDs() ([]PID, error) {
	values := f.Params[ParamPID]
	pids := make([]PID, 0, len(values))
	for _, v := range values {
		pid, err := ParsePID(v)
		if err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// SetPIDs replaces the field's PID parameters.
func (f *Field) SetPIDs(pids []PID) {
	if len(pids) == 0 {
		delete(f.Params, ParamPID)
		return
	}
	if f.Params == nil {
		f.Params = make(Params)
	}
	values := make([]string, len(pids))
	for i, pid := range pids {
		values[i] = pid.String()
	}
	f.Params[ParamPID] = values
}

// A ClientPIDMap is a parsed CLIENTPIDMAP field, as defined in RFC 6350
// section 6.7.1. It maps a source ID to the URI of the client that assigned
// the local identifiers of compound PIDs.
type ClientPIDMap struct {
	SourceID int
	URI      string
}

// ParseClientPIDMap parses a CLIENTPIDMAP field value such as
// "1;urn:uuid:53e374d9-337e-4727-8803-a1e9c14e0556".
func ParseClientPIDMap(value string) (ClientPIDMap, error) {
	parts := strings.SplitN(value, ";", 2)
	if len(parts) != 2 || parts[1] == "" {
		return ClientPIDMap{}, fmt.Errorf("vcard: invalid CLIENTPIDMAP %q", value)
	}
	id, ok := parsePIDNumber(parts[0])
	if !ok {
		return ClientPIDMap{}, fmt.Errorf("vcard: invalid CLIENTPIDMAP %q", value)
	}
	return ClientPIDMap{SourceID: id, URI: parts[1]}, nil
}

// String formats the map as a CLIENTPIDMAP field value.
func (m ClientPIDMap) String() string {
	return strconv.Itoa(m.SourceID) + ";" + m.URI
}

// ClientPIDMaps parses the card's CLIENTPIDMAP fields.
func (c Card) ClientPIDMaps() ([]ClientPIDMap, error) {
	fields := c[FieldClientPIDMap]
	maps := make([]ClientPIDMap, 0, len(fields))
	for _, f := range fields {
		m, err := ParseClientPIDMap(f.Value)
		if err != nil {
			return nil, err
		}
		maps = append(maps, m)
	}
	return maps, nil
}

// A GlobalPID identifies a property instance across clients. It combines the
// URI of the client that assigned the local identifier (taken from
// CLIENTPIDMAP) with the client-local identifier. Two PIDs from different
// cards only refer to the same sync object if their GlobalPIDs are equal;
// equal field values are not sufficient.
type GlobalPID struct {
	ClientURI string
	LocalID   int
}

// MissingClientPIDMapError is returned when a compound PID references a
// CLIENTPIDMAP source ID that the card doesn't define.
type MissingClientPIDMapError struct {
	SourceID int
}

func (e *MissingClientPIDMapError) Error() string {
	return fmt.Sprintf("vcard: PID references missing CLIENTPIDMAP source %d", e.SourceID)
}

// GlobalPIDs resolves the field's compound PID parameters against the card's
// CLIENTPIDMAP fields. Local-only PIDs have no global identity and are
// skipped.
func (c Card) GlobalPIDs(f *Field) ([]GlobalPID, error) {
	maps, err := c.ClientPIDMaps()
	if err != nil {
		return nil, err
	}
	bySourceID := make(map[int]string, len(maps))
	for _, m := range maps {
		bySourceID[m.SourceID] = m.URI
	}

	pids, err := f.PIDs()
	if err != nil {
		return nil, err
	}
	var globals []GlobalPID
	for _, pid := range pids {
		if pid.Source == 0 {
			continue
		}
		uri, ok := bySourceID[pid.Source]
		if !ok {
			return nil, &MissingClientPIDMapError{SourceID: pid.Source}
		}
		globals = append(globals, GlobalPID{ClientURI: uri, LocalID: pid.ID})
	}
	return globals, nil
}
