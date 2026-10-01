package imapserver

import (
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// SessionNotify is an IMAP session which supports the NOTIFY extension
// (RFC 5465).
//
// A new NOTIFY command fully replaces the previous subscription: the session
// is responsible for storing the requested options (or for clearing them when
// NOTIFY NONE is requested).
//
// When the session has an active subscription, it must report the subscribed
// events for mailboxes other than the currently selected one during Poll and
// Idle, using NotifyWriter.WriteStatus. Updates for the selected mailbox are
// already delivered through UpdateWriter via the mailbox tracker.
type SessionNotify interface {
	Session

	// Notify replaces any previous NOTIFY subscription. A nil options
	// pointer disables all notifications (NOTIFY NONE).
	Notify(w *NotifyWriter, options *imap.NotifyOptions) error

	// NotifyPoll flushes pending updates during a command, like Poll. It
	// combines selected-mailbox updates with non-selected mailbox NOTIFY
	// events.
	NotifyPoll(nw *NotifyWriter, uw *UpdateWriter, allowExpunge bool) error

	// NotifyIdle continuously writes updates while IDLE, combining the
	// selected mailbox tracker with non-selected mailbox NOTIFY events.
	NotifyIdle(nw *NotifyWriter, uw *UpdateWriter, stop <-chan struct{}) error
}

// NotifyWriter writes NOTIFY event responses (RFC 5465).
//
// Mailbox message events for non-selected mailboxes are sent as unsolicited
// STATUS responses.
type NotifyWriter struct {
	conn *Conn
}

// WriteStatus writes an unsolicited STATUS response representing a NOTIFY
// event for a non-selected mailbox.
func (w *NotifyWriter) WriteStatus(data *imap.StatusData, options *imap.StatusOptions) error {
	if w.conn == nil {
		return nil
	}
	return w.conn.writeStatus(data, options)
}

// WriteMessageFlags writes an unsolicited UID FETCH response representing a
// FlagChange event for a non-selected mailbox.
func (w *NotifyWriter) WriteMessageFlags(uid imap.UID, flags []imap.Flag) error {
	if w.conn == nil {
		return nil
	}
	return w.conn.writeNotifyFetch(uid, flags)
}

// writeNotifyFetch writes a FlagChange event for a non-selected mailbox:
//
//   - UID FETCH (UID <uid> FLAGS (...))
//
// The response uses UID FETCH since the client has no sequence number context
// for a mailbox it hasn't selected (RFC 5465 section 7.2).
func (c *Conn) writeNotifyFetch(uid imap.UID, flags []imap.Flag) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("UID").SP().Atom("FETCH").SP().Special('(')
	enc.Atom("UID").SP().UID(uid).SP()
	enc.Atom("FLAGS").SP().List(len(flags), func(i int) {
		enc.Flag(flags[i])
	})
	return enc.Special(')').CRLF()
}

// WriteNotificationOverflow writes a NOTIFICATIONOVERFLOW response and
// disables the active NOTIFY subscription on the connection.
//
// It is used when the server is no longer able to monitor all of the
// requested events, for instance because too many updates are pending for a
// slow client.
func (w *NotifyWriter) WriteNotificationOverflow() error {
	if w.conn == nil {
		return nil
	}
	return w.conn.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Code: imap.ResponseCodeNotificationOverflow,
		Text: "Too many events; NOTIFY subscription removed",
	})
}

func (c *Conn) handleNotify(dec *imapwire.Decoder) error {
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	options, err := readNotifyOptions(dec)
	if err != nil {
		return err
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	sess, ok := c.session.(SessionNotify)
	if !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "NOTIFY is not supported",
		}
	}

	w := &NotifyWriter{conn: c}
	return sess.Notify(w, options)
}

// readNotifyOptions parses the arguments of a NOTIFY command (RFC 5465
// section 6) and validates the mailbox specifier/event combinations that the
// server is able to support. Unknown or unsupported combinations result in a
// protocol error (BAD response).
func readNotifyOptions(dec *imapwire.Decoder) (*imap.NotifyOptions, error) {
	var kind string
	if !dec.ExpectSP() || !dec.ExpectAtom(&kind) {
		return nil, dec.Err()
	}

	switch strings.ToUpper(kind) {
	case "NONE":
		return nil, nil
	case "SET":
	default:
		return nil, newClientBugError("Syntax error: expected SET or NONE")
	}

	options := &imap.NotifyOptions{}

	// status-indicator / event-groups. At least one event group is required;
	// the optional (STATUS) indicator comes first.
	haveStatus := false
	haveGroup := false
	for dec.SP() {
		var (
			isStatus bool
			item     imap.NotifyItem
		)
		isList, err := dec.List(func() error {
			// The first field is either an explicit mailbox list
			// ((INBOX "Sent") (...)) or a specifier atom
			// (SELECTED/PERSONAL/SUBTREE/...).
			isMailboxList, err := dec.List(func() error {
				var mailbox string
				if !dec.ExpectMailbox(&mailbox) {
					return dec.Err()
				}
				item.Mailboxes = append(item.Mailboxes, mailbox)
				return nil
			})
			if err != nil {
				return err
			}

			if isMailboxList {
				// Explicit mailbox list: fall through to the event list.
			} else {
				var spec string
				if !dec.ExpectAtom(&spec) {
					return dec.Err()
				}
				switch {
				case strings.EqualFold(spec, "STATUS"):
					if dec.SP() {
						return newClientBugError("Syntax error: STATUS indicator takes no arguments")
					}
					isStatus = true
					return nil
				case strings.EqualFold(spec, string(imap.NotifyMailboxSpecSelected)):
					item.MailboxSpec = imap.NotifyMailboxSpecSelected
				case strings.EqualFold(spec, string(imap.NotifyMailboxSpecSelectedDelayed)):
					item.MailboxSpec = imap.NotifyMailboxSpecSelectedDelayed
				case strings.EqualFold(spec, string(imap.NotifyMailboxSpecPersonal)):
					item.MailboxSpec = imap.NotifyMailboxSpecPersonal
				case strings.EqualFold(spec, string(imap.NotifyMailboxSpecInboxes)),
					strings.EqualFold(spec, string(imap.NotifyMailboxSpecSubscribed)):
					return &imap.Error{
						Type: imap.StatusResponseTypeBad,
						Text: "Unsupported NOTIFY mailbox specifier: " + spec,
					}
				case strings.EqualFold(spec, "SUBTREE"):
					item.Subtree = true
					if !dec.ExpectSP() {
						return dec.Err()
					}
					if err := readNotifyMailboxList(dec, &item); err != nil {
						return err
					}
				default:
					return newClientBugError("Syntax error: unknown NOTIFY mailbox specifier " + spec)
				}
			}

			if dec.SP() {
				if err := readNotifyEventList(dec, &item); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !isList {
			return nil, newClientBugError("Syntax error: expected NOTIFY event group")
		}

		if isStatus {
			if haveStatus || haveGroup {
				return nil, newClientBugError("Syntax error: (STATUS) indicator can only appear first")
			}
			haveStatus = true
			options.Status = true
			continue
		}

		haveGroup = true
		options.Items = append(options.Items, item)
	}

	if !haveGroup {
		return nil, newClientBugError("Syntax error: NOTIFY SET requires at least one event group")
	}

	if err := validateNotifyOptions(options); err != nil {
		return nil, err
	}

	return options, nil
}

func readNotifyMailboxList(dec *imapwire.Decoder, item *imap.NotifyItem) error {
	return dec.ExpectList(func() error {
		var mailbox string
		if !dec.ExpectMailbox(&mailbox) {
			return dec.Err()
		}
		item.Mailboxes = append(item.Mailboxes, mailbox)
		return nil
	})
}

func readNotifyEventList(dec *imapwire.Decoder, item *imap.NotifyItem) error {
	return dec.ExpectList(func() error {
		var event string
		if !dec.ExpectAtom(&event) {
			return dec.Err()
		}
		notifyEvent := notifyEventFromWire(event)
		if notifyEvent == "" {
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Text: "Unknown NOTIFY event: " + event,
			}
		}
		item.Events = append(item.Events, notifyEvent)
		return nil
	})
}

func notifyEventFromWire(s string) imap.NotifyEvent {
	for _, event := range []imap.NotifyEvent{
		imap.NotifyEventMessageNew,
		imap.NotifyEventMessageExpunge,
		imap.NotifyEventFlagChange,
		imap.NotifyEventAnnotationChange,
		imap.NotifyEventMailboxName,
		imap.NotifyEventSubscriptionChange,
		imap.NotifyEventMailboxMetadataChange,
		imap.NotifyEventServerMetadataChange,
	} {
		if strings.EqualFold(s, string(event)) {
			return event
		}
	}
	return ""
}

// validateNotifyOptions checks RFC 5465 section 7 restrictions and rejects
// any combination the server doesn't implement. Only message events are
// handled, and only for SELECTED, explicit mailbox lists and the PERSONAL
// namespace.
func validateNotifyOptions(options *imap.NotifyOptions) error {
	seenSpecs := make(map[imap.NotifyMailboxSpec]struct{})
	for _, item := range options.Items {
		switch item.MailboxSpec {
		case imap.NotifyMailboxSpecSelected, imap.NotifyMailboxSpecSelectedDelayed:
			if _, ok := seenSpecs[imap.NotifyMailboxSpecSelected]; ok {
				return newClientBugError("Duplicate SELECTED NOTIFY event group")
			}
			seenSpecs[imap.NotifyMailboxSpecSelected] = struct{}{}
			if err := checkNotifyMessageEvents(item.Events, "SELECTED"); err != nil {
				return err
			}
		case imap.NotifyMailboxSpecPersonal:
			if _, ok := seenSpecs[imap.NotifyMailboxSpecPersonal]; ok {
				return newClientBugError("Duplicate PERSONAL NOTIFY event group")
			}
			seenSpecs[imap.NotifyMailboxSpecPersonal] = struct{}{}
			if err := checkNotifyMessageEvents(item.Events, "PERSONAL"); err != nil {
				return err
			}
		case "":
			if len(item.Mailboxes) == 0 {
				return newClientBugError("Syntax error: NOTIFY group has no mailbox")
			}
			if err := checkNotifyMessageEvents(item.Events, "mailbox list"); err != nil {
				return err
			}
		default:
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Text: "Unsupported NOTIFY mailbox specifier: " + string(item.MailboxSpec),
			}
		}
	}
	return nil
}

// checkNotifyMessageEvents ensures the event group only contains message
// events: the server implements no mailbox, subscription, annotation or
// metadata events.
func checkNotifyMessageEvents(events []imap.NotifyEvent, context string) error {
	for _, event := range events {
		switch event {
		case imap.NotifyEventMessageNew,
			imap.NotifyEventMessageExpunge,
			imap.NotifyEventFlagChange:
			continue
		}
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Unsupported NOTIFY event " + string(event) + " with " + context,
		}
	}
	return nil
}
