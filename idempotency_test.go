package centrifuge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishContentDigestDeterministic(t *testing.T) {
	t.Parallel()

	base := PublishOptions{
		Tags: map[string]string{
			"a": "1",
			"b": "2",
			"c": "3",
		},
		ClientInfo:   &ClientInfo{ClientID: "c", UserID: "u", ConnInfo: []byte("conn"), ChanInfo: []byte("chan")},
		UseDelta:     true,
		Version:      7,
		VersionEpoch: "epoch",
		Key:          "key",
		Removed:      true,
	}
	data := []byte("payload")

	d1 := publishContentDigest(data, base)

	// Same content, tags built in a different order: identical request must
	// always yield the same digest.
	reordered := base
	reordered.Tags = map[string]string{
		"c": "3",
		"a": "1",
		"b": "2",
	}
	d2 := publishContentDigest(data, reordered)
	require.True(t, bytes.Equal(d1, d2))

	// Every content-identity field must participate in the digest.
	cases := []struct {
		name string
		data []byte
		opts PublishOptions
	}{
		{"data", []byte("different"), base},
		{"tag_value", data, func() PublishOptions { o := base; o.Tags = map[string]string{"a": "x", "b": "2", "c": "3"}; return o }()},
		{"tag_key", data, func() PublishOptions { o := base; o.Tags = map[string]string{"a": "1", "b": "2", "z": "3"}; return o }()},
		{"tags_added", data, func() PublishOptions {
			o := base
			o.Tags = map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}
			return o
		}()},
		{"tags_removed", data, func() PublishOptions { o := base; o.Tags = map[string]string{"a": "1", "b": "2"}; return o }()},
		{"client_info", data, func() PublishOptions { o := base; o.ClientInfo = &ClientInfo{ClientID: "other", UserID: "u"}; return o }()},
		{"conn_info", data, func() PublishOptions {
			o := base
			o.ClientInfo = &ClientInfo{ClientID: "c", UserID: "u", ConnInfo: []byte("other")}
			return o
		}()},
		{"use_delta", data, func() PublishOptions { o := base; o.UseDelta = false; return o }()},
		{"version", data, func() PublishOptions { o := base; o.Version = 8; return o }()},
		{"version_epoch", data, func() PublishOptions { o := base; o.VersionEpoch = "other"; return o }()},
		{"key", data, func() PublishOptions { o := base; o.Key = "other"; return o }()},
		{"removed", data, func() PublishOptions { o := base; o.Removed = false; return o }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, bytes.Equal(d1, publishContentDigest(tc.data, tc.opts)))
		})
	}

	// Fields unrelated to content must NOT affect the digest.
	storageOnly := base
	storageOnly.HistorySize = 42
	storageOnly.HistoryTTL = 42 * time.Second
	storageOnly.HistoryMetaTTL = 43 * time.Second
	storageOnly.IdempotencyKey = "k"
	storageOnly.IdempotentResultTTL = 44 * time.Second
	require.True(t, bytes.Equal(d1, publishContentDigest(data, storageOnly)))

	// Nil and empty tags collapse to the same identity, and nil vs non-nil
	// ClientInfo differ.
	require.True(t, bytes.Equal(
		publishContentDigest(nil, PublishOptions{}),
		publishContentDigest(nil, PublishOptions{Tags: map[string]string{}}),
	))
}

func TestMemoryBrokerStrictIdempotentRetrySameContent(t *testing.T) {
	t.Parallel()
	for _, withHistory := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%v", withHistory), func(t *testing.T) {
			e := testMemoryBroker()
			defer func() { _ = e.node.Shutdown(context.Background()) }()

			numPubs := 0
			e.eventHandler = &testBrokerEventHandler{
				HandlePublicationFunc: func(ch string, pub *Publication, sp StreamPosition, delta bool, prevPub *Publication) error {
					numPubs++
					return nil
				},
			}

			opts := PublishOptions{
				IdempotencyKey:    "k",
				StrictIdempotency: true,
				Tags:              map[string]string{"x": "y"},
			}
			if withHistory {
				opts.HistorySize = 4
				opts.HistoryTTL = time.Minute
			}

			r1, err := e.Publish("ch", []byte("data"), opts)
			require.NoError(t, err)
			require.False(t, r1.Suppressed)

			// Same content, tags map rebuilt in another order.
			opts.Tags = map[string]string{"x": "y"}
			r2, err := e.Publish("ch", []byte("data"), opts)
			require.NoError(t, err)
			require.True(t, r2.Suppressed)
			require.Equal(t, SuppressReasonIdempotency, r2.SuppressReason)
			require.Equal(t, r1.StreamPosition, r2.StreamPosition)
			require.Equal(t, 1, numPubs)

			if withHistory {
				pubs, _, err := e.History("ch", HistoryOptions{Filter: HistoryFilter{Limit: -1}})
				require.NoError(t, err)
				require.Len(t, pubs, 1)
			}
		})
	}
}

func TestMemoryBrokerStrictIdempotentContentConflict(t *testing.T) {
	t.Parallel()
	for _, withHistory := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%v", withHistory), func(t *testing.T) {
			e := testMemoryBroker()
			defer func() { _ = e.node.Shutdown(context.Background()) }()

			numPubs := 0
			e.eventHandler = &testBrokerEventHandler{
				HandlePublicationFunc: func(ch string, pub *Publication, sp StreamPosition, delta bool, prevPub *Publication) error {
					numPubs++
					return nil
				},
			}

			opts := func() PublishOptions {
				o := PublishOptions{IdempotencyKey: "k", StrictIdempotency: true}
				if withHistory {
					o.HistorySize = 4
					o.HistoryTTL = time.Minute
				}
				return o
			}

			r1, err := e.Publish("ch", []byte("first"), opts())
			require.NoError(t, err)

			r2, err := e.Publish("ch", []byte("different content"), opts())
			require.ErrorIs(t, err, ErrIdempotencyConflict)
			require.Equal(t, PublishResult{}, r2, "conflict must not report the old StreamPosition")

			// The first result stays usable for a genuine identical retry.
			r3, err := e.Publish("ch", []byte("first"), opts())
			require.NoError(t, err)
			require.True(t, r3.Suppressed)
			require.Equal(t, r1.StreamPosition, r3.StreamPosition)

			// Tags and content-affecting options also trigger the conflict.
			o := opts()
			o.Tags = map[string]string{"a": "b"}
			_, err = e.Publish("ch", []byte("first"), o)
			require.ErrorIs(t, err, ErrIdempotencyConflict)

			require.Equal(t, 1, numPubs)

			if withHistory {
				pubs, _, err := e.History("ch", HistoryOptions{Filter: HistoryFilter{Limit: -1}})
				require.NoError(t, err)
				require.Len(t, pubs, 1)
				require.Equal(t, []byte("first"), pubs[0].Data)
			}
		})
	}
}

func TestMemoryBrokerStrictIdempotentKeyIsChannelScoped(t *testing.T) {
	t.Parallel()
	e := testMemoryBroker()
	defer func() { _ = e.node.Shutdown(context.Background()) }()

	opts := PublishOptions{IdempotencyKey: "k", StrictIdempotency: true}
	_, err := e.Publish("ch1", []byte("a"), opts)
	require.NoError(t, err)
	// Same key, different channel: no conflict.
	_, err = e.Publish("ch2", []byte("b"), opts)
	require.NoError(t, err)
}

func TestMemoryBrokerStrictIdempotentReoccupyAfterExpiry(t *testing.T) {
	t.Parallel()
	e := testMemoryBroker()
	defer func() { _ = e.node.Shutdown(context.Background()) }()

	opts := PublishOptions{
		IdempotencyKey:      "k",
		StrictIdempotency:   true,
		IdempotentResultTTL: time.Second,
	}
	_, err := e.Publish("ch", []byte("first"), opts)
	require.NoError(t, err)
	_, err = e.Publish("ch", []byte("second"), opts)
	require.ErrorIs(t, err, ErrIdempotencyConflict)

	require.Eventually(t, func() bool {
		_, err := e.Publish("ch", []byte("second"), opts)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "key must become reusable after the cached result expires")
}

// TestMemoryBrokerStrictIdempotentConcurrentDifferentContent publishes two
// different payloads under one key concurrently. Exactly one must win: the
// loser gets ErrIdempotencyConflict, only one publication reaches history.
func TestMemoryBrokerStrictIdempotentConcurrentDifferentContent(t *testing.T) {
	t.Parallel()

	e := testMemoryBroker()
	defer func() { _ = e.node.Shutdown(context.Background()) }()

	numPubs := int32(0)
	e.eventHandler = &testBrokerEventHandler{
		HandlePublicationFunc: func(ch string, pub *Publication, sp StreamPosition, delta bool, prevPub *Publication) error {
			atomic.AddInt32(&numPubs, 1)
			return nil
		},
	}

	const iterations = 50
	for i := 0; i < iterations; i++ {
		channel := fmt.Sprintf("ch_%d", i)
		optsA := PublishOptions{
			IdempotencyKey:    "k",
			StrictIdempotency: true,
			HistorySize:       10,
			HistoryTTL:        time.Minute,
		}
		optsB := optsA

		var wg sync.WaitGroup
		wg.Add(2)
		var mu sync.Mutex
		var results []PublishResult
		var errs []error
		start := make(chan struct{})

		go func() {
			defer wg.Done()
			<-start
			r, err := e.Publish(channel, []byte("payload-a"), optsA)
			mu.Lock()
			results, errs = append(results, r), append(errs, err)
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			<-start
			r, err := e.Publish(channel, []byte("payload-b"), optsB)
			mu.Lock()
			results, errs = append(results, r), append(errs, err)
			mu.Unlock()
		}()

		close(start)
		wg.Wait()

		var conflicts, successes int
		for j, err := range errs {
			switch {
			case errors.Is(err, ErrIdempotencyConflict):
				conflicts++
				require.Equal(t, PublishResult{}, results[j], "conflict result must be zero-valued")
			case err == nil:
				successes++
				require.False(t, results[j].Suppressed)
				require.NotZero(t, results[j].Offset)
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		require.Equal(t, 1, successes, "exactly one concurrent publish must enter history")
		require.Equal(t, 1, conflicts, "the losing publish must report a conflict")

		pubs, _, err := e.History(channel, HistoryOptions{Filter: HistoryFilter{Limit: -1}})
		require.NoError(t, err)
		require.Len(t, pubs, 1)
		require.Contains(t, []string{"payload-a", "payload-b"}, string(pubs[0].Data))

	}
	require.Equal(t, int32(iterations), atomic.LoadInt32(&numPubs),
		"exactly one publication per race must be delivered")
}

// TestMemoryBrokerLegacyIdempotencyDifferentContent locks in the old behavior:
// without strict mode a different payload under the same key is silently
// deduplicated to the first result.
func TestMemoryBrokerLegacyIdempotencyDifferentContent(t *testing.T) {
	t.Parallel()
	for _, withHistory := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%v", withHistory), func(t *testing.T) {
			e := testMemoryBroker()
			defer func() { _ = e.node.Shutdown(context.Background()) }()

			numPubs := 0
			e.eventHandler = &testBrokerEventHandler{
				HandlePublicationFunc: func(ch string, pub *Publication, sp StreamPosition, delta bool, prevPub *Publication) error {
					numPubs++
					return nil
				},
			}

			opts := PublishOptions{IdempotencyKey: "k"}
			if withHistory {
				opts.HistorySize = 4
				opts.HistoryTTL = time.Minute
			}

			r1, err := e.Publish("ch", []byte("first"), opts)
			require.NoError(t, err)
			r2, err := e.Publish("ch", []byte("totally different"), opts)
			require.NoError(t, err)
			require.True(t, r2.Suppressed)
			require.Equal(t, SuppressReasonIdempotency, r2.SuppressReason)
			require.Equal(t, r1.StreamPosition, r2.StreamPosition)
			require.Equal(t, 1, numPubs)
		})
	}
}
