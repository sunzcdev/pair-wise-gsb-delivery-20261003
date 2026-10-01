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

func TestFlash(t *testing.T) {
	m := NewWithCleanupInterval(0)
	original := item{object: []byte("encoded_data"), expiration: time.Now().Add(time.Second).UnixNano()}
	m.items["session_token"] = original

	called := false
	ok, err := m.Flash("session_token", "key", func(b []byte) ([]byte, bool, error) {
		called = true
		if !bytes.Equal(b, []byte("encoded_data")) {
			t.Fatalf("got %v: expected %v", b, []byte("encoded_data"))
		}
		return []byte("updated_data"), true, nil
	})
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}
	if ok != true {
		t.Fatalf("got %v: expected %v", ok, true)
	}
	if called != true {
		t.Fatal("expected callback to be invoked")
	}

	updated := m.items["session_token"]
	if !bytes.Equal(updated.object, []byte("updated_data")) {
		t.Fatalf("got %v: expected %v", updated.object, []byte("updated_data"))
	}
	if updated.expiration != original.expiration {
		t.Fatalf("flash changed the expiry time")
	}
}

func TestFlashMissing(t *testing.T) {
	m := NewWithCleanupInterval(0)

	called := false
	ok, err := m.Flash("missing_session_token", "key", func(b []byte) ([]byte, bool, error) {
		called = true
		if b != nil {
			t.Fatalf("got %v: expected nil", b)
		}
		return nil, false, nil
	})
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}
	if ok != false {
		t.Fatalf("got %v: expected %v", ok, false)
	}
	if called != true {
		t.Fatal("expected callback to be invoked")
	}
	if _, found := m.items["missing_session_token"]; found {
		t.Fatal("expected no record to be created when the flash key is absent")
	}
}

func TestFlashExpired(t *testing.T) {
	m := NewWithCleanupInterval(0)
	m.items["session_token"] = item{object: []byte("encoded_data"), expiration: time.Now().Add(-time.Second).UnixNano()}

	ok, err := m.Flash("session_token", "key", func(b []byte) ([]byte, bool, error) {
		if b != nil {
			t.Fatalf("got %v: expected nil for expired record", b)
		}
		return nil, false, nil
	})
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}
	if ok != false {
		t.Fatalf("got %v: expected %v", ok, false)
	}
}

func TestMergeCommit(t *testing.T) {
	m := NewWithCleanupInterval(0)
	original := item{object: []byte("encoded_data"), expiration: time.Now().Add(time.Second).UnixNano()}
	m.items["session_token"] = original

	newExpiry := time.Now().Add(time.Minute)
	err := m.MergeCommit("session_token", func(current []byte) ([]byte, error) {
		if !bytes.Equal(current, []byte("encoded_data")) {
			t.Fatalf("got %v: expected %v", current, []byte("encoded_data"))
		}
		return []byte("merged_data"), nil
	}, newExpiry)
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}

	updated, found := m.items["session_token"]
	if found != true {
		t.Fatalf("got %v: expected %v", found, true)
	}
	if !bytes.Equal(updated.object, []byte("merged_data")) {
		t.Fatalf("got %v: expected %v", updated.object, []byte("merged_data"))
	}
	if updated.expiration != newExpiry.UnixNano() {
		t.Fatalf("got %v: expected %v", updated.expiration, newExpiry.UnixNano())
	}
}

func TestMergeCommitMissing(t *testing.T) {
	m := NewWithCleanupInterval(0)

	expiry := time.Now().Add(time.Minute)
	err := m.MergeCommit("missing_session_token", func(current []byte) ([]byte, error) {
		if current != nil {
			t.Fatalf("got %v: expected nil", current)
		}
		return []byte("new_data"), nil
	}, expiry)
	if err != nil {
		t.Fatalf("got %v: expected %v", err, nil)
	}

	v, found := m.items["missing_session_token"]
	if found != true {
		t.Fatalf("got %v: expected %v", found, true)
	}
	if !bytes.Equal(v.object, []byte("new_data")) {
		t.Fatalf("got %v: expected %v", v.object, []byte("new_data"))
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
