package itip

import (
	"strings"
	"testing"
	"time"

	ical "github.com/arran4/golang-ical"
)

const tsLayout = "20060102T150405Z"

func mustTS(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(tsLayout, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return tm.UTC()
}

type eventSpec struct {
	uid          string
	sequence     int
	dtstamp      string
	start        string
	end          string
	summary      string
	rrule        string
	recurrenceID string
	status       ical.ObjectStatus
	organizer    string
	attendees    []attendeeSpec
}

type attendeeSpec struct {
	email    string
	partstat ical.ParticipationStatus
}

func buildMessage(t *testing.T, method ical.Method, specs ...eventSpec) *Message {
	t.Helper()
	cal := ical.NewCalendar()
	cal.SetMethod(method)
	for _, spec := range specs {
		event := ical.NewEvent(spec.uid)
		event.SetDtStampTime(mustTS(t, spec.dtstamp))
		event.SetSequence(spec.sequence)
		event.SetStartAt(mustTS(t, spec.start))
		if spec.end != "" {
			event.SetEndAt(mustTS(t, spec.end))
		}
		if spec.summary != "" {
			event.SetSummary(spec.summary)
		}
		if spec.rrule != "" {
			event.AddRrule(spec.rrule)
		}
		if spec.organizer != "" {
			event.SetOrganizer(spec.organizer)
		}
		if spec.status != "" {
			event.SetStatus(spec.status)
		}
		if spec.recurrenceID != "" {
			event.AddProperty(ical.ComponentPropertyRecurrenceId, spec.recurrenceID)
		}
		for _, a := range spec.attendees {
			params := []ical.PropertyParameter{}
			if a.partstat != "" {
				params = append(params, &ical.KeyValues{
					Key:   string(ical.ParameterParticipationStatus),
					Value: []string{string(a.partstat)},
				})
			}
			event.AddAttendee(a.email, params...)
		}
		cal.AddVEvent(event)
	}
	return NewMessage(cal)
}

func organizerSender(email string) Sender { return Sender{Email: email} }

func attendeeSender(email string) Sender { return Sender{Email: email} }

func assertDisposition(t *testing.T, res *Result, want Disposition, wantReason Reason) {
	t.Helper()
	if res.Disposition != want || res.Reason != wantReason {
		t.Fatalf("disposition = %s/%s, want %s/%s; detail=%q",
			res.Disposition, res.Reason, want, wantReason, res.Detail)
	}
}

func assertComponent(t *testing.T, res *Result, idx int, want Disposition, wantReason Reason) ComponentResult {
	t.Helper()
	if len(res.Components) <= idx {
		t.Fatalf("result has %d components, want index %d", len(res.Components), idx)
	}
	cr := res.Components[idx]
	if cr.Disposition != want || cr.Reason != wantReason {
		t.Fatalf("component %d = %s/%s, want %s/%s; detail=%q",
			idx, cr.Disposition, cr.Reason, want, wantReason, cr.Detail)
	}
	return cr
}

func masterStart(t *testing.T, store Store, uid string) time.Time {
	t.Helper()
	obj, ok := store.Get(uid)
	if !ok || obj.Master == nil {
		t.Fatalf("no master for %s", uid)
	}
	start, err := obj.Master.Event.GetStartAt()
	if err != nil {
		t.Fatalf("stored master has no start: %v", err)
	}
	return start.UTC()
}

func masterStatus(t *testing.T, store Store, uid string) ical.ObjectStatus {
	t.Helper()
	obj, ok := store.Get(uid)
	if !ok || obj.Master == nil {
		t.Fatalf("no master for %s", uid)
	}
	if prop := obj.Master.Event.GetProperty(ical.ComponentPropertyStatus); prop != nil {
		return ical.ObjectStatus(prop.Value)
	}
	return ""
}

func attendeePartstat(ev *ical.VEvent, email string) ical.ParticipationStatus {
	for _, a := range ev.Attendees() {
		if strings.EqualFold(a.Email(), email) {
			return a.ParticipationStatus()
		}
	}
	return ""
}

const (
	orgEmail = "organizer@example.com"
	attEmail = "attendee@example.com"
	bobEmail = "bob@example.com"
	uid1     = "meeting-1@example.com"
	uidRec   = "recurring-1@example.com"
)

func invitation(uid string, seq int, dtstamp, start, summary string) eventSpec {
	return eventSpec{
		uid:       uid,
		sequence:  seq,
		dtstamp:   dtstamp,
		start:     start,
		end:       start[:8] + "T" + incHour(start[9:]),
		summary:   summary,
		organizer: orgEmail,
		attendees: []attendeeSpec{{email: attEmail, partstat: ical.ParticipationStatusNeedsAction}},
	}
}

func incHour(hhmmssZ string) string {
	tm, err := time.Parse("150405Z", hhmmssZ)
	if err != nil {
		panic("bad test time " + hhmmssZ)
	}
	return tm.Add(time.Hour).Format("150405Z")
}

func reply(uid string, seq int, dtstamp, start string, status ical.ParticipationStatus) eventSpec {
	return eventSpec{
		uid:       uid,
		sequence:  seq,
		dtstamp:   dtstamp,
		start:     start,
		end:       start[:8] + "T" + incHour(start[9:]),
		organizer: orgEmail,
		attendees: []attendeeSpec{{email: attEmail, partstat: status}},
	}
}

func cancelSpec(uid string, seq int, dtstamp, start, recurrenceID string) eventSpec {
	return eventSpec{
		uid:          uid,
		sequence:     seq,
		dtstamp:      dtstamp,
		start:        start,
		end:          start[:8] + "T" + incHour(start[9:]),
		summary:      "Cancelled",
		organizer:    orgEmail,
		status:       ical.ObjectStatusCancelled,
		recurrenceID: recurrenceID,
		attendees:    []attendeeSpec{{email: attEmail}},
	}
}

// TestInterleavedRescheduleOldReplyAndReply exercises the headline out of
// order delivery: reschedule first, then an old invitation, then an
// attendee reply to the old version, and finally the reply to the current
// version.
func TestInterleavedRescheduleOldReplyAndReply(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	originalStart := "20260110T100000Z"
	rescheduledStart := "20260110T120000Z"

	// 1. The reschedule (SEQUENCE 1) arrives before the original invitation.
	rescheduled := invitation(uid1, 1, "20260108T080000Z", rescheduledStart, "Moved")
	res, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, rescheduled))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	if got := masterStart(t, store, uid1); !got.Equal(mustTS(t, rescheduledStart)) {
		t.Fatalf("start = %v, want rescheduled %v", got, rescheduledStart)
	}

	// 2. The old invitation (SEQUENCE 0) turns up late and is ignored.
	oldInvite := invitation(uid1, 0, "20260101T080000Z", originalStart, "Original")
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, oldInvite))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonStale)
	if got := masterStart(t, store, uid1); !got.Equal(mustTS(t, rescheduledStart)) {
		t.Fatalf("old invite changed start to %v", got)
	}

	// 3. An attendee replies to the old SEQUENCE 0 invitation; it is stale.
	oldReply := reply(uid1, 0, "20260102T090000Z", originalStart, ical.ParticipationStatusAccepted)
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodReply, oldReply))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonStale)
	obj, _ := store.Get(uid1)
	if len(obj.Master.Replies) != 0 {
		t.Fatalf("stale reply recorded: %#v", obj.Master.Replies)
	}

	// 4. The reply matching the current revision is applied.
	currentReply := reply(uid1, 1, "20260109T090000Z", rescheduledStart, ical.ParticipationStatusAccepted)
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodReply, currentReply))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	if got := attendeePartstat(obj.Master.Event, attEmail); got != ical.ParticipationStatusAccepted {
		t.Fatalf("partstat = %q, want ACCEPTED", got)
	}
}

// TestFullSeriesCancelInterleaved creates a recurring series, interleaves
// requests and cancels, and checks that only the newest revision survives.
func TestFullSeriesCancelInterleaved(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	initial := invitation(uidRec, 0, "20260101T080000Z", "20260112T100000Z", "Weekly")
	res, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, initial))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)

	// Whole series CANCEL at SEQUENCE 2, skipping 1.
	seriesCancel := cancelSpec(uidRec, 2, "20260105T080000Z", "20260112T100000Z", "")
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodCancel, seriesCancel))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	if masterStatus(t, store, uidRec) != ical.ObjectStatusCancelled {
		t.Fatalf("master status = %q, want CANCELLED", masterStatus(t, store, uidRec))
	}
	obj, _ := store.Get(uidRec)
	if !obj.Master.Cancelled {
		t.Fatal("master Cancelled flag not set")
	}

	// Late REQUEST at SEQUENCE 1 cannot undo the cancel.
	late := invitation(uidRec, 1, "20260103T080000Z", "20260112T100000Z", "Weekly rescheduled")
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, late))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonStale)
	if masterStatus(t, store, uidRec) != ical.ObjectStatusCancelled {
		t.Fatalf("late request resurrected series, status = %q", masterStatus(t, store, uidRec))
	}

	// Redelivery of the same cancel is stable.
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodCancel, seriesCancel))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	if !res.Duplicate {
		t.Fatal("redelivered series cancel not marked duplicate")
	}

	// A newer request resurrects the series.
	revival := invitation(uidRec, 3, "20260106T080000Z", "20260119T100000Z", "Weekly is back")
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, revival))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	if got := masterStatus(t, store, uidRec); got == ical.ObjectStatusCancelled {
		t.Fatalf("series still cancelled after newer request: %q", got)
	}
	if obj, _ = store.Get(uidRec); obj.Master.Cancelled {
		t.Fatal("master Cancelled flag still set after revival")
	}
}

// TestSingleInstanceCancel covers a recurring instance override being
// rescheduled and then cancelled, including stale and unknown instances.
func TestSingleInstanceCancel(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	instanceStart := "20260113T140000Z"
	recurrenceID := "20260113T100000Z"

	// The recurring master is known to the server.
	master := invitation(uidRec, 0, "20260101T080000Z", "20260112T100000Z", "Weekly")
	master.rrule = "FREQ=WEEKLY;COUNT=5"
	res, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, master))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)

	// Organizer sends a per-instance override (single component with
	// RECURRENCE-ID), moving one occurrence.
	override := invitation(uidRec, 0, "20260109T080000Z", instanceStart, "Moved instance")
	override.recurrenceID = recurrenceID
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, override))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)

	// The single instance is cancelled.
	instanceCancel := cancelSpec(uidRec, 1, "20260110T080000Z", instanceStart, recurrenceID)
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodCancel, instanceCancel))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	obj, _ := store.Get(uidRec)
	key := mustTS(t, recurrenceID)
	cs := obj.Instances[key]
	if cs == nil || !cs.Cancelled {
		t.Fatalf("instance not cancelled: %#v", cs)
	}
	if got := ical.ObjectStatus(cs.Event.GetProperty(ical.ComponentPropertyStatus).Value); got != ical.ObjectStatusCancelled {
		t.Fatalf("instance status = %q, want CANCELLED", got)
	}

	// An old SEQUENCE 0 cancel cannot undo it.
	oldInstanceCancel := cancelSpec(uidRec, 0, "20260101T080000Z", instanceStart, recurrenceID)
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodCancel, oldInstanceCancel))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonStale)
	if !obj.Instances[key].Cancelled {
		t.Fatal("stale cancel un-cancelled the instance")
	}

	// Redelivery is stable.
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodCancel, instanceCancel))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	if !res.Duplicate {
		t.Fatal("redelivered instance cancel not marked duplicate")
	}

	// Cancelling an instance the server never tracked is ignored.
	unknown := cancelSpec(uidRec, 5, "20260111T080000Z", "20260120T140000Z", "20260120T100000Z")
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodCancel, unknown))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonInstanceUnknown)
}

// TestDuplicateDeliveryIsStable redelivers requests and replies and asserts
// the outcome and stored state stay the same.
func TestDuplicateDeliveryIsStable(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	invite := invitation(uid1, 0, "20260101T080000Z", "20260110T100000Z", "Meeting")
	msg := buildMessage(t, ical.MethodRequest, invite)
	res, err := proc.Process(organizerSender(orgEmail), msg)
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)

	before := storeDigest(t, store, uid1)
	for i := 0; i < 2; i++ {
		res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, invite))
		if err != nil {
			t.Fatal(err)
		}
		assertDisposition(t, res, DispositionApplied, ReasonNone)
		if !res.Duplicate {
			t.Fatalf("delivery %d not marked duplicate", i+2)
		}
	}
	if after := storeDigest(t, store, uid1); after != before {
		t.Fatal("duplicate request changed stored state")
	}

	accepted := reply(uid1, 0, "20260102T080000Z", "20260110T100000Z", ical.ParticipationStatusAccepted)
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodReply, accepted))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)

	before = storeDigest(t, store, uid1)
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodReply, accepted))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	if !res.Duplicate {
		t.Fatal("duplicate reply not marked duplicate")
	}
	if after := storeDigest(t, store, uid1); after != before {
		t.Fatal("duplicate reply changed stored state")
	}
}

func storeDigest(t *testing.T, store Store, uid string) string {
	t.Helper()
	obj, ok := store.Get(uid)
	if !ok || obj.Master == nil {
		t.Fatal("missing master")
	}
	return obj.Master.Revision.Digest + "|" + canonicalEventText(obj.Master.Event)
}

// TestAuthorizationNeverTrustsICSOrganizer ensures the ORGANIZER property in
// the ICS cannot grant authority; only the verified sender does.
func TestAuthorizationNeverTrustsICSOrganizer(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	invite := invitation(uid1, 0, "20260101T080000Z", "20260110T100000Z", "Meeting")
	res, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, invite))
	if err != nil {
		t.Fatal(err)
	}

	// An attendee forging a newer REQUEST is rejected.
	forged := invitation(uid1, 1, "20260102T080000Z", "20260110T120000Z", "Forged")
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodRequest, forged))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionConflict, ReasonNotOrganizer)

	// A stranger whose ICS still names the real organizer is rejected.
	res, err = proc.Process(attendeeSender(bobEmail), buildMessage(t, ical.MethodRequest, forged))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionConflict, ReasonNotOrganizer)

	// A new event whose authenticated sender disagrees with its ORGANIZER
	// property is rejected.
	foreign := invitation("foreign@example.com", 0, "20260101T080000Z", "20260110T100000Z", "Foreign")
	res, err = proc.Process(attendeeSender(bobEmail), buildMessage(t, ical.MethodRequest, foreign))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionConflict, ReasonOrganizerMismatch)
	if _, ok := store.Get("foreign@example.com"); ok {
		t.Fatal("foreign event with mismatched organizer was stored")
	}

	// A REPLY from a non-attendee is rejected.
	strangerReply := reply(uid1, 0, "20260102T080000Z", "20260110T100000Z", ical.ParticipationStatusDeclined)
	strangerReply.attendees = []attendeeSpec{{email: bobEmail, partstat: ical.ParticipationStatusDeclined}}
	res, err = proc.Process(attendeeSender(bobEmail), buildMessage(t, ical.MethodReply, strangerReply))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionConflict, ReasonNotAttendee)

	// A CANCEL from an attendee is rejected.
	cancel := cancelSpec(uid1, 1, "20260103T080000Z", "20260110T100000Z", "")
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodCancel, cancel))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionConflict, ReasonNotOrganizer)
}

// TestSameSequenceDifferentContentConflicts sends two revisions with equal
// SEQUENCE and DTSTAMP but different times.
func TestSameSequenceDifferentContentConflicts(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	first := invitation(uid1, 1, "20260101T080000Z", "20260110T100000Z", "Room A")
	res, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, first))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)

	second := invitation(uid1, 1, "20260101T080000Z", "20260111T100000Z", "Room B")
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, second))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionConflict, ReasonSameTimestamp)
}

// TestUnknownObjectRepliesAndCancels verifies messages for unknown UIDs are
// ignored rather than creating phantom state.
func TestUnknownObjectRepliesAndCancels(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	unknownCancel := cancelSpec("never@example.com", 0, "20260101T080000Z", "20260110T100000Z", "")
	res, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodCancel, unknownCancel))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonObjectUnknown)

	unknownReply := reply("never@example.com", 0, "20260101T080000Z", "20260110T100000Z", ical.ParticipationStatusAccepted)
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodReply, unknownReply))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonObjectUnknown)

	instanceOnly := cancelSpec(uidRec, 0, "20260101T080000Z", "20260113T140000Z", "20260113T100000Z")
	res, err = proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodCancel, instanceOnly))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonObjectUnknown)
}

// TestReplySequenceKeepsLatestAttendeeWord sends TENTATIVE then an older
// ACCEPTED then a newer DECLINED.
func TestReplySequenceKeepsLatestAttendeeWord(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	invite := invitation(uid1, 2, "20260105T080000Z", "20260110T100000Z", "Meeting")
	if _, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, invite)); err != nil {
		t.Fatal(err)
	}

	tentative := reply(uid1, 2, "20260106T080000Z", "20260110T100000Z", ical.ParticipationStatusTentative)
	res, err := proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodReply, tentative))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)

	// Same SEQUENCE, older DTSTAMP: stale.
	older := reply(uid1, 2, "20260105T200000Z", "20260110T100000Z", ical.ParticipationStatusAccepted)
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodReply, older))
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionIgnored, ReasonStale)
	obj, _ := store.Get(uid1)
	if got := attendeePartstat(obj.Master.Event, attEmail); got != ical.ParticipationStatusTentative {
		t.Fatalf("partstat overwritten by stale reply: %q", got)
	}

	// Same SEQUENCE and DTSTAMP, identical bytes: duplicate.
	res, err = proc.Process(attendeeSender(attEmail), buildMessage(t, ical.MethodReply, tentative))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate {
		t.Fatal("identical reply not marked duplicate")
	}
}

// TestMultiComponentAggregation sends one calendar with an applied master and
// a stale instance and checks CONFLICT/APPLIED/IGNORED precedence.
func TestMultiComponentAggregation(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	master := invitation(uidRec, 1, "20260102T080000Z", "20260112T100000Z", "Weekly")
	if _, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, master)); err != nil {
		t.Fatal(err)
	}
	instanceOverride := invitation(uidRec, 1, "20260102T080000Z", "20260113T140000Z", "Moved")
	instanceOverride.recurrenceID = "20260113T100000Z"
	if _, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, instanceOverride)); err != nil {
		t.Fatal(err)
	}

	newMaster := invitation(uidRec, 2, "20260103T080000Z", "20260112T100000Z", "Weekly 2")
	staleInstance := invitation(uidRec, 0, "20260101T080000Z", "20260113T140000Z", "Old")
	staleInstance.recurrenceID = "20260113T100000Z"

	res, err := proc.Process(organizerSender(orgEmail), buildMessage(t, ical.MethodRequest, newMaster, staleInstance))
	if err != nil {
		t.Fatal(err)
	}
	if res.Disposition != DispositionApplied {
		t.Fatalf("aggregate = %s, want APPLIED (one applied, one ignored)", res.Disposition)
	}
	assertComponent(t, res, 0, DispositionApplied, ReasonNone)
	assertComponent(t, res, 1, DispositionIgnored, ReasonStale)
}

// TestMalformedAndUnsupported covers missing identity, unknown method and
// missing UID/DTSTAMP.
func TestMalformedAndUnsupported(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	if _, err := proc.Process(Sender{}, buildMessage(t, ical.MethodRequest,
		invitation(uid1, 0, "20260101T080000Z", "20260110T100000Z", "M"))); err != ErrNoSender {
		t.Fatalf("want ErrNoSender, got %v", err)
	}

	msg := buildMessage(t, ical.MethodRefresh,
		invitation(uid1, 0, "20260101T080000Z", "20260110T100000Z", "M"))
	res, err := proc.Process(organizerSender(orgEmail), msg)
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionConflict, ReasonUnsupportedMethod)

	cal := ical.NewCalendar()
	cal.SetMethod(ical.MethodRequest)
	ev := ical.NewEvent("")
	ev.SetDtStampTime(mustTS(t, "20260101T080000Z"))
	cal.AddVEvent(ev)
	res, err = proc.Process(organizerSender(orgEmail), NewMessage(cal))
	if err != nil {
		t.Fatal(err)
	}
	assertComponent(t, res, 0, DispositionConflict, ReasonMalformed)

	cal = ical.NewCalendar()
	cal.SetMethod(ical.MethodRequest)
	ev = ical.NewEvent(uid1)
	ev.SetSequence(0)
	ev.SetStartAt(mustTS(t, "20260110T100000Z"))
	cal.AddVEvent(ev)
	res, err = proc.Process(organizerSender(orgEmail), NewMessage(cal))
	if err != nil {
		t.Fatal(err)
	}
	assertComponent(t, res, 0, DispositionConflict, ReasonMalformed)
}

// TestParseMessageEndToEnd feeds raw text through ParseMessage and merges it.
func TestParseMessageEndToEnd(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//test//EN\r\n" +
		"METHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:raw-1@example.com\r\n" +
		"DTSTAMP:20260101T080000Z\r\n" +
		"SEQUENCE:0\r\n" +
		"DTSTART:20260110T100000Z\r\n" +
		"DTEND:20260110T110000Z\r\n" +
		"SUMMARY:Raw\r\n" +
		"ORGANIZER:mailto:organizer@example.com\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:attendee@example.com\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	msg, err := ParseMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != ical.MethodRequest {
		t.Fatalf("method = %q", msg.Method)
	}
	store := NewMemoryStore()
	res, err := NewProcessor(store).Process(organizerSender(orgEmail), msg)
	if err != nil {
		t.Fatal(err)
	}
	assertDisposition(t, res, DispositionApplied, ReasonNone)
	if obj, ok := store.Get("raw-1@example.com"); !ok || obj.Organizer != orgEmail {
		t.Fatalf("stored organizer wrong: %#v", obj)
	}
}
