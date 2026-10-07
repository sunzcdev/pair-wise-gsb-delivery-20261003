package ics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	itipOrganizer = "boss@example.com"
	itipAttendeeA = "alice@example.com"
	itipAttendeeB = "bob@example.com"
	itipUID       = "meeting-123@example.com"
)

func itipStamp(hour int) time.Time {
	return time.Date(2026, 10, 6, hour, 0, 0, 0, time.UTC)
}

// itipRequestEvent builds one revision of the meeting invitation.
func itipRequestEvent(uid, summary string, seq int, stamp time.Time) *VEvent {
	e := NewEvent(uid)
	e.SetDtStampTime(stamp)
	e.SetSequence(seq)
	e.SetSummary(summary)
	e.SetOrganizer(itipOrganizer)
	e.AddAttendee(itipAttendeeA, ParticipationStatusNeedsAction)
	e.AddAttendee(itipAttendeeB, ParticipationStatusNeedsAction)
	e.SetStartAt(time.Date(2026, 10, 10, 9+seq, 0, 0, 0, time.UTC))
	return e
}

// itipReplyEvent builds an attendee reply for the given revision.
func itipReplyEvent(uid, attendee string, partstat ParticipationStatus, seq int, stamp time.Time) *VEvent {
	e := NewEvent(uid)
	e.SetDtStampTime(stamp)
	e.SetSequence(seq)
	e.SetOrganizer(itipOrganizer)
	e.AddAttendee(attendee, partstat)
	return e
}

func itipMessage(method Method, events ...*VEvent) *Calendar {
	cal := NewCalendar()
	cal.SetMethod(method)
	for _, e := range events {
		cal.Components = append(cal.Components, e)
	}
	return cal
}

func itipSingleResult(t *testing.T, store *Calendar, msg *Calendar, sender string) *ITIPResult {
	t.Helper()
	results := store.ProcessITIPMessage(msg, sender)
	require.Len(t, results, 1)
	return results[0]
}

func itipStoredSummary(t *testing.T, store *Calendar, uid string) string {
	t.Helper()
	e := store.itipFindEvent(uid, "")
	require.NotNil(t, e)
	p := e.GetProperty(ComponentPropertySummary)
	require.NotNil(t, p)
	return p.Value
}

func itipStoredPartstat(t *testing.T, store *Calendar, uid, attendee string) ParticipationStatus {
	t.Helper()
	e := store.itipFindEvent(uid, "")
	require.NotNil(t, e)
	a := itipFindAttendee(e, attendee)
	require.NotNil(t, a)
	return a.ParticipationStatus()
}

func TestItipRequestOutOfOrderDelivery(t *testing.T) {
	store := NewCalendar()

	// The reschedule (SEQUENCE 1) arrives before the original invite (SEQUENCE 0).
	res := itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "rescheduled", 1, itipStamp(10))), itipOrganizer)
	assert.Equal(t, ITIPVerdictApplied, res.Verdict)

	res = itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "original", 0, itipStamp(9))), itipOrganizer)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)
	assert.Contains(t, res.Reason, "SEQUENCE")
	assert.Equal(t, "rescheduled", itipStoredSummary(t, store, itipUID))
}

func TestItipRequestSameSequenceDecidedByDtstamp(t *testing.T) {
	store := NewCalendar()

	res := itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "newer stamp", 1, itipStamp(11))), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	// Same sequence but older DTSTAMP loses.
	res = itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "older stamp", 1, itipStamp(10))), itipOrganizer)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)
	assert.Equal(t, "newer stamp", itipStoredSummary(t, store, itipUID))

	// Same sequence, newer DTSTAMP wins.
	res = itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "even newer", 1, itipStamp(12))), itipOrganizer)
	assert.Equal(t, ITIPVerdictApplied, res.Verdict)
	assert.Equal(t, "even newer", itipStoredSummary(t, store, itipUID))
}

func TestItipRequestDuplicateDeliveryIsStable(t *testing.T) {
	store := NewCalendar()
	msg := itipMessage(MethodRequest, itipRequestEvent(itipUID, "original", 0, itipStamp(9)))

	first := itipSingleResult(t, store, msg, itipOrganizer)
	assert.Equal(t, ITIPVerdictApplied, first.Verdict)

	second := itipSingleResult(t, store, msg, itipOrganizer)
	assert.Equal(t, ITIPVerdictIgnored, second.Verdict)
	assert.Equal(t, "original", itipStoredSummary(t, store, itipUID))
	assert.Len(t, store.Events(), 1)
}

func TestItipRequestRequiresOrganizer(t *testing.T) {
	store := NewCalendar()
	res := itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "original", 0, itipStamp(9))), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	// An attendee cannot push a new revision, even if the message itself
	// claims them as ORGANIZER.
	forged := itipRequestEvent(itipUID, "forged", 2, itipStamp(12))
	forged.SetOrganizer(itipAttendeeA)
	res = itipSingleResult(t, store, itipMessage(MethodRequest, forged), itipAttendeeA)
	assert.Equal(t, ITIPVerdictConflict, res.Verdict)
	assert.Contains(t, res.Reason, "organizer")
	assert.Equal(t, "original", itipStoredSummary(t, store, itipUID))
}

func TestItipReplyAppliedAndStaleReplyIgnored(t *testing.T) {
	store := NewCalendar()
	res := itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "rescheduled", 1, itipStamp(10))), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	// A reply written against the old SEQUENCE 0 invite arrives late.
	res = itipSingleResult(t, store, itipMessage(MethodReply, itipReplyEvent(itipUID, itipAttendeeA, ParticipationStatusAccepted, 0, itipStamp(9))), itipAttendeeA)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)
	assert.Equal(t, ParticipationStatusNeedsAction, itipStoredPartstat(t, store, itipUID, itipAttendeeA))

	// A current reply is merged into the stored attendee list.
	res = itipSingleResult(t, store, itipMessage(MethodReply, itipReplyEvent(itipUID, itipAttendeeA, ParticipationStatusAccepted, 1, itipStamp(11))), itipAttendeeA)
	assert.Equal(t, ITIPVerdictApplied, res.Verdict)
	assert.Equal(t, ParticipationStatusAccepted, itipStoredPartstat(t, store, itipUID, itipAttendeeA))
	// Other attendees are untouched.
	assert.Equal(t, ParticipationStatusNeedsAction, itipStoredPartstat(t, store, itipUID, itipAttendeeB))

	// Re-delivering the same reply is a stable no-op.
	res = itipSingleResult(t, store, itipMessage(MethodReply, itipReplyEvent(itipUID, itipAttendeeA, ParticipationStatusAccepted, 1, itipStamp(11))), itipAttendeeA)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)
	assert.Equal(t, ParticipationStatusAccepted, itipStoredPartstat(t, store, itipUID, itipAttendeeA))
}

func TestItipReplyCannotModifyOrganizerContent(t *testing.T) {
	store := NewCalendar()
	res := itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "original", 1, itipStamp(10))), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	// The reply tries to smuggle in a summary change; only PARTSTAT may change.
	reply := itipReplyEvent(itipUID, itipAttendeeA, ParticipationStatusDeclined, 1, itipStamp(11))
	reply.SetSummary("hijacked")
	res = itipSingleResult(t, store, itipMessage(MethodReply, reply), itipAttendeeA)
	assert.Equal(t, ITIPVerdictApplied, res.Verdict)
	assert.Equal(t, "original", itipStoredSummary(t, store, itipUID))
	assert.Equal(t, ParticipationStatusDeclined, itipStoredPartstat(t, store, itipUID, itipAttendeeA))
}

func TestItipReplyRequiresAttendee(t *testing.T) {
	store := NewCalendar()
	res := itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "original", 0, itipStamp(9))), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	// The organizer is not an attendee and cannot reply.
	res = itipSingleResult(t, store, itipMessage(MethodReply, itipReplyEvent(itipUID, itipOrganizer, ParticipationStatusAccepted, 0, itipStamp(10))), itipOrganizer)
	assert.Equal(t, ITIPVerdictConflict, res.Verdict)

	// A stranger cannot reply either.
	res = itipSingleResult(t, store, itipMessage(MethodReply, itipReplyEvent(itipUID, "mallory@example.com", ParticipationStatusAccepted, 0, itipStamp(10))), "mallory@example.com")
	assert.Equal(t, ITIPVerdictConflict, res.Verdict)

	// A reply for an unknown UID conflicts.
	res = itipSingleResult(t, store, itipMessage(MethodReply, itipReplyEvent("other@example.com", itipAttendeeA, ParticipationStatusAccepted, 0, itipStamp(10))), itipAttendeeA)
	assert.Equal(t, ITIPVerdictConflict, res.Verdict)
}

func TestItipCancelWholeSeries(t *testing.T) {
	store := NewCalendar()
	master := itipRequestEvent(itipUID, "weekly sync", 1, itipStamp(10))
	master.AddRrule("FREQ=WEEKLY")
	res := itipSingleResult(t, store, itipMessage(MethodRequest, master), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	// Cancel the whole series.
	cancel := NewEvent(itipUID)
	cancel.SetDtStampTime(itipStamp(12))
	cancel.SetSequence(2)
	cancel.SetOrganizer(itipOrganizer)
	res = itipSingleResult(t, store, itipMessage(MethodCancel, cancel), itipOrganizer)
	assert.Equal(t, ITIPVerdictApplied, res.Verdict)

	stored := store.itipFindEvent(itipUID, "")
	require.NotNil(t, stored)
	assert.Equal(t, string(ObjectStatusCancelled), stored.GetProperty(ComponentPropertyStatus).Value)

	// Duplicate cancel is ignored and the state stays stable.
	res = itipSingleResult(t, store, itipMessage(MethodCancel, cancel), itipOrganizer)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)

	// A late, older REQUEST must not resurrect the event.
	res = itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "zombie", 1, itipStamp(10))), itipOrganizer)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)
	assert.Equal(t, string(ObjectStatusCancelled), store.itipFindEvent(itipUID, "").GetProperty(ComponentPropertyStatus).Value)
}

func TestItipCancelSingleInstance(t *testing.T) {
	store := NewCalendar()
	master := itipRequestEvent(itipUID, "weekly sync", 1, itipStamp(10))
	master.AddRrule("FREQ=WEEKLY")
	res := itipSingleResult(t, store, itipMessage(MethodRequest, master), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	// Cancel one occurrence only.
	cancel := NewEvent(itipUID)
	cancel.SetDtStampTime(itipStamp(11))
	cancel.SetSequence(1)
	cancel.SetOrganizer(itipOrganizer)
	cancel.SetProperty(ComponentPropertyRecurrenceId, "20261017T090000Z")
	res = itipSingleResult(t, store, itipMessage(MethodCancel, cancel), itipOrganizer)
	assert.Equal(t, ITIPVerdictApplied, res.Verdict)

	stored := store.itipFindEvent(itipUID, "")
	require.NotNil(t, stored)
	exdates, err := stored.GetExDates()
	require.NoError(t, err)
	require.Len(t, exdates, 1)
	assert.Equal(t, time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC), exdates[0])
	// The series itself is not cancelled.
	assert.Nil(t, stored.GetProperty(ComponentPropertyStatus))

	// Duplicate single-instance cancel is stable.
	res = itipSingleResult(t, store, itipMessage(MethodCancel, cancel), itipOrganizer)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)
	exdates, err = store.itipFindEvent(itipUID, "").GetExDates()
	require.NoError(t, err)
	assert.Len(t, exdates, 1)
}

func TestItipCancelRequiresOrganizer(t *testing.T) {
	store := NewCalendar()
	res := itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "original", 0, itipStamp(9))), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	cancel := NewEvent(itipUID)
	cancel.SetDtStampTime(itipStamp(10))
	cancel.SetSequence(1)
	cancel.SetOrganizer(itipOrganizer)
	res = itipSingleResult(t, store, itipMessage(MethodCancel, cancel), itipAttendeeA)
	assert.Equal(t, ITIPVerdictConflict, res.Verdict)
	assert.Nil(t, store.itipFindEvent(itipUID, "").GetProperty(ComponentPropertyStatus))
}

func TestItipInterleavedDelivery(t *testing.T) {
	store := NewCalendar()

	// 1. Reschedule arrives before the original invite.
	res := itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "rescheduled", 1, itipStamp(10))), itipOrganizer)
	require.Equal(t, ITIPVerdictApplied, res.Verdict)

	// 2. The original invite arrives late and is dropped.
	res = itipSingleResult(t, store, itipMessage(MethodRequest, itipRequestEvent(itipUID, "original", 0, itipStamp(9))), itipOrganizer)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)

	// 3. An attendee reply against the old invite is dropped as stale.
	res = itipSingleResult(t, store, itipMessage(MethodReply, itipReplyEvent(itipUID, itipAttendeeA, ParticipationStatusAccepted, 0, itipStamp(9))), itipAttendeeA)
	assert.Equal(t, ITIPVerdictIgnored, res.Verdict)

	// 4. The attendee replies to the current revision.
	res = itipSingleResult(t, store, itipMessage(MethodReply, itipReplyEvent(itipUID, itipAttendeeA, ParticipationStatusTentative, 1, itipStamp(11))), itipAttendeeA)
	assert.Equal(t, ITIPVerdictApplied, res.Verdict)

	// 5. The organizer cancels the whole series.
	cancel := NewEvent(itipUID)
	cancel.SetDtStampTime(itipStamp(12))
	cancel.SetSequence(2)
	cancel.SetOrganizer(itipOrganizer)
	res = itipSingleResult(t, store, itipMessage(MethodCancel, cancel), itipOrganizer)
	assert.Equal(t, ITIPVerdictApplied, res.Verdict)

	stored := store.itipFindEvent(itipUID, "")
	require.NotNil(t, stored)
	assert.Equal(t, "rescheduled", stored.GetProperty(ComponentPropertySummary).Value)
	assert.Equal(t, string(ObjectStatusCancelled), stored.GetProperty(ComponentPropertyStatus).Value)
	assert.Equal(t, ParticipationStatusTentative, itipStoredPartstat(t, store, itipUID, itipAttendeeA))
}
