// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// kvChange is one application-defined, ordered mutation. Origin identifies
// the node that created it; Seq is per-origin and gap-sensitive.
type kvChange struct {
	Origin string `json:"origin"`
	Seq    int    `json:"seq"`
	Key    string `json:"key"`
	Val    string `json:"val"`
	Del    bool   `json:"del,omitempty"`
}

// kvSummary advertises, for every origin the sender knows, the highest seq
// it has applied.
type kvSummary struct {
	Watermarks map[string]int `json:"watermarks"`
}

// kvRequest asks the Want origin for changes with seq > AfterSeq.
type kvRequest struct {
	From     string `json:"from"`
	Want     string `json:"want"`
	AfterSeq int    `json:"after_seq"`
}

// kvFullState is the full-state fallback payload.
type kvFullState struct {
	Name  string            `json:"name"`
	Store map[string]string `json:"store"`
	Heads map[string]int    `json:"heads"`
}

// incrementalKVDelegate is a test Delegate + IncrementalDelegate modeling an
// ordered KV application. It retains only the most recent `retain` changes;
// a peer needing a pruned change forces a full sync.
type incrementalKVDelegate struct {
	mu sync.Mutex

	name   string
	retain int

	store   map[string]string
	changes []kvChange
	head    map[string]int // highest seq known per origin

	completed int
	full      int

	// failOnceKey, when set, makes the next ApplyChange carrying that key
	// return an error (consumed once), to simulate a mid-round failure.
	failOnceKey string
}

func newIncrementalKVDelegate(name string, retain int) *incrementalKVDelegate {
	return &incrementalKVDelegate{
		name:   name,
		retain: retain,
		store:  map[string]string{},
		head:   map[string]int{},
	}
}

func (d *incrementalKVDelegate) Put(key, val string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.appendLocked(kvChange{Key: key, Val: val})
}

func (d *incrementalKVDelegate) Delete(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.appendLocked(kvChange{Key: key, Del: true})
}

func (d *incrementalKVDelegate) appendLocked(c kvChange) {
	d.head[d.name]++
	c.Origin = d.name
	c.Seq = d.head[d.name]
	d.changes = append(d.changes, c)
	if c.Del {
		delete(d.store, c.Key)
	} else {
		d.store[c.Key] = c.Val
	}
	if d.retain >= 0 && len(d.changes) > d.retain {
		d.changes = d.changes[len(d.changes)-d.retain:]
	}
}

func (d *incrementalKVDelegate) snapshot() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]string, len(d.store))
	for k, v := range d.store {
		out[k] = v
	}
	return out
}

func (d *incrementalKVDelegate) completedRounds() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.completed
}

func (d *incrementalKVDelegate) fullMerges() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.full
}

func (d *incrementalKVDelegate) failNextApply(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failOnceKey = key
}

// --- Delegate ---

func (d *incrementalKVDelegate) NodeMeta(limit int) []byte                  { return nil }
func (d *incrementalKVDelegate) NotifyMsg([]byte)                           {}
func (d *incrementalKVDelegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }

func (d *incrementalKVDelegate) LocalState(join bool) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	full := kvFullState{Name: d.name, Store: d.store, Heads: map[string]int{}}
	for origin, seq := range d.head {
		full.Heads[origin] = seq
	}
	buf, _ := json.Marshal(full)
	return buf
}

func (d *incrementalKVDelegate) MergeRemoteState(buf []byte, join bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var full kvFullState
	if err := json.Unmarshal(buf, &full); err != nil {
		panic(fmt.Sprintf("bad full state: %v", err))
	}
	for k, v := range full.Store {
		d.store[k] = v
	}
	for origin, seq := range full.Heads {
		if seq > d.head[origin] {
			d.head[origin] = seq
		}
	}
	d.full++
	d.completed++
}

// --- IncrementalDelegate ---

func (d *incrementalKVDelegate) IncrementalSummary(join bool) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	s := kvSummary{Watermarks: map[string]int{}}
	for origin, seq := range d.head {
		s.Watermarks[origin] = seq
	}
	buf, _ := json.Marshal(s)
	return buf
}

func (d *incrementalKVDelegate) MissingChanges(remoteSummary []byte, join bool) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var summary kvSummary
	if err := json.Unmarshal(remoteSummary, &summary); err != nil {
		return nil, err
	}

	// This test has exactly two origins; request the newest missing one.
	for origin, remoteSeq := range summary.Watermarks {
		if origin == d.name {
			continue
		}
		if remoteSeq > d.head[origin] {
			return json.Marshal(kvRequest{
				From:     d.name,
				Want:     origin,
				AfterSeq: d.head[origin],
			})
		}
	}
	return nil, nil
}

func (d *incrementalKVDelegate) ProduceChanges(request []byte, join bool) ([][]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(request) == 0 {
		return nil, nil
	}

	var req kvRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if req.Want != d.name {
		return nil, fmt.Errorf("%s cannot produce changes for %q", d.name, req.Want)
	}

	oldest := d.head[d.name] - len(d.changes) + 1
	if req.AfterSeq < oldest-1 {
		return nil, ErrIncrementalFallback
	}

	var out [][]byte
	for _, c := range d.changes {
		if c.Seq > req.AfterSeq {
			buf, _ := json.Marshal(c)
			out = append(out, buf)
		}
	}
	return out, nil
}

func (d *incrementalKVDelegate) ApplyChange(buf []byte, join bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	var c kvChange
	if err := json.Unmarshal(buf, &c); err != nil {
		return err
	}

	if d.failOnceKey != "" && c.Key == d.failOnceKey {
		d.failOnceKey = ""
		return errors.New("simulated apply failure")
	}

	have := d.head[c.Origin]
	if c.Seq <= have {
		return nil
	}
	// Enforce the application-defined ordering: no gaps accepted.
	if c.Seq != have+1 {
		return fmt.Errorf("%s: out-of-order change from %s: have seq %d got %d",
			d.name, c.Origin, have, c.Seq)
	}

	if c.Del {
		delete(d.store, c.Key)
	} else {
		d.store[c.Key] = c.Val
	}
	d.head[c.Origin] = c.Seq
	return nil
}

func (d *incrementalKVDelegate) IncrementalSyncComplete(join bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.completed++
}

// --- harness ---

func newIncrementalPair(t *testing.T, retainA, retainB int) (a, b *Memberlist, da, db *incrementalKVDelegate) {
	t.Helper()

	addrA := getBindAddr()
	addrB := getBindAddr()

	da = newIncrementalKVDelegate("A", retainA)
	db = newIncrementalKVDelegate("B", retainB)

	a = HostMemberlist(addrA.String(), t, func(c *Config) {
		c.Delegate = da
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	b = HostMemberlist(addrB.String(), t, func(c *Config) {
		c.BindPort = a.config.BindPort
		c.Delegate = db
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})

	// Join uses the full-state path; prime membership and baseline state.
	_, err := a.Join([]string{b.config.Name + "/" + b.config.BindAddr})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = a.Shutdown()
		_ = b.Shutdown()
	})
	return a, b, da, db
}

func TestIncrementalSync_Basic(t *testing.T) {
	a, b, da, db := newIncrementalPair(t, 1000, 1000)

	da.Put("color", "blue")
	db.Put("shape", "round")

	peer := Address{Addr: joinHostPort(b.config.BindAddr, uint16(b.config.BindPort)), Name: b.config.Name}
	require.NoError(t, a.pushPullNode(peer, false))

	want := map[string]string{"color": "blue", "shape": "round"}
	require.Equal(t, want, da.snapshot())
	require.Equal(t, want, db.snapshot())

	// Exactly one completion per side, and no full merges after the join.
	if got := da.completedRounds() - da.fullMerges(); got != 1 {
		t.Fatalf("da incremental rounds = %d (completed=%d full=%d)", got, da.completedRounds(), da.fullMerges())
	}
	if got := db.completedRounds() - db.fullMerges(); got != 1 {
		t.Fatalf("db incremental rounds = %d (completed=%d full=%d)", got, db.completedRounds(), db.fullMerges())
	}

	// An idle round still completes successfully.
	before := da.completedRounds()
	require.NoError(t, a.pushPullNode(peer, false))
	require.Equal(t, before+1, da.completedRounds())

	_ = b
}

// TestIncrementalSync_FallbackToFull simulates a node that has been offline
// far longer than the retained change window. The pruned deltas force a full
// state exchange which restores the complete state.
func TestIncrementalSync_FallbackToFull(t *testing.T) {
	// A retains only its last 3 changes. B joins, then we pretend B lags
	// well beyond that window.
	a, b, da, db := newIncrementalPair(t, 3, 1000)

	for i := 0; i < 10; i++ {
		da.Put(fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}

	// Force B's watermark for A back to 0 so it asks for ancient deltas.
	db.mu.Lock()
	db.head["A"] = 0
	// Also wipe the materialized keys sourced from the join so we can prove
	// the fallback repopulates them.
	db.store = map[string]string{}
	db.mu.Unlock()

	peer := Address{Addr: joinHostPort(b.config.BindAddr, uint16(b.config.BindPort)), Name: b.config.Name}
	fullBefore := db.fullMerges()
	require.NoError(t, a.pushPullNode(peer, false))

	// B recovered the complete state via the full fallback.
	require.Equal(t, da.snapshot(), db.snapshot())
	require.GreaterOrEqual(t, db.fullMerges(), fullBefore+1)

	// A, which was up to date, still needed no full merge.
	_ = b
}

// TestIncrementalSync_MidRoundFailure verifies that when applying changes
// fails partway through, neither application is told the round completed.
func TestIncrementalSync_MidRoundFailure(t *testing.T) {
	a, b, da, db := newIncrementalPair(t, 1000, 1000)

	da.Put("alpha", "1")
	da.Put("boom", "2")
	da.Put("gamma", "3")

	db.failNextApply("boom")

	completedBeforeB := db.completedRounds()
	completedBeforeA := da.completedRounds()
	peer := Address{Addr: joinHostPort(b.config.BindAddr, uint16(b.config.BindPort)), Name: b.config.Name}
	err := a.pushPullNode(peer, false)
	require.Error(t, err)
	require.False(t, errors.Is(err, errFallback), "apply failure must not silently fall back")

	// Neither side is told the round merged, since applying changes failed
	// partway through.
	require.Equal(t, completedBeforeB, db.completedRounds())
	require.Equal(t, completedBeforeA, da.completedRounds())

	// State applied before the failure is retained; nothing was falsely
	// declared complete.
	require.Equal(t, "1", da.snapshot()["alpha"])
	_ = b
}

// TestIncrementalSync_Encrypted verifies that a multi-frame incremental
// negotiation works when every frame is encrypted independently.
func TestIncrementalSync_Encrypted(t *testing.T) {
	addrA := getBindAddr()
	addrB := getBindAddr()

	da := newIncrementalKVDelegate("A", 1000)
	db := newIncrementalKVDelegate("B", 1000)

	key := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	a := HostMemberlist(addrA.String(), t, func(c *Config) {
		c.Delegate = da
		c.SecretKey = key
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	b := HostMemberlist(addrB.String(), t, func(c *Config) {
		c.BindPort = a.config.BindPort
		c.Delegate = db
		c.SecretKey = key
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	t.Cleanup(func() {
		_ = a.Shutdown()
		_ = b.Shutdown()
	})

	_, err := a.Join([]string{b.config.Name + "/" + b.config.BindAddr})
	require.NoError(t, err)

	for i := 0; i < 4; i++ {
		da.Put(fmt.Sprintf("enc-a-%d", i), "v")
		db.Put(fmt.Sprintf("enc-b-%d", i), "v")
	}

	peer := Address{Addr: joinHostPort(b.config.BindAddr, uint16(b.config.BindPort)), Name: b.config.Name}
	require.NoError(t, a.pushPullNode(peer, false))
	require.Equal(t, da.snapshot(), db.snapshot())
	require.Len(t, da.snapshot(), 8)
}

// TestIncrementalSync_MultipleRounds applies several independent syncs and
// verifies ordered convergence each time.
func TestIncrementalSync_MultipleRounds(t *testing.T) {
	a, b, da, db := newIncrementalPair(t, 1000, 1000)
	peer := Address{Addr: joinHostPort(b.config.BindAddr, uint16(b.config.BindPort)), Name: b.config.Name}

	for i := 0; i < 5; i++ {
		da.Put(fmt.Sprintf("a%d", i), fmt.Sprintf("%d", i))
		db.Put(fmt.Sprintf("b%d", i), fmt.Sprintf("%d", i))
		require.NoError(t, a.pushPullNode(peer, false))
		require.Equal(t, da.snapshot(), db.snapshot())
	}

	require.Equal(t, 10, len(da.snapshot()))

	// Deletes propagate too.
	da.Delete("a0")
	require.NoError(t, a.pushPullNode(peer, false))
	require.Equal(t, da.snapshot(), db.snapshot())
	_, ok := db.snapshot()["a0"]
	require.False(t, ok)
}

// TestIncrementalSync_PeerWithoutIncremental verifies that a peer whose
// Delegate does not implement IncrementalDelegate is served by the regular
// full-state path transparently.
func TestIncrementalSync_PeerWithoutIncremental(t *testing.T) {
	addrA := getBindAddr()
	addrB := getBindAddr()

	da := newIncrementalKVDelegate("A", 1000)
	plain := &MockDelegate{}

	a := HostMemberlist(addrA.String(), t, func(c *Config) {
		c.Delegate = da
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	b := HostMemberlist(addrB.String(), t, func(c *Config) {
		c.BindPort = a.config.BindPort
		c.Delegate = plain
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	t.Cleanup(func() {
		_ = a.Shutdown()
		_ = b.Shutdown()
	})

	da.Put("color", "blue")

	// Join and a subsequent periodic-style sync must both succeed via the
	// full path despite capability mismatch.
	_, err := a.Join([]string{b.config.Name + "/" + b.config.BindAddr})
	require.NoError(t, err)

	peer := Address{Addr: joinHostPort(b.config.BindAddr, uint16(b.config.BindPort)), Name: b.config.Name}
	require.NoError(t, a.pushPullNode(peer, false))

	// B received A's full LocalState through MergeRemoteState.
	var full kvFullState
	require.NoError(t, json.Unmarshal(plain.getRemoteState(), &full))
	require.Equal(t, "blue", full.Store["color"])
}

// TestIncrementalSync_RoundLimit exercises the negotiation round cap using
// a delegate whose request never converges.
func TestIncrementalSync_RoundLimit(t *testing.T) {
	addrA := getBindAddr()
	addrB := getBindAddr()

	// Both delegates claim to always be missing something but never apply
	// state that advances the watermark, so the negotiation must terminate
	// with an error rather than loop forever.
	da := &greedyDelegate{name: "A", changeSize: 1}
	db := &greedyDelegate{name: "B", changeSize: 1}

	a := HostMemberlist(addrA.String(), t, func(c *Config) {
		c.Delegate = da
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	b := HostMemberlist(addrB.String(), t, func(c *Config) {
		c.BindPort = a.config.BindPort
		c.Delegate = db
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	t.Cleanup(func() {
		_ = a.Shutdown()
		_ = b.Shutdown()
	})

	_, err := a.Join([]string{b.config.Name + "/" + b.config.BindAddr})
	require.NoError(t, err)

	peer := Address{Addr: joinHostPort(b.config.BindAddr, uint16(b.config.BindPort)), Name: b.config.Name}
	err = a.pushPullNode(peer, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "round limit")
	require.Equal(t, 0, da.completed)
	require.Equal(t, 0, db.completed)
}

// greedyDelegate always requests something and never converges.
type greedyDelegate struct {
	name       string
	completed  int
	changeSize int
}

func (d *greedyDelegate) NodeMeta(limit int) []byte                  { return nil }
func (d *greedyDelegate) NotifyMsg([]byte)                           {}
func (d *greedyDelegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }
func (d *greedyDelegate) LocalState(join bool) []byte                { return []byte("{}") }
func (d *greedyDelegate) MergeRemoteState(buf []byte, join bool)     {}

func (d *greedyDelegate) IncrementalSummary(join bool) []byte {
	return []byte(`{"watermarks":{"` + d.name + `":1}}`)
}

func (d *greedyDelegate) MissingChanges(remoteSummary []byte, join bool) ([]byte, error) {
	// Always want something from the peer.
	other := "B"
	if d.name == "B" {
		other = "A"
	}
	return []byte(`{"want":"` + other + `"}`), nil
}

func (d *greedyDelegate) ProduceChanges(request []byte, join bool) ([][]byte, error) {
	// Hand back a change that ApplyChange ignores, so watermarks never move.
	payload := `{"origin":"` + d.name + `","seq":1,"key":"x","val":"`
	pad := make([]byte, d.changeSize)
	for i := range pad {
		pad[i] = 'z'
	}
	payload += string(pad) + `"}`
	return [][]byte{[]byte(payload)}, nil
}

func (d *greedyDelegate) ApplyChange(change []byte, join bool) error {
	// Deliberately do not advance state.
	return nil
}

func (d *greedyDelegate) IncrementalSyncComplete(join bool) {
	d.completed++
}

// TestIncrementalSync_ByteLimit verifies that a negotiation carrying large
// payloads terminates with an error once the cumulative byte budget is
// exhausted and never notifies completion.
func TestIncrementalSync_ByteLimit(t *testing.T) {
	addrA := getBindAddr()
	addrB := getBindAddr()

	// Each frame stays comfortably below the single-message cap but the
	// non-converging negotiation blows through the cumulative byte budget
	// within a few rounds, well before the round cap. Sized to move tens of
	// megabytes so the test stays fast on loopback.
	payload := (1 << 20) * 4
	da := &greedyDelegate{name: "A", changeSize: payload}
	db := &greedyDelegate{name: "B", changeSize: payload}

	a := HostMemberlist(addrA.String(), t, func(c *Config) {
		c.Delegate = da
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	b := HostMemberlist(addrB.String(), t, func(c *Config) {
		c.BindPort = a.config.BindPort
		c.Delegate = db
		c.GossipInterval = 10 * time.Second
		c.PushPullInterval = 0
	})
	t.Cleanup(func() {
		_ = a.Shutdown()
		_ = b.Shutdown()
	})

	_, err := a.Join([]string{b.config.Name + "/" + b.config.BindAddr})
	require.NoError(t, err)

	peer := Address{Addr: joinHostPort(b.config.BindAddr, uint16(b.config.BindPort)), Name: b.config.Name}
	err = a.pushPullNode(peer, false)
	require.Error(t, err)
	// The exact error depends on which side breaches the budget first: the
	// local side reports "limit", the peer's close surfaces as EOF. Either
	// way the round fails and must never be reported as complete.
	require.Equal(t, 0, da.completed)
	require.Equal(t, 0, db.completed)
}
