package sserecovery

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrackerSeedAndSnapshot(t *testing.T) {
	tr := NewTracker()

	// A fully recovered subscription seeds its position immediately.
	tr.Seed("a", "ep1", 10, true)
	// A subscription whose history expired stays unknown until a full delivery.
	tr.Seed("b", "ep2", 0, false)

	chs := tr.Snapshot()
	require.Equal(t, []Channel{{Channel: "a", Epoch: "ep1", Offset: 10}}, chs)
	require.Equal(t, []string{"a", "b"}, tr.KnownChannels())

	// A complete delivery makes b known.
	tr.Advance("b", "ep2", 21)
	require.Equal(t, []Channel{
		{Channel: "a", Epoch: "ep1", Offset: 10},
		{Channel: "b", Epoch: "ep2", Offset: 21},
	}, tr.Snapshot())
}

func TestTrackerMonotonicAdvance(t *testing.T) {
	tr := NewTracker()
	tr.Seed("a", "ep1", 5, true)
	// A smaller offset must not move the delivered position backwards.
	tr.Advance("a", "ep1", 3)
	require.Equal(t, uint64(5), tr.Snapshot()[0].Offset)

	tr.Advance("a", "ep1", 9)
	require.Equal(t, uint64(9), tr.Snapshot()[0].Offset)

	// Offset zero and empty channel are ignored.
	tr.Advance("a", "ep1", 0)
	tr.Advance("", "ep1", 100)
	require.Equal(t, uint64(9), tr.Snapshot()[0].Offset)
}

func TestTrackerEpochChange(t *testing.T) {
	tr := NewTracker()
	tr.Seed("a", "ep1", 5, true)
	// A new epoch takes over at its own offset rather than comparing across
	// unrelated streams.
	tr.Advance("a", "ep2", 2)
	snap := tr.Snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "ep2", snap[0].Epoch)
	require.Equal(t, uint64(2), snap[0].Offset)
}

func TestTrackerFreshChannelAdvance(t *testing.T) {
	tr := NewTracker()
	// Advancing a channel that was never seeded still records it (live
	// publication for a subscription tracked under its numeric id mapping).
	tr.Advance("a", "ep1", 7)
	require.Equal(t, []Channel{{Channel: "a", Epoch: "ep1", Offset: 7}}, tr.Snapshot())
}
