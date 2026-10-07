// Package caldav provides a client and server CalDAV implementation.
//
// CalDAV is defined in RFC 4791.
package caldav

import (
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/internal"
)

var CapabilityCalendar = webdav.Capability("calendar-access")

func NewCalendarHomeSet(path string) webdav.BackendSuppliedHomeSet {
	return &calendarHomeSet{Href: internal.Href{Path: path}}
}

// ValidateCalendarObject checks the validity of a calendar object according to
// the contraints layed out in RFC 4791 section 4.1 and returns the only event
// type and UID occuring in this calendar, or an error if the calendar could
// not be validated.
func ValidateCalendarObject(cal *ical.Calendar) (eventType string, uid string, err error) {
	// Calendar object resources contained in calendar collections
	// MUST NOT specify the iCalendar METHOD property.
	if prop := cal.Props.Get(ical.PropMethod); prop != nil {
		return "", "", fmt.Errorf("calendar resource must not specify METHOD property")
	}

	for _, comp := range cal.Children {
		// Calendar object resources contained in calendar collections
		// MUST NOT contain more than one type of calendar component
		// (e.g., VEVENT, VTODO, VJOURNAL, VFREEBUSY, etc.) with the
		// exception of VTIMEZONE components, which MUST be specified
		// for each unique TZID parameter value specified in the
		// iCalendar object.
		if comp.Name != ical.CompTimezone {
			if eventType == "" {
				eventType = comp.Name
			}
			if eventType != comp.Name {
				return "", "", fmt.Errorf("conflicting event types in calendar: %s, %s", eventType, comp.Name)
			}
			// TODO check VTIMEZONE for each TZID?
		}

		// Calendar components in a calendar collection that have
		// different UID property values MUST be stored in separate
		// calendar object resources.
		compUID, err := comp.Props.Text(ical.PropUID)
		if err != nil {
			return "", "", fmt.Errorf("error checking component UID: %v", err)
		}
		if uid == "" {
			uid = compUID
		}
		if compUID != "" && uid != compUID {
			return "", "", fmt.Errorf("conflicting UID values in calendar: %s, %s", uid, compUID)
		}
	}
	return eventType, uid, nil
}

type Calendar struct {
	Path                  string
	Name                  string
	Description           string
	MaxResourceSize       int64
	SupportedComponentSet []string
	// ReadOnly reports that the current user may only read this calendar. It
	// controls the DAV:current-user-privilege-set reported by the server.
	ReadOnly bool
}

type CalendarCompRequest struct {
	Name string

	AllProps bool
	Props    []string

	AllComps bool
	Comps    []CalendarCompRequest

	Expand *CalendarExpandRequest
}

type CalendarExpandRequest struct {
	Start, End time.Time
}

type CompFilter struct {
	Name         string
	IsNotDefined bool
	Start, End   time.Time
	Props        []PropFilter
	Comps        []CompFilter
}

type ParamFilter struct {
	Name         string
	IsNotDefined bool
	TextMatch    *TextMatch
}

type PropFilter struct {
	Name         string
	IsNotDefined bool
	Start, End   time.Time
	TextMatch    *TextMatch
	ParamFilter  []ParamFilter
}

type TextMatch struct {
	Text            string
	NegateCondition bool
}

type CalendarQuery struct {
	CompRequest CalendarCompRequest
	CompFilter  CompFilter
}

type CalendarMultiGet struct {
	Paths       []string
	CompRequest CalendarCompRequest
}

type CalendarObject struct {
	Path          string
	ModTime       time.Time
	ContentLength int64
	ETag          string
	Data          *ical.Calendar
}

// SyncQuery is the query struct represents a sync-collection request
type SyncQuery struct {
	CompRequest CalendarCompRequest
	SyncToken   string
	Limit       int // <= 0 means unlimited
}

// SyncResponse contains the result of a sync-collection REPORT.
//
// Updated contains the calendar object resources that have been added or
// modified. Deleted contains the paths of resources that have been removed.
// The distinction is conveyed in the protocol by an explicit response status,
// as required by RFC 6578 section 6.3.
type SyncResponse struct {
	SyncToken   string
	Updated     []CalendarObject
	Deleted     []string
	MoreResults bool
}

// SyncCollectionBackend is an optional Backend extension adding support for
// the RFC 6578 sync-collection REPORT. A Backend that doesn't implement this
// interface keeps its previous behavior: the server doesn't advertise the
// "sync-collection" DAV feature and rejects sync-collection requests.
type SyncCollectionBackend interface {
	// SyncCollection synchronizes the calendar collection at path. An empty
	// syncToken requests an initial synchronization; a non-empty token
	// requests the changes that happened since the token was issued. The
	// limit, when positive, bounds the number of returned changes and forces
	// MoreResults to be true when more pages remain.
	SyncCollection(ctx context.Context, path string, query *SyncQuery) (*SyncResponse, error)

	// CurrentSyncToken returns the current sync token of the calendar
	// collection at path. It backs the DAV:sync-token live property.
	CurrentSyncToken(ctx context.Context, path string) (string, error)
}

// InvalidSyncTokenError wraps err into an HTTP 403 error carrying the
// DAV:valid-sync-token precondition (RFC 6578 section 3.2). Backends should
// return it when the sync token is unknown, expired, or was issued for another
// collection.
func InvalidSyncTokenError(err error) error {
	return internal.InvalidSyncTokenError(err)
}
