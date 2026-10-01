package imapmemserver

import (
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// maxNotifyEvents is the maximum number of NOTIFY events queued for a single
// session that isn't actively reading responses. Once the queue is full, the
// session gets a single NOTIFICATIONOVERFLOW response instead of growing the
// queue without bounds.
const maxNotifyEvents = 256

// notifyState tracks the active NOTIFY subscriptions of a UserSession and
// queues events for mailboxes other than the currently selected one.
type notifyState struct {
	user *User

	mu       sync.Mutex
	items    []imap.NotifyItem
	events   []notifyEvent
	overflow bool
	active   bool
	wake     chan struct{}
}

func newNotifyState(user *User) *notifyState {
	return &notifyState{user: user}
}

// notifyEvent is a pending NOTIFY event for a non-selected mailbox.
type notifyEvent struct {
	mailbox *Mailbox
	kind    imap.NotifyEvent

	// For FlagChange events.
	uid   imap.UID
	flags []imap.Flag

	// For message count events (MessageNew, MessageExpunge).
	numMessages uint32
}

// flagChange is a deferred FlagChange notification, collected while a mailbox
// lock is held and dispatched after it is released.
type flagChange struct {
	uid   imap.UID
	flags []imap.Flag
}

// set replaces the active subscription. A nil options pointer disables all
// notifications (NOTIFY NONE).
func (s *notifyState) set(options *imap.NotifyOptions) {
	s.user.mutex.Lock()
	defer s.user.mutex.Unlock()

	if s.active {
		s.user.unregisterLocked(s)
	}

	s.mu.Lock()
	s.items = nil
	s.events = nil
	s.overflow = false
	s.active = options != nil
	if options != nil {
		s.items = append(s.items, options.Items...)
	}
	s.mu.Unlock()

	if options != nil {
		s.user.registerLocked(s)
	}
}

// close unregisters the state when the session is closed.
func (s *notifyState) close() {
	s.user.mutex.Lock()
	defer s.user.mutex.Unlock()

	s.mu.Lock()
	active := s.active
	s.active = false
	s.wake = nil
	s.mu.Unlock()

	if active {
		s.user.unregisterLocked(s)
	}
}

// setIdleWake registers (or clears with nil) the channel used to wake up an
// ongoing IDLE when new events are queued.
func (s *notifyState) setIdleWake(ch chan struct{}) {
	s.mu.Lock()
	s.wake = ch
	s.mu.Unlock()
}

// subscribe returns the set of events the subscription requests for the
// mailbox with the given name.
func (s *notifyState) subscribe(name string) map[imap.NotifyEvent]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	events := make(map[imap.NotifyEvent]struct{})
	for _, item := range s.items {
		switch item.MailboxSpec {
		case imap.NotifyMailboxSpecPersonal:
			for _, event := range item.Events {
				events[event] = struct{}{}
			}
		case "":
			for _, mailbox := range item.Mailboxes {
				if mailbox == name || (item.Subtree && isSubtree(mailbox, name)) {
					for _, event := range item.Events {
						events[event] = struct{}{}
					}
					break
				}
			}
		}
	}
	return events
}

func isSubtree(root, name string) bool {
	return name == root || strings.HasPrefix(name, root+string(mailboxDelim))
}

// enqueue adds an event to the session queue.
func (s *notifyState) enqueue(event notifyEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.overflow {
		return
	}

	// Coalesce with a pending event when possible, to keep the queue bounded
	// and to only report the latest state.
	if !s.coalesceLocked(event) {
		if len(s.events) >= maxNotifyEvents {
			s.overflow = true
			s.events = nil
		} else {
			s.events = append(s.events, event)
		}
	}

	if s.wake != nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// coalesceLocked merges the event into an already queued one. It reports
// whether the event was merged (and thus must not be appended).
func (s *notifyState) coalesceLocked(event notifyEvent) bool {
	if event.kind != imap.NotifyEventFlagChange {
		// MessageNew/MessageExpunge only carry the current MESSAGES count:
		// any earlier queued count event for the same mailbox is now stale, so
		// replace it and drop the new one.
		for i := range s.events {
			prev := &s.events[i]
			if prev.mailbox == event.mailbox && prev.kind == event.kind {
				prev.numMessages = event.numMessages
				return true
			}
		}
		return false
	}

	// FlagChange carries a flags list for a UID; an earlier queued event for
	// the same message is replaced by the most recent flags.
	for i := range s.events {
		prev := &s.events[i]
		if prev.mailbox == event.mailbox && prev.kind == imap.NotifyEventFlagChange && prev.uid == event.uid {
			prev.flags = event.flags
			return true
		}
	}
	return false
}

// drain writes all pending events. It is called during Poll or Idle, so the
// write lock of the connection is held by the caller.
func (s *notifyState) drain(w *imapserver.NotifyWriter) error {
	s.mu.Lock()
	events := s.events
	overflow := s.overflow
	s.events = nil
	s.overflow = false
	s.mu.Unlock()

	for _, event := range events {
		if event.kind == imap.NotifyEventFlagChange {
			if err := w.WriteMessageFlags(event.uid, event.flags); err != nil {
				return err
			}
			continue
		}

		num := event.numMessages
		data := &imap.StatusData{
			Mailbox:     event.mailbox.name,
			NumMessages: &num,
		}
		options := &imap.StatusOptions{NumMessages: true}
		if err := w.WriteStatus(data, options); err != nil {
			return err
		}
	}

	if overflow {
		return w.WriteNotificationOverflow()
	}
	return nil
}

// Notify implements imapserver.SessionNotify.
func (sess *UserSession) Notify(w *imapserver.NotifyWriter, options *imap.NotifyOptions) error {
	sess.notify.set(options)
	return sess.notify.drain(w)
}
