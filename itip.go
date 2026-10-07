package ics

import (
	"fmt"
	"strconv"
	"strings"
)

// ITIPVerdict describes the outcome of processing a single iTIP (RFC 5546)
// message component against the stored calendar state.
type ITIPVerdict int

const (
	// ITIPVerdictApplied means the message was newer than the stored state and
	// has been merged into it.
	ITIPVerdictApplied ITIPVerdict = iota
	// ITIPVerdictIgnored means the message was stale, a duplicate, or a no-op
	// and the stored state was left untouched.
	ITIPVerdictIgnored
	// ITIPVerdictConflict means the message could not be reconciled with the
	// stored state, e.g. the sender is not authorized for the change or the
	// referenced component does not exist.
	ITIPVerdictConflict
)

func (v ITIPVerdict) String() string {
	switch v {
	case ITIPVerdictApplied:
		return "applied"
	case ITIPVerdictIgnored:
		return "ignored"
	case ITIPVerdictConflict:
		return "conflict"
	}
	return "unknown"
}

// ITIPResult is the per-component outcome of ProcessITIPMessage. Reason always
// carries a human readable explanation of why the verdict was reached.
type ITIPResult struct {
	Method       Method
	UID          string
	RecurrenceID string
	Verdict      ITIPVerdict
	Reason       string
}

// ProcessITIPMessage merges an incoming iTIP message (a *Calendar carrying a
// METHOD property) into the receiving calendar, which represents the
// server-side schedule state.
//
// verifiedSender is the already authenticated identity of the sender (a
// cal-address such as "mailto:boss@example.com" or a bare address), supplied
// by the caller. Authorization is always checked against the *stored* state:
// the ORGANIZER/ATTENDEE fields inside the message itself are never trusted
// to grant privileges. Only the organizer may apply REQUEST and CANCEL
// changes; an attendee may only update their own PARTSTAT via REPLY.
//
// Freshness is decided per component by UID + RECURRENCE-ID using SEQUENCE
// first and DTSTAMP as a tie-breaker, so messages that arrive out of order
// or are delivered twice converge to a stable state.
//
// Supported methods are REQUEST, REPLY and CANCEL. One result is returned per
// VEVENT contained in the message.
func (cal *Calendar) ProcessITIPMessage(msg *Calendar, verifiedSender string) []*ITIPResult {
	var results []*ITIPResult
	method := itipMessageMethod(msg)
	for _, ev := range msg.Events() {
		res := &ITIPResult{
			Method:       method,
			UID:          ev.Id(),
			RecurrenceID: itipRecurrenceIDValue(ev),
		}
		switch {
		case method == "":
			res.Verdict = ITIPVerdictConflict
			res.Reason = "message has no METHOD property"
		case res.UID == "":
			res.Verdict = ITIPVerdictConflict
			res.Reason = "VEVENT has no UID"
		default:
			switch method {
			case MethodRequest:
				cal.itipApplyRequest(ev, verifiedSender, res)
			case MethodReply:
				cal.itipApplyReply(ev, verifiedSender, res)
			case MethodCancel:
				cal.itipApplyCancel(ev, verifiedSender, res)
			default:
				res.Verdict = ITIPVerdictConflict
				res.Reason = fmt.Sprintf("unsupported METHOD %q", method)
			}
		}
		results = append(results, res)
	}
	return results
}

// itipApplyRequest handles METHOD:REQUEST. Only the organizer of the stored
// event may create or replace components.
func (cal *Calendar) itipApplyRequest(ev *VEvent, sender string, res *ITIPResult) {
	stored := cal.itipFindEvent(res.UID, res.RecurrenceID)
	if stored == nil {
		// Initial invitation: there is no stored state to authorize against, so
		// the sender must at least match the organizer the message claims.
		if org := itipOrganizerOf(ev); org != "" && !itipSameAddress(org, sender) {
			res.Verdict = ITIPVerdictConflict
			res.Reason = "sender does not match the organizer of the new event"
			return
		}
		cal.Components = append(cal.Components, ev)
		res.Verdict = ITIPVerdictApplied
		res.Reason = "new event stored"
		return
	}
	if !cal.itipIsOrganizer(res.UID, stored, sender) {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "sender is not the organizer of the stored event"
		return
	}
	if stale, reason := itipCheckStale(ev, stored); stale {
		res.Verdict = ITIPVerdictIgnored
		res.Reason = reason
		return
	}
	cal.itipReplaceEvent(stored, ev)
	res.Verdict = ITIPVerdictApplied
	res.Reason = "stored event replaced by newer revision"
}

// itipApplyReply handles METHOD:REPLY. An attendee may only update the
// PARTSTAT of their own ATTENDEE entry; all other content is organizer
// controlled and is never taken from a reply.
func (cal *Calendar) itipApplyReply(ev *VEvent, sender string, res *ITIPResult) {
	stored := cal.itipFindEvent(res.UID, res.RecurrenceID)
	if stored == nil && res.RecurrenceID != "" {
		// Replies to a single instance still update the attendee list on the
		// master component when no instance override is stored.
		stored = cal.itipFindEvent(res.UID, "")
	}
	if stored == nil {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "no stored event matches the reply UID"
		return
	}
	storedAttendee := itipFindAttendee(stored, sender)
	if storedAttendee == nil {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "sender is not an attendee of the stored event"
		return
	}
	replyAttendee := itipFindAttendee(ev, sender)
	if replyAttendee == nil {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "reply contains no ATTENDEE entry for the sender"
		return
	}
	if stale, reason := itipCheckStale(ev, stored); stale {
		res.Verdict = ITIPVerdictIgnored
		res.Reason = reason
		return
	}
	newPartstat := replyAttendee.ParticipationStatus()
	if newPartstat == "" {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "reply ATTENDEE entry has no PARTSTAT"
		return
	}
	if storedAttendee.ParticipationStatus() == newPartstat {
		res.Verdict = ITIPVerdictIgnored
		res.Reason = "participant status already up to date (duplicate delivery)"
		return
	}
	for i := range stored.Properties {
		if stored.Properties[i].IANAToken == string(ComponentPropertyAttendee) &&
			itipSameAddress(stored.Properties[i].Value, sender) {
			stored.Properties[i].ICalParameters[string(ParameterParticipationStatus)] = []string{string(newPartstat)}
		}
	}
	res.Verdict = ITIPVerdictApplied
	res.Reason = fmt.Sprintf("participant status updated to %s", newPartstat)
}

// itipApplyCancel handles METHOD:CANCEL. Only the organizer may cancel. A
// cancel without RECURRENCE-ID cancels the whole series; with RECURRENCE-ID
// it cancels a single instance.
func (cal *Calendar) itipApplyCancel(ev *VEvent, sender string, res *ITIPResult) {
	if res.RecurrenceID == "" {
		cal.itipCancelSeries(ev, sender, res)
		return
	}
	cal.itipCancelInstance(ev, sender, res)
}

func (cal *Calendar) itipCancelSeries(ev *VEvent, sender string, res *ITIPResult) {
	var targets []*VEvent
	for _, e := range cal.Events() {
		if e.Id() == res.UID {
			targets = append(targets, e)
		}
	}
	if len(targets) == 0 {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "no stored event matches the cancel UID"
		return
	}
	reference := cal.itipFindEvent(res.UID, "")
	if reference == nil {
		reference = targets[0]
	}
	if !cal.itipIsOrganizer(res.UID, reference, sender) {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "sender is not the organizer of the stored event"
		return
	}
	if stale, reason := itipCheckStale(ev, reference); stale {
		res.Verdict = ITIPVerdictIgnored
		res.Reason = reason
		return
	}
	for _, e := range targets {
		e.SetStatus(ObjectStatusCancelled)
		itipStampRevision(e, ev)
	}
	res.Verdict = ITIPVerdictApplied
	res.Reason = fmt.Sprintf("series cancelled (%d component(s) marked CANCELLED)", len(targets))
}

func (cal *Calendar) itipCancelInstance(ev *VEvent, sender string, res *ITIPResult) {
	stored := cal.itipFindEvent(res.UID, res.RecurrenceID)
	master := cal.itipFindEvent(res.UID, "")
	reference := stored
	if reference == nil {
		reference = master
	}
	if reference == nil {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "no stored event matches the cancel UID"
		return
	}
	if !cal.itipIsOrganizer(res.UID, reference, sender) {
		res.Verdict = ITIPVerdictConflict
		res.Reason = "sender is not the organizer of the stored event"
		return
	}
	if stale, reason := itipCheckStale(ev, reference); stale {
		res.Verdict = ITIPVerdictIgnored
		res.Reason = reason
		return
	}
	if stored != nil {
		stored.SetStatus(ObjectStatusCancelled)
		itipStampRevision(stored, ev)
		res.Verdict = ITIPVerdictApplied
		res.Reason = "instance marked CANCELLED"
		return
	}
	// No stored override for this instance: record an EXDATE on the master.
	if itipHasExdate(master, res.RecurrenceID) {
		res.Verdict = ITIPVerdictIgnored
		res.Reason = "instance already excluded from the series (duplicate delivery)"
		return
	}
	var params []PropertyParameter
	if rid := ev.GetProperty(ComponentPropertyRecurrenceId); rid != nil {
		if tzid := rid.ICalParameters[string(ParameterTzid)]; len(tzid) > 0 {
			params = append(params, WithTZID(tzid[0]))
		}
	}
	master.AddExdate(res.RecurrenceID, params...)
	itipStampRevision(master, ev)
	res.Verdict = ITIPVerdictApplied
	res.Reason = "instance excluded from series via EXDATE"
}

// itipStampRevision advances the stored component's SEQUENCE and DTSTAMP to
// the incoming revision so that re-delivery of the same message is detected
// as a duplicate.
func itipStampRevision(stored, incoming *VEvent) {
	if p := incoming.GetProperty(ComponentPropertySequence); p != nil {
		stored.SetProperty(ComponentPropertySequence, p.Value)
	}
	if p := incoming.GetProperty(ComponentPropertyDtstamp); p != nil {
		stored.SetProperty(ComponentPropertyDtstamp, p.Value)
	}
}

func itipHasExdate(master *VEvent, value string) bool {
	for _, p := range master.GetProperties(ComponentPropertyExdate) {
		for _, v := range strings.Split(p.Value, ",") {
			if strings.TrimSpace(v) == value {
				return true
			}
		}
	}
	return false
}

// itipCheckStale reports whether the incoming component is older than (or a
// duplicate of) the stored one, comparing SEQUENCE first and DTSTAMP as the
// tie-breaker for equal sequences.
func itipCheckStale(incoming, stored *VEvent) (bool, string) {
	inSeq := itipSequenceOf(incoming)
	storedSeq := itipSequenceOf(stored)
	if inSeq < storedSeq {
		return true, fmt.Sprintf("incoming SEQUENCE %d is older than stored SEQUENCE %d", inSeq, storedSeq)
	}
	if inSeq == storedSeq {
		inStamp, inErr := incoming.GetDtStampTime()
		storedStamp, storedErr := stored.GetDtStampTime()
		if inErr == nil && storedErr == nil && !inStamp.After(storedStamp) {
			return true, "incoming DTSTAMP is not newer than the stored one (duplicate or stale)"
		}
	}
	return false, ""
}

func itipSequenceOf(ev *VEvent) int {
	p := ev.GetProperty(ComponentPropertySequence)
	if p == nil {
		return 0
	}
	seq, err := strconv.Atoi(strings.TrimSpace(p.Value))
	if err != nil {
		return 0
	}
	return seq
}

func itipMessageMethod(cal *Calendar) Method {
	for _, p := range cal.CalendarProperties {
		if p.IANAToken == string(PropertyMethod) {
			return Method(p.Value)
		}
	}
	return ""
}

func itipRecurrenceIDValue(ev *VEvent) string {
	p := ev.GetProperty(ComponentPropertyRecurrenceId)
	if p == nil {
		return ""
	}
	return p.Value
}

// itipFindEvent locates a stored VEVENT by UID and RECURRENCE-ID value. An
// empty recurrenceID matches the master component (the one without a
// RECURRENCE-ID).
func (cal *Calendar) itipFindEvent(uid, recurrenceID string) *VEvent {
	for _, e := range cal.Events() {
		if e.Id() == uid && itipRecurrenceIDValue(e) == recurrenceID {
			return e
		}
	}
	return nil
}

func (cal *Calendar) itipReplaceEvent(stored, incoming *VEvent) {
	for i, c := range cal.Components {
		if c == Component(stored) {
			cal.Components[i] = incoming
			return
		}
	}
	cal.Components = append(cal.Components, incoming)
}

func itipOrganizerOf(ev *VEvent) string {
	p := ev.GetProperty(ComponentPropertyOrganizer)
	if p == nil {
		return ""
	}
	return p.Value
}

// itipIsOrganizer authorizes sender against the stored state: the component's
// own ORGANIZER if present, otherwise the master component's ORGANIZER.
func (cal *Calendar) itipIsOrganizer(uid string, reference *VEvent, sender string) bool {
	if org := itipOrganizerOf(reference); org != "" {
		return itipSameAddress(org, sender)
	}
	if reference != nil && itipRecurrenceIDValue(reference) != "" {
		if master := cal.itipFindEvent(uid, ""); master != nil {
			if org := itipOrganizerOf(master); org != "" {
				return itipSameAddress(org, sender)
			}
		}
	}
	return false
}

func itipFindAttendee(ev *VEvent, address string) *Attendee {
	for _, a := range ev.Attendees() {
		if itipSameAddress(a.Value, address) {
			return a
		}
	}
	return nil
}

// itipSameAddress compares two cal-addresses, ignoring the mailto: scheme and
// case. Either side may be a bare address or a mailto: URI.
func itipSameAddress(a, b string) bool {
	return itipNormalizeAddress(a) == itipNormalizeAddress(b)
}

func itipNormalizeAddress(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "mailto:") {
		s = s[len("mailto:"):]
	}
	return strings.ToLower(s)
}
