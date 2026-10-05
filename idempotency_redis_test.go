//go:build integration

package centrifuge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRedisBrokerStrictIdempotent(t *testing.T) {
	t.Parallel()
	for _, tt := range historyRedisTests {
		t.Run(tt.Name, func(t *testing.T) {
			node := testNode(t)
			b := newTestRedisBroker(t, node, tt.UseStreams, tt.UseCluster, tt.Port)
			defer func() { _ = node.Shutdown(context.Background()) }()
			defer stopRedisBroker(b)

			channel := uuid.NewString()

			strictOpts := func(withHistory bool) PublishOptions {
				o := PublishOptions{
					IdempotencyKey:    "key",
					StrictIdempotency: true,
					Tags:              map[string]string{"t": "v"},
				}
				if withHistory {
					o.HistorySize = 10
					o.HistoryTTL = time.Minute
				}
				return o
			}

			// Same content retry (with history): first result is returned.
			opts := strictOpts(true)
			r1, err := b.Publish(channel, []byte("a"), opts)
			require.NoError(t, err)
			require.False(t, r1.Suppressed)
			require.NotZero(t, r1.Offset)

			opts2 := strictOpts(true)
			opts2.Tags = map[string]string{"t": "v"} // same logical tags
			r2, err := b.Publish(channel, []byte("a"), opts2)
			require.NoError(t, err)
			require.True(t, r2.Suppressed)
			require.Equal(t, SuppressReasonIdempotency, r2.SuppressReason)
			require.Equal(t, r1.StreamPosition, r2.StreamPosition)

			// Different data: recognizable conflict, old position not reused.
			r3, err := b.Publish(channel, []byte("different"), strictOpts(true))
			require.ErrorIs(t, err, ErrIdempotencyConflict)
			require.Equal(t, PublishResult{}, r3)

			// Different tags: conflict too.
			opts4 := strictOpts(true)
			opts4.Tags = map[string]string{"t": "other"}
			_, err = b.Publish(channel, []byte("a"), opts4)
			require.ErrorIs(t, err, ErrIdempotencyConflict)

			pubs, _, err := b.History(channel, HistoryOptions{Filter: HistoryFilter{Limit: -1}})
			require.NoError(t, err)
			require.Len(t, pubs, 1)
			require.Equal(t, []byte("a"), pubs[0].Data)

			// Same key, different channel: independent.
			otherChannel := uuid.NewString()
			_, err = b.Publish(otherChannel, []byte("z"), strictOpts(true))
			require.NoError(t, err)

			// No-history path on a fresh channel: conflict there as well.
			noHistoryChannel := uuid.NewString()
			_, err = b.Publish(noHistoryChannel, []byte("a"), strictOpts(false))
			require.NoError(t, err)
			_, err = b.Publish(noHistoryChannel, []byte("b"), strictOpts(false))
			require.ErrorIs(t, err, ErrIdempotencyConflict)
			_, err = b.Publish(noHistoryChannel, []byte("a"), strictOpts(false))
			require.NoError(t, err)
		})
	}
}

func TestRedisBrokerStrictIdempotentExpiryReoccupy(t *testing.T) {
	t.Parallel()
	for _, tt := range historyRedisTests {
		t.Run(tt.Name, func(t *testing.T) {
			node := testNode(t)
			b := newTestRedisBroker(t, node, tt.UseStreams, tt.UseCluster, tt.Port)
			defer func() { _ = node.Shutdown(context.Background()) }()
			defer stopRedisBroker(b)

			channel := uuid.NewString()
			opts := PublishOptions{
				IdempotencyKey:      "key",
				StrictIdempotency:   true,
				IdempotentResultTTL: time.Second,
				HistorySize:         10,
				HistoryTTL:          time.Minute,
			}
			_, err := b.Publish(channel, []byte("first"), opts)
			require.NoError(t, err)
			_, err = b.Publish(channel, []byte("second"), opts)
			require.ErrorIs(t, err, ErrIdempotencyConflict)

			require.Eventually(t, func() bool {
				_, err := b.Publish(channel, []byte("second"), opts)
				return err == nil
			}, 10*time.Second, 100*time.Millisecond)
		})
	}
}

// TestRedisBrokerStrictIdempotentConcurrentDifferentContent races two
// different payloads under one key. Redis executes the Lua script atomically,
// so exactly one request must win and enter history.
func TestRedisBrokerStrictIdempotentConcurrentDifferentContent(t *testing.T) {
	t.Parallel()
	for _, tt := range historyRedisTests {
		t.Run(tt.Name, func(t *testing.T) {
			node := testNode(t)
			b := newTestRedisBroker(t, node, tt.UseStreams, tt.UseCluster, tt.Port)
			defer func() { _ = node.Shutdown(context.Background()) }()
			defer stopRedisBroker(b)

			const iterations = 25
			for i := 0; i < iterations; i++ {
				channel := fmt.Sprintf("%s-%d", uuid.NewString(), i)
				opts := PublishOptions{
					IdempotencyKey:    "key",
					StrictIdempotency: true,
					HistorySize:       10,
					HistoryTTL:        time.Minute,
				}

				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				var successes, conflicts int64
				var mu sync.Mutex

				publish := func(data string) {
					defer wg.Done()
					<-start
					r, err := b.Publish(channel, []byte(data), opts)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case errors.Is(err, ErrIdempotencyConflict):
						conflicts++
						require.Equal(t, PublishResult{}, r)
					case err == nil:
						successes++
					default:
						t.Errorf("unexpected error: %v", err)
					}
				}
				go publish("payload-a")
				go publish("payload-b")

				close(start)
				wg.Wait()

				require.Equal(t, int64(1), successes, "exactly one publish must enter history")
				require.Equal(t, int64(1), conflicts, "exactly one publish must conflict")

				pubs, _, err := b.History(channel, HistoryOptions{Filter: HistoryFilter{Limit: -1}})
				require.NoError(t, err)
				require.Len(t, pubs, 1)
				require.Contains(t, []string{"payload-a", "payload-b"}, string(pubs[0].Data))
			}
		})
	}
}

// TestRedisBrokerLegacyIdempotencyDifferentContent keeps the pre-strict
// behavior pinned: without StrictIdempotency a different payload under the same
// key is deduplicated to the first result and never errors.
func TestRedisBrokerLegacyIdempotencyDifferentContent(t *testing.T) {
	t.Parallel()
	for _, tt := range historyRedisTests {
		t.Run(tt.Name, func(t *testing.T) {
			node := testNode(t)
			b := newTestRedisBroker(t, node, tt.UseStreams, tt.UseCluster, tt.Port)
			defer func() { _ = node.Shutdown(context.Background()) }()
			defer stopRedisBroker(b)

			channel := uuid.NewString()
			opts := PublishOptions{IdempotencyKey: "key", HistorySize: 10, HistoryTTL: time.Minute}

			r1, err := b.Publish(channel, []byte("first"), opts)
			require.NoError(t, err)
			r2, err := b.Publish(channel, []byte("different"), opts)
			require.NoError(t, err)
			require.True(t, r2.Suppressed)
			require.Equal(t, SuppressReasonIdempotency, r2.SuppressReason)
			require.Equal(t, r1.StreamPosition, r2.StreamPosition)

			pubs, _, err := b.History(channel, HistoryOptions{Filter: HistoryFilter{Limit: -1}})
			require.NoError(t, err)
			require.Len(t, pubs, 1)

			// Legacy followed by strict request: the cached entry has no
			// digest, so strict mode treats it leniently (upgrade-safe).
			strictOpts := opts
			strictOpts.StrictIdempotency = true
			r3, err := b.Publish(channel, []byte("different-again"), strictOpts)
			require.NoError(t, err)
			require.True(t, r3.Suppressed)
		})
	}
}
