package itip

import (
	"strconv"
	"time"

	ical "github.com/arran4/golang-ical"
)

// validAttendeePartstats lists the VEVENT PARTSTAT values a REPLY may carry.
var validAttendeePartstats = map[ical.ParticipationStatus]bool{
	ical.ParticipationStatusNeedsAction: true,
	ical.ParticipationStatusAccepted:    true,
	ical.ParticipationStatusDeclined:    true,
	ical.ParticipationStatusTentative:   true,
	ical.ParticipationStatusDelegated:   true,
}

// mergeReplies applies stored replies onto ev. Attendees that no longer exist
// are dropped from the returned map.
func mergeReplies(ev *ical.VEvent, replies map[string]ReplyState) map[string]ReplyState {
	kept := map[string]ReplyState{}
	for _, attendee := range ev.Attendees() {
		email := normalizeEmail(attendee.Email())
		if rs, ok := replies[email]; ok {
			setAttendeePartstat(ev, email, rs.Status)
			kept[email] = rs
		}
	}
	return kept
}

// setAttendeePartstat replaces the PARTSTAT parameter of the first attendee
// property matching email, preserving all other parameters.
func setAttendeePartstat(ev *ical.VEvent, email string, status ical.ParticipationStatus) {
	props := ev.UnknownPropertiesIANAProperties()
	for i := range props {
		if props[i].IANAToken != string(ical.ComponentPropertyAttendee) {
			continue
		}
		if normalizeEmail(props[i].Value) != email {
			continue
		}
		params := props[i].ICalParameters
		if params == nil {
			params = map[string][]string{}
		}
		params[string(ical.ParameterParticipationStatus)] = []string{string(status)}
		props[i].ICalParameters = params
		return
	}
}

func findAttendee(ev *ical.VEvent, email string) (*ical.Attendee, bool) {
	for _, attendee := range ev.Attendees() {
		if normalizeEmail(attendee.Email()) == email {
			return attendee, true
		}
	}
	return nil, false
}

func (p *Processor) applyRequest(
	sender Sender,
	uid string,
	recID time.Time,
	hasRecurrenceID bool,
	event *ical.VEvent,
	incoming Revision,
	cr ComponentResult,
) ComponentResult {
	obj, exists := p.store.Get(uid)

	if !exists {
		if hasRecurrenceID {
			return ignored(cr, ReasonObjectUnknown, "instance REQUEST for unknown UID")
		}
		if declared := organizerEmail(event); declared != "" && declared != sender.Email {
			return conflict(cr, ReasonOrganizerMismatch,
				"ORGANIZER property "+declared+" does not match authenticated sender "+sender.Email)
		}
		cloned, err := cloneEvent(event)
		if err != nil {
			return conflict(cr, ReasonMalformed, "cannot store event: "+err.Error())
		}
		obj = newObject(uid, sender.Email)
		obj.Master = newComponentState(time.Time{}, false, cloned, incoming)
		p.store.Put(obj)
		return applied(cr, "new master event created")
	}

	if sender.Email != obj.Organizer {
		return conflict(cr, ReasonNotOrganizer, "sender "+sender.Email+" is not organizer "+obj.Organizer)
	}
	if declared := organizerEmail(event); declared != "" && declared != obj.Organizer {
		return conflict(cr, ReasonOrganizerMismatch,
			"ORGANIZER property "+declared+" does not match bound organizer "+obj.Organizer)
	}

	if !hasRecurrenceID {
		return p.applyMasterRequest(obj, event, incoming, cr)
	}
	return p.applyInstanceRequest(obj, recID, event, incoming, cr)
}

func (p *Processor) applyMasterRequest(obj *Object, event *ical.VEvent, incoming Revision, cr ComponentResult) ComponentResult {
	cs := obj.Master
	if cs == nil {
		cloned, err := cloneEvent(event)
		if err != nil {
			return conflict(cr, ReasonMalformed, "cannot store event: "+err.Error())
		}
		obj.Master = newComponentState(time.Time{}, false, cloned, incoming)
		p.store.Put(obj)
		return applied(cr, "master event materialized")
	}

	switch incoming.Compare(cs.Revision) {
	case -1:
		return ignored(cr, ReasonStale, "older revision than current SEQUENCE/DTSTAMP")
	case 0:
		if incoming.Digest == cs.Revision.Digest {
			cr.Duplicate = true
			return applied(cr, "identical revision already stored")
		}
		return conflict(cr, ReasonSameTimestamp, "same SEQUENCE and DTSTAMP but different content")
	}

	cloned, err := cloneEvent(event)
	if err != nil {
		return conflict(cr, ReasonMalformed, "cannot store event: "+err.Error())
	}
	replies := mergeReplies(cloned, cs.Replies)
	obj.Master = newComponentState(time.Time{}, false, cloned, incoming)
	obj.Master.Replies = replies
	p.store.Put(obj)

	if cs.Cancelled {
		return applied(cr, "newer revision resurrects cancelled series")
	}
	return applied(cr, "master event updated to newer revision")
}

func (p *Processor) applyInstanceRequest(obj *Object, recID time.Time, event *ical.VEvent, incoming Revision, cr ComponentResult) ComponentResult {
	key := recID.UTC()
	cs, ok := obj.Instances[key]
	if !ok {
		cloned, err := cloneEvent(event)
		if err != nil {
			return conflict(cr, ReasonMalformed, "cannot store event: "+err.Error())
		}
		obj.Instances[key] = newComponentState(key, false, cloned, incoming)
		p.store.Put(obj)
		return applied(cr, "new recurring instance stored")
	}

	switch incoming.Compare(cs.Revision) {
	case -1:
		return ignored(cr, ReasonStale, "older instance revision than current")
	case 0:
		if incoming.Digest == cs.Revision.Digest {
			cr.Duplicate = true
			return applied(cr, "identical instance revision already stored")
		}
		return conflict(cr, ReasonSameTimestamp, "same instance SEQUENCE and DTSTAMP but different content")
	}

	cloned, err := cloneEvent(event)
	if err != nil {
		return conflict(cr, ReasonMalformed, "cannot store event: "+err.Error())
	}
	replies := mergeReplies(cloned, cs.Replies)
	next := newComponentState(key, false, cloned, incoming)
	next.Replies = replies
	obj.Instances[key] = next
	p.store.Put(obj)
	return applied(cr, "recurring instance updated")
}

func (p *Processor) applyCancel(
	sender Sender,
	uid string,
	recID time.Time,
	hasRecurrenceID bool,
	event *ical.VEvent,
	incoming Revision,
	cr ComponentResult,
) ComponentResult {
	obj, exists := p.store.Get(uid)
	if !exists {
		return ignored(cr, ReasonObjectUnknown, "CANCEL for unknown UID")
	}
	if sender.Email != obj.Organizer {
		return conflict(cr, ReasonNotOrganizer, "sender "+sender.Email+" is not organizer "+obj.Organizer)
	}
	if declared := organizerEmail(event); declared != "" && declared != obj.Organizer {
		return conflict(cr, ReasonOrganizerMismatch,
			"ORGANIZER property "+declared+" does not match bound organizer "+obj.Organizer)
	}

	if !hasRecurrenceID {
		return p.applySeriesCancel(obj, event, incoming, cr)
	}
	return p.applyInstanceCancel(obj, recID, event, incoming, cr)
}

func (p *Processor) applySeriesCancel(obj *Object, event *ical.VEvent, incoming Revision, cr ComponentResult) ComponentResult {
	cs := obj.Master
	if cs == nil {
		return ignored(cr, ReasonObjectUnknown, "series CANCEL with no stored master")
	}

	switch incoming.Compare(cs.Revision) {
	case -1:
		return ignored(cr, ReasonStale, "older CANCEL than current series revision")
	case 0:
		if incoming.Digest == cs.Revision.Digest {
			cr.Duplicate = true
			return applied(cr, "series CANCEL already stored")
		}
		return conflict(cr, ReasonSameTimestamp, "same series SEQUENCE and DTSTAMP but different content")
	}

	cancelled, err := cloneEvent(event)
	if err != nil {
		return conflict(cr, ReasonMalformed, "cannot store cancel: "+err.Error())
	}
	cancelled.SetStatus(ical.ObjectStatusCancelled)
	next := newComponentState(time.Time{}, true, cancelled, incoming)
	next.Replies = mergeReplies(cancelled, cs.Replies)
	obj.Master = next
	p.store.Put(obj)
	return applied(cr, "whole series cancelled")
}

func (p *Processor) applyInstanceCancel(obj *Object, recID time.Time, event *ical.VEvent, incoming Revision, cr ComponentResult) ComponentResult {
	key := recID.UTC()
	cs, ok := obj.Instances[key]
	if !ok {
		return ignored(cr, ReasonInstanceUnknown, "CANCEL for instance with no stored override")
	}

	switch incoming.Compare(cs.Revision) {
	case -1:
		if obj.Master != nil && obj.Master.Cancelled {
			return ignored(cr, ReasonStale, "series already cancelled by a newer revision")
		}
		return ignored(cr, ReasonStale, "older instance CANCEL than current revision")
	case 0:
		if incoming.Digest == cs.Revision.Digest {
			cr.Duplicate = true
			return applied(cr, "instance CANCEL already stored")
		}
		return conflict(cr, ReasonSameTimestamp, "same instance SEQUENCE and DTSTAMP but different content")
	}

	cancelled, err := cloneEvent(event)
	if err != nil {
		return conflict(cr, ReasonMalformed, "cannot store cancel: "+err.Error())
	}
	cancelled.SetStatus(ical.ObjectStatusCancelled)
	next := newComponentState(key, true, cancelled, incoming)
	next.Replies = mergeReplies(cancelled, cs.Replies)
	obj.Instances[key] = next
	p.store.Put(obj)
	return applied(cr, "single instance cancelled")
}

func (p *Processor) applyReply(
	sender Sender,
	uid string,
	recID time.Time,
	hasRecurrenceID bool,
	event *ical.VEvent,
	incoming Revision,
	cr ComponentResult,
) ComponentResult {
	obj, exists := p.store.Get(uid)
	if !exists {
		return ignored(cr, ReasonObjectUnknown, "REPLY for unknown UID")
	}
	if declared := organizerEmail(event); declared != "" && declared != obj.Organizer {
		return conflict(cr, ReasonOrganizerMismatch,
			"REPLY ORGANIZER property "+declared+" does not match bound organizer "+obj.Organizer)
	}

	target, known := componentFor(obj, recID, hasRecurrenceID)
	if !known {
		if hasRecurrenceID {
			return ignored(cr, ReasonInstanceUnknown, "REPLY for unknown instance")
		}
		return ignored(cr, ReasonObjectUnknown, "REPLY before any invitation")
	}

	// Membership is checked against the organizer's stored roster, not the
	// attendee-supplied ICS, which an attacker controls.
	if _, onRoster := findAttendee(target.Event, sender.Email); !onRoster {
		return conflict(cr, ReasonNotAttendee, "sender "+sender.Email+" is not an attendee of the event")
	}
	attendee, ok := findAttendee(event, sender.Email)
	if !ok {
		return conflict(cr, ReasonNotAttendee, "REPLY contains no ATTENDEE for sender "+sender.Email)
	}
	status := attendee.ParticipationStatus()
	if !validAttendeePartstats[status] {
		return conflict(cr, ReasonInvalidPartstat, "PARTSTAT "+string(status)+" is not valid for VEVENT replies")
	}

	// Reply SEQUENCE must echo the organizer revision it answers. A reply to a
	// newer revision the server has never seen is held back; a reply to an
	// older revision is stale even if it is the first reply from that
	// attendee.
	switch {
	case incoming.Sequence > target.Revision.Sequence:
		return ignored(cr, ReasonFuture, "REPLY SEQUENCE "+itoa(incoming.Sequence)+" is newer than known revision")
	case incoming.Sequence < target.Revision.Sequence:
		return ignored(cr, ReasonStale, "REPLY answers older SEQUENCE "+itoa(incoming.Sequence)+", current is "+itoa(target.Revision.Sequence))
	}

	if target.Replies == nil {
		target.Replies = map[string]ReplyState{}
	}
	if previous, ok := target.Replies[sender.Email]; ok {
		switch incomingReply(incoming, status).Compare(previous) {
		case -1:
			return ignored(cr, ReasonStale, "older REPLY already superseded by newer one")
		case 0:
			if incoming.Digest == previous.Digest && previous.Status == status {
				cr.Duplicate = true
				return appliedWithReply(cr, sender.Email, status, "identical reply already stored")
			}
			return conflict(cr, ReasonSameTimestamp, "same REPLY SEQUENCE and DTSTAMP but different content")
		}
	}

	setAttendeePartstat(target.Event, sender.Email, status)
	target.Replies[sender.Email] = ReplyState{
		Sequence: incoming.Sequence,
		DtStamp:  incoming.DtStamp,
		Status:   status,
		Digest:   incoming.Digest,
	}
	p.store.Put(obj)
	return appliedWithReply(cr, sender.Email, status, "reply recorded")
}

func incomingReply(rev Revision, status ical.ParticipationStatus) ReplyState {
	return ReplyState{Sequence: rev.Sequence, DtStamp: rev.DtStamp, Status: status, Digest: rev.Digest}
}

func newComponentState(recID time.Time, cancelled bool, event *ical.VEvent, rev Revision) *ComponentState {
	return &ComponentState{
		RecurrenceID: recID,
		Event:        event,
		Revision:     rev,
		Cancelled:    cancelled,
		Replies:      map[string]ReplyState{},
	}
}

func applied(cr ComponentResult, detail string) ComponentResult {
	cr.Disposition = DispositionApplied
	cr.Detail = detail
	return cr
}

func appliedWithReply(cr ComponentResult, email string, status ical.ParticipationStatus, detail string) ComponentResult {
	cr = applied(cr, detail)
	if cr.AppliedReplies == nil {
		cr.AppliedReplies = map[string]ical.ParticipationStatus{}
	}
	cr.AppliedReplies[email] = status
	return cr
}

func ignored(cr ComponentResult, reason Reason, detail string) ComponentResult {
	cr.Disposition = DispositionIgnored
	cr.Reason = reason
	cr.Detail = detail
	return cr
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
