package itip

import (
	"sync"
	"testing"

	ical "github.com/arran4/golang-ical"
)

// TestConcurrentRedeliveries hammers the memory store with the same message
// from several goroutines and checks the stored revision never corrupts.
func TestConcurrentRedeliveries(t *testing.T) {
	store := NewMemoryStore()
	proc := NewProcessor(store)

	invite := invitation(uid1, 0, "20260101T080000Z", "20260110T100000Z", "Meeting")
	requestMsg := buildMessage(t, ical.MethodRequest, invite)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = proc.Process(organizerSender(orgEmail), requestMsg)
		}()
	}
	wg.Wait()

	obj, ok := store.Get(uid1)
	if !ok || obj.Master == nil {
		t.Fatal("master missing after concurrent processing")
	}
	if obj.Master.Revision.Sequence != 0 {
		t.Fatalf("sequence = %d, want 0", obj.Master.Revision.Sequence)
	}

	accepted := reply(uid1, 0, "20260102T080000Z", "20260110T100000Z", ical.ParticipationStatusAccepted)
	replyMsg := buildMessage(t, ical.MethodReply, accepted)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = proc.Process(attendeeSender(attEmail), replyMsg)
		}()
	}
	wg.Wait()

	if got := obj.Master.Replies[attEmail].Status; got != ical.ParticipationStatusAccepted {
		t.Fatalf("stored partstat = %q, want ACCEPTED", got)
	}
}
