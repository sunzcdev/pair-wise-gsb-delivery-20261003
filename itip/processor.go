package itip

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	ical "github.com/arran4/golang-ical"
)

// Disposition classifies how one VEVENT of an iTIP message was handled.
type Disposition string

const (
	// DispositionApplied means the message changed the stored schedule state
	// or restated the already current revision.
	DispositionApplied Disposition = "APPLIED"

	// DispositionIgnored means the message carried no actionable content
	// (stale version, unknown object, duplicate delivery, future revision).
	DispositionIgnored Disposition = "IGNORED"

	// DispositionConflict means the message could not be merged automatically
	// and needs operator or protocol level resolution.
	DispositionConflict Disposition = "CONFLICT"
)

// Reason explains the disposition. Values are stable identifiers.
type Reason string

const (
	// ReasonNone is used when no further explanation is needed.
	ReasonNone Reason = ""

	// ReasonDuplicate marks an exact redelivery of the revision or reply
	// already applied.
	ReasonDuplicate Reason = "duplicate"

	// ReasonStale marks an older SEQUENCE, or the same SEQUENCE with an
	// older DTSTAMP.
	ReasonStale Reason = "stale"

	// ReasonSameTimestamp marks the same SEQUENCE and DTSTAMP as the stored
	// revision but different content, which RFC 5546 forbids.
	ReasonSameTimestamp Reason = "same-timestamp-different-content"

	// ReasonFuture marks a reply whose SEQUENCE does not match any organizer
	// revision known to the server.
	ReasonFuture Reason = "future-revision"

	// ReasonObjectUnknown marks a message for a UID the server has never
	// seen.
	ReasonObjectUnknown Reason = "unknown-object"

	// ReasonInstanceUnknown marks a per-instance message whose
	// RECURRENCE-ID has no stored override and cannot be derived.
	ReasonInstanceUnknown Reason = "unknown-instance"

	// ReasonNotOrganizer marks an organizer only operation from a sender
	// other than the bound organizer.
	ReasonNotOrganizer Reason = "not-organizer"

	// ReasonNotAttendee marks a REPLY from a sender that is not an attendee
	// of the target component.
	ReasonNotAttendee Reason = "not-attendee"

	// ReasonOrganizerMismatch marks a payload whose ORGANIZER property does
	// not name the authenticated organizer.
	ReasonOrganizerMismatch Reason = "organizer-property-mismatch"

	// ReasonInvalidPartstat marks a REPLY carrying an unrecognized
	// PARTSTAT value.
	ReasonInvalidPartstat Reason = "invalid-partstat"

	// ReasonMalformed marks a component missing UID or DTSTAMP, carrying an
	// unparsable SEQUENCE or RECURRENCE-ID, or otherwise unsuitable for
	// merging.
	ReasonMalformed Reason = "malformed"

	// ReasonUnsupportedMethod marks a calendar without a recognized
	// iTIP METHOD, or a component type other than VEVENT.
	ReasonUnsupportedMethod Reason = "unsupported-method"

	// ReasonNoComponents marks a calendar carrying no VEVENT components.
	ReasonNoComponents Reason = "no-components"
)

// Sender is the identity the calling transport already authenticated. The
// library deliberately has no way to "prove" this identity from the ICS
// payload.
type Sender struct {
	// Email is the normalized (lower case, mailto-less) mailbox of the
	// authenticated sender.
	Email string
}

// Message is one incoming iTIP transport message.
type Message struct {
	// Method is the VCALENDAR METHOD (REQUEST, REPLY or CANCEL).
	Method ical.Method

	// Calendar is the parsed VCALENDAR. Only its VEVENT components are
	// considered.
	Calendar *ical.Calendar
}

// ParseMessage parses an iTIP VCALENDAR from r and reads its METHOD property.
func ParseMessage(r io.Reader) (*Message, error) {
	cal, err := ical.ParseCalendar(r)
	if err != nil {
		return nil, err
	}
	return &Message{Method: calendarMethod(cal), Calendar: cal}, nil
}

// NewMessage wraps an already parsed calendar, reading its METHOD.
func NewMessage(cal *ical.Calendar) *Message {
	return &Message{Method: calendarMethod(cal), Calendar: cal}
}

func calendarMethod(cal *ical.Calendar) ical.Method {
	if cal == nil {
		return ""
	}
	for i := range cal.CalendarProperties {
		if cal.CalendarProperties[i].IANAToken == string(ical.ComponentPropertyMethod) {
			return ical.Method(cal.CalendarProperties[i].Value)
		}
	}
	return ""
}

// ComponentResult is the disposition of one VEVENT inside a message.
type ComponentResult struct {
	Disposition    Disposition
	Reason         Reason
	Detail         string
	UID            string
	RecurrenceID   string
	Sequence       int
	DtStamp        time.Time
	Duplicate      bool
	AppliedReplies map[string]ical.ParticipationStatus
}

// Result is the aggregate disposition of an iTIP message. When several
// components disagree, CONFLICT wins over APPLIED which wins over IGNORED.
// Duplicate is true only when every component result was an exact redelivery.
type Result struct {
	Disposition Disposition
	Reason      Reason
	Detail      string
	Components  []ComponentResult

	// Duplicate is true when the whole message was an exact redelivery.
	Duplicate bool
}

// Processor merges iTIP messages into a Store.
type Processor struct {
	store Store
	mu    sync.Mutex
}

// NewProcessor returns a Processor backed by store.
func NewProcessor(store Store) *Processor {
	if store == nil {
		panic("itip: nil store")
	}
	return &Processor{store: store}
}

var (
	// ErrNoSender indicates Process was called without an authenticated
	// sender.
	ErrNoSender = errors.New("itip: verified sender identity is required")

	// ErrNoMessage indicates Process was called without a message.
	ErrNoMessage = errors.New("itip: message is required")
)

// Process folds msg into the store on behalf of the authenticated sender. It
// returns an error only for caller/transport level misuse; every calendaring
// decision, including malformed content, is reported through Result.
func (p *Processor) Process(sender Sender, msg *Message) (*Result, error) {
	if strings.TrimSpace(sender.Email) == "" {
		return nil, ErrNoSender
	}
	if msg == nil || msg.Calendar == nil {
		return nil, ErrNoMessage
	}
	sender.Email = normalizeEmail(sender.Email)

	// Serialize read-modify-write against the store. Store implementations
	// that already provide their own serializable transactions may replace
	// the Processor; the lock keeps the in-memory implementation correct.
	p.mu.Lock()
	defer p.mu.Unlock()

	result := &Result{}
	switch msg.Method {
	case ical.MethodRequest, ical.MethodReply, ical.MethodCancel:
	default:
		result.Disposition = DispositionConflict
		result.Reason = ReasonUnsupportedMethod
		result.Detail = "method " + string(msg.Method) + " is not supported"
		return result, nil
	}

	events := msg.Calendar.Events()
	if len(events) == 0 {
		result.Disposition = DispositionIgnored
		result.Reason = ReasonNoComponents
		return result, nil
	}

	for _, event := range events {
		result.Components = append(result.Components, p.processEvent(sender, msg.Method, event))
	}

	aggregate(result)
	return result, nil
}

// ProcessCalendar is a convenience wrapper for callers that already have a
// parsed calendar.
func (p *Processor) ProcessCalendar(sender Sender, cal *ical.Calendar) (*Result, error) {
	return p.Process(sender, NewMessage(cal))
}

func aggregate(result *Result) {
	allDuplicate := true
	for i := range result.Components {
		c := &result.Components[i]
		if !c.Duplicate {
			allDuplicate = false
		}
		switch c.Disposition {
		case DispositionConflict:
			result.Disposition = DispositionConflict
			result.Reason = c.Reason
			result.Detail = c.Detail
			result.Duplicate = false
			return
		case DispositionApplied:
			if result.Disposition != DispositionConflict {
				result.Disposition = DispositionApplied
				result.Reason = c.Reason
				result.Detail = c.Detail
			}
		case DispositionIgnored:
			if result.Disposition == "" {
				result.Disposition = DispositionIgnored
				result.Reason = c.Reason
				result.Detail = c.Detail
			}
		}
	}
	result.Duplicate = allDuplicate
}

func (p *Processor) processEvent(sender Sender, method ical.Method, event *ical.VEvent) ComponentResult {
	uid := normalizeUID(event.Id())
	cr := ComponentResult{UID: uid, Sequence: -1}
	if uid == "" {
		return conflict(cr, ReasonMalformed, "VEVENT without UID")
	}

	dtstamp, err := event.GetDtStampTime()
	if err != nil {
		return conflict(cr, ReasonMalformed, "VEVENT without parseable DTSTAMP: "+err.Error())
	}
	cr.DtStamp = dtstamp.UTC()

	seq := 0
	if prop := event.GetProperty(ical.ComponentPropertySequence); prop != nil && prop.Value != "" {
		seq, err = strconv.Atoi(strings.TrimSpace(prop.Value))
		if err != nil {
			return conflict(cr, ReasonMalformed, "unparseable SEQUENCE "+prop.Value)
		}
	}
	cr.Sequence = seq

	recurrenceID, hasRecurrenceID, err := eventRecurrenceID(event)
	if err != nil {
		cr.RecurrenceID = rawRecurrenceID(event)
		return conflict(cr, ReasonMalformed, "unparseable RECURRENCE-ID: "+err.Error())
	}
	if hasRecurrenceID {
		cr.RecurrenceID = rawRecurrenceID(event)
	}

	incoming := Revision{Sequence: seq, DtStamp: dtstamp.UTC(), Digest: digestEvent(event)}

	switch method {
	case ical.MethodRequest:
		return p.applyRequest(sender, uid, recurrenceID, hasRecurrenceID, event, incoming, cr)
	case ical.MethodCancel:
		return p.applyCancel(sender, uid, recurrenceID, hasRecurrenceID, event, incoming, cr)
	case ical.MethodReply:
		return p.applyReply(sender, uid, recurrenceID, hasRecurrenceID, event, incoming, cr)
	default:
		return conflict(cr, ReasonUnsupportedMethod, "unsupported method "+string(method))
	}
}

func conflict(cr ComponentResult, reason Reason, detail string) ComponentResult {
	cr.Disposition = DispositionConflict
	cr.Reason = reason
	cr.Detail = detail
	return cr
}

// componentFor returns the target component state for an instance message:
// the override when present, otherwise the master. ok is false when neither is
// available.
func componentFor(obj *Object, recID time.Time, hasRecurrenceID bool) (*ComponentState, bool) {
	if !hasRecurrenceID {
		if obj.Master != nil {
			return obj.Master, true
		}
		return nil, false
	}
	if cs, ok := obj.Instances[recID.UTC()]; ok {
		return cs, true
	}
	if obj.Master != nil {
		return obj.Master, false
	}
	return nil, false
}

// digestEvent returns a stable digest of the canonical serialization of event.
func digestEvent(event *ical.VEvent) string {
	text := canonicalEventText(event)
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func canonicalEventText(event *ical.VEvent) string {
	cal := ical.NewCalendar()
	cal.AddVEvent(event)
	var buf bytes.Buffer
	// Use the RFC mandated CRLF regardless of the host default so digests
	// are stable across parse/serialize round trips and machines.
	_ = cal.SerializeTo(&buf, ical.WithNewLine("\r\n"))
	return buf.String()
}

// cloneEvent produces an independent copy of event by serializing and
// re-parsing it inside a throwaway VCALENDAR envelope.
func cloneEvent(event *ical.VEvent) (*ical.VEvent, error) {
	cal := ical.NewCalendar()
	cal.AddVEvent(event)
	var buf bytes.Buffer
	if err := cal.SerializeTo(&buf, ical.WithNewLine("\r\n")); err != nil {
		return nil, err
	}
	parsed, err := ical.ParseCalendar(&buf)
	if err != nil {
		return nil, err
	}
	events := parsed.Events()
	if len(events) != 1 {
		return nil, errors.New("itip: clone produced unexpected event count")
	}
	return events[0], nil
}

func eventRecurrenceID(event *ical.VEvent) (time.Time, bool, error) {
	if event.GetProperty(ical.ComponentPropertyRecurrenceId) == nil {
		return time.Time{}, false, nil
	}
	t, err := event.GetRecurrenceID()
	if err != nil {
		return time.Time{}, true, err
	}
	return t.UTC(), true, nil
}

func rawRecurrenceID(event *ical.VEvent) string {
	if prop := event.GetProperty(ical.ComponentPropertyRecurrenceId); prop != nil {
		return prop.Value
	}
	return ""
}

func normalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "mailto:")
	return s
}

func normalizeUID(s string) string {
	return strings.TrimSpace(s)
}

// organizerEmail reads the declared ORGANIZER property. It is used only as a
// cross check against the authenticated organizer, never for authorization.
func organizerEmail(event *ical.VEvent) string {
	prop := event.GetProperty(ical.ComponentPropertyOrganizer)
	if prop == nil {
		return ""
	}
	return normalizeEmail(prop.Value)
}
