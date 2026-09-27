package memstore

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

func TestFind(t *testing.T) {
	m := NewWithCleanupInterval(0)
	m.items["session_token"] = item{object: []byte("encoded_data"), expiration: time.Now().Add(time.Second).UnixNano()}

	b, found, err := m.Find("session_token")
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}
	if found != true {
		t.Fatalf("got %v: expected %v", found, true)
	}
	if bytes.Equal(b, []byte("encoded_data")) == false {
		t.Fatalf("got %v: expected %v", b, []byte("encoded_data"))
	}
}

func TestFindMissing(t *testing.T) {
	m := NewWithCleanupInterval(0)

	_, found, err := m.Find("missing_session_token")
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}
	if found != false {
		t.Fatalf("got %v: expected %v", found, false)
	}
}

func TestCommitVersioned(t *testing.T) {
	m := NewWithCleanupInterval(0)
	expiry := time.Now().Add(time.Minute)

	err := m.Commit("session_token", []byte("v1_data"), expiry)
	if err != nil {
		t.Fatal(err)
	}

	_, version, found, err := m.FindVersioned("session_token")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected token to be found")
	}

	// A commit with the correct expected version succeeds and bumps the
	// version.
	committed, err := m.CommitVersioned("session_token", []byte("v2_data"), expiry, version)
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("expected commit to succeed")
	}

	b, newVersion, found, err := m.FindVersioned("session_token")
	if err != nil {
		t.Fatal(err)
	}
	if !found || !bytes.Equal(b, []byte("v2_data")) {
		t.Fatalf("got %v (found %v): expected %v", b, found, []byte("v2_data"))
	}
	if newVersion == version {
		t.Fatal("expected version to change after commit")
	}

	// A commit with a stale expected version is a no-op.
	committed, err = m.CommitVersioned("session_token", []byte("v3_data"), expiry, version)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("expected commit with stale version to fail")
	}

	b, found, err = m.Find("session_token")
	if err != nil {
		t.Fatal(err)
	}
	if !found || !bytes.Equal(b, []byte("v2_data")) {
		t.Fatalf("got %v (found %v): expected %v", b, found, []byte("v2_data"))
	}
}

func TestCommitVersionedMissing(t *testing.T) {
	m := NewWithCleanupInterval(0)

	// A versioned commit must never create a token which does not exist
	// (e.g. one which was deleted by a concurrent Destroy).
	committed, err := m.CommitVersioned("missing_session_token", []byte("data"), time.Now().Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("expected commit for missing token to fail")
	}

	_, found, err := m.Find("missing_session_token")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("missing token was created by CommitVersioned")
	}
}

func TestCommitNew(t *testing.T) {
	m := NewWithCleanupInterval(0)

	err := m.Commit("session_token", []byte("encoded_data"), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}

	v, found := m.items["session_token"]
	if found != true {
		t.Fatalf("got %v: expected %v", found, true)
	}

	if reflect.DeepEqual(v.object, []byte("encoded_data")) == false {
		t.Fatalf("got %v: expected %v", v.object, []byte("encoded_data"))
	}
}

func TestCommitUpdated(t *testing.T) {
	m := NewWithCleanupInterval(0)

	err := m.Commit("session_token", []byte("encoded_data"), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}

	err = m.Commit("session_token", []byte("new_encoded_data"), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}

	v := m.items["session_token"].object

	if reflect.DeepEqual(v, []byte("new_encoded_data")) == false {
		t.Fatalf("got %v: expected %v", v, []byte("new_encoded_data"))
	}
}

func TestExpiry(t *testing.T) {
	m := NewWithCleanupInterval(0)

	err := m.Commit("session_token", []byte("encoded_data"), time.Now().Add(100*time.Millisecond))
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}

	_, found, _ := m.Find("session_token")
	if found != true {
		t.Fatalf("got %v: expected %v", found, true)
	}

	time.Sleep(101 * time.Millisecond)
	_, found, _ = m.Find("session_token")
	if found != false {
		t.Fatalf("got %v: expected %v", found, false)
	}
}

func TestDelete(t *testing.T) {
	m := NewWithCleanupInterval(0)
	m.items["session_token"] = item{object: []byte("encoded_data"), expiration: time.Now().Add(time.Second).UnixNano()}

	err := m.Delete("session_token")
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}

	_, found := m.items["session_token"]
	if found != false {
		t.Fatalf("got %v: expected %v", found, false)
	}
}

func TestCleanupInterval(t *testing.T) {
	m := NewWithCleanupInterval(100 * time.Millisecond)
	defer m.StopCleanup()
	m.mu.Lock()
	m.items["session_token"] = item{object: []byte("encoded_data"), expiration: time.Now().Add(500 * time.Millisecond).UnixNano()}
	m.mu.Unlock()

	m.mu.Lock()
	_, ok := m.items["session_token"]
	m.mu.Unlock()
	if !ok {
		t.Fatalf("got %v: expected %v", ok, true)
	}

	time.Sleep(time.Second)
	m.mu.Lock()
	_, ok = m.items["session_token"]
	m.mu.Unlock()
	if ok {
		t.Fatalf("got %v: expected %v", ok, false)
	}
}
