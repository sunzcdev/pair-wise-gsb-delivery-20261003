// Derived via LLM from Apache Kafka's TransactionsWithMaxInFlightOneTest.java (Apache 2.0).
// https://github.com/apache/kafka/blob/trunk/clients/clients-integration-tests/src/test/java/org/apache/kafka/clients/producer/TransactionsWithMaxInFlightOneTest.java

package kafka_tests

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// TestTransactionalProducerSingleBrokerMaxInFlightOne tests the transactional
// producer with MaxProduceRequestsInflightPerBroker=1. It produces an aborted
// transaction and a committed transaction, then verifies that a
// read_committed consumer sees only the committed records.
func TestTransactionalProducerSingleBrokerMaxInFlightOne(t *testing.T) {
	t.Parallel()
	topic := "txn-inflight-one"
	c := newCluster(t, kfake.NumBrokers(1), kfake.SeedTopics(1, topic))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	txnProducer := newClient848(t, c,
		kgo.DefaultProduceTopic(topic),
		kgo.TransactionalID("txn-inflight-test"),
		kgo.MaxProduceRequestsInflightPerBroker(1),
	)

	// Transaction 1: produce and abort.
	if err := txnProducer.BeginTransaction(); err != nil {
		t.Fatalf("begin txn 1: %v", err)
	}
	for i := range 3 {
		r := kgo.StringRecord("aborted-" + strconv.Itoa(i))
		if err := txnProducer.ProduceSync(ctx, r).FirstErr(); err != nil {
			t.Fatalf("produce aborted record %d: %v", i, err)
		}
	}
	if err := txnProducer.EndTransaction(ctx, kgo.TryAbort); err != nil {
		t.Fatalf("abort txn 1: %v", err)
	}

	// Transaction 2: produce and commit.
	if err := txnProducer.BeginTransaction(); err != nil {
		t.Fatalf("begin txn 2: %v", err)
	}
	for i := range 3 {
		r := kgo.StringRecord("committed-" + strconv.Itoa(i))
		if err := txnProducer.ProduceSync(ctx, r).FirstErr(); err != nil {
			t.Fatalf("produce committed record %d: %v", i, err)
		}
	}
	if err := txnProducer.EndTransaction(ctx, kgo.TryCommit); err != nil {
		t.Fatalf("commit txn 2: %v", err)
	}

	// Verify read_committed consumer only sees committed records.
	consumer := newClient848(t, c,
		kgo.ConsumeTopics(topic),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
	)
	records := consumeN(t, consumer, 3, 5*time.Second)
	if len(records) != 3 {
		t.Fatalf("expected 3 committed records, got %d", len(records))
	}
	for _, r := range records {
		v := string(r.Value)
		if len(v) >= 7 && v[:7] == "aborted" {
			t.Errorf("read_committed consumer saw aborted record: %s", v)
		}
	}

	// Verify read_uncommitted consumer sees all 6 data records (aborted + committed).
	uncommittedConsumer := newClient848(t, c,
		kgo.ConsumeTopics(topic),
		kgo.FetchIsolationLevel(kgo.ReadUncommitted()),
	)
	allRecords := consumeN(t, uncommittedConsumer, 6, 5*time.Second)
	if len(allRecords) != 6 {
		t.Fatalf("expected 6 total records with read_uncommitted, got %d", len(allRecords))
	}
}

// TestEndTransactionResultDroppedCommitResponse drops every EndTxn response
// after the broker applies it. The commit lands broker-side, but the client
// only sees transport errors: EndTransactionWithResult must report the
// outcome as unknown (never a false commit, never a false abort), keeping
// the send-phase error as evidence. VerifyTransactionEnd then confirms the
// commit through the coordinator -- read-only, without resending anything.
func TestEndTransactionResultDroppedCommitResponse(t *testing.T) {
	t.Parallel()
	topic := "txn-end-result-commit"
	c := newCluster(t, kfake.NumBrokers(1), kfake.SeedTopics(1, topic))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cl := newPlainClient(t, c,
		kgo.TransactionalID("txn-end-result-commit"),
		kgo.RequestRetries(2),
	)

	if err := cl.BeginTransaction(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	r := kgo.StringRecord("committed-record")
	r.Topic = topic
	produceSync(t, cl, r)
	if err := cl.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	drop := c.Fault(kfake.Fault{
		Keys:         []kmsg.Key{kmsg.EndTxn},
		DropResponse: true,
		Count:        -1,
	})
	res := cl.EndTransactionWithResult(ctx, kgo.TryCommit)
	drop.Remove()

	if !res.Unknown() {
		t.Fatalf("dropped commit response must be unknown, got %v (err %v)", res.State, res.Err)
	}
	if res.Err == nil {
		t.Fatal("unknown outcome must keep the send-phase error as evidence")
	}

	// The broker applied the commit before dropping the response: a
	// read_committed consumer sees the record. The unknown result was
	// honest -- the commit really did land.
	consumer := newPlainClient(t, c,
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.FetchMaxWait(250*time.Millisecond),
	)
	if records := consumeN(t, consumer, 1, 10*time.Second); len(records) != 1 {
		t.Fatalf("expected the committed record to be visible, got %d", len(records))
	}

	// Verification is read-only: no produce, no EndTxn, no partition
	// re-registration may be sent to probe the outcome.
	obs := c.Fault(kfake.Fault{
		Keys:    []kmsg.Key{kmsg.Produce, kmsg.EndTxn, kmsg.AddPartitionsToTxn},
		Observe: true,
		Count:   -1,
	})
	vres, err := cl.VerifyTransactionEnd(ctx)
	obs.Remove()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vres.Committed() {
		t.Fatalf("expected verified committed, got %v", vres.State)
	}
	if hits := obs.Hits(); hits != 0 {
		t.Fatalf("verify must not resend business requests, saw %d", hits)
	}
}

// TestEndTransactionResultDroppedAbortResponse is the abort twin of the
// commit test: the abort lands broker-side, the client sees only transport
// errors and must report unknown, and verification then confirms the abort.
func TestEndTransactionResultDroppedAbortResponse(t *testing.T) {
	t.Parallel()
	topic := "txn-end-result-abort"
	c := newCluster(t, kfake.NumBrokers(1), kfake.SeedTopics(1, topic))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cl := newPlainClient(t, c,
		kgo.TransactionalID("txn-end-result-abort"),
		kgo.RequestRetries(2),
	)

	if err := cl.BeginTransaction(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	r := kgo.StringRecord("aborted-record")
	r.Topic = topic
	produceSync(t, cl, r)
	if err := cl.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	drop := c.Fault(kfake.Fault{
		Keys:         []kmsg.Key{kmsg.EndTxn},
		DropResponse: true,
		Count:        -1,
	})
	res := cl.EndTransactionWithResult(ctx, kgo.TryAbort)
	drop.Remove()

	if !res.Unknown() {
		t.Fatalf("dropped abort response must be unknown, got %v (err %v)", res.State, res.Err)
	}
	if res.Err == nil {
		t.Fatal("unknown outcome must keep the send-phase error as evidence")
	}

	vres, err := cl.VerifyTransactionEnd(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vres.Aborted() {
		t.Fatalf("expected verified aborted, got %v", vres.State)
	}

	// The abort landed: read_committed sees nothing.
	consumer := newPlainClient(t, c,
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.FetchMaxWait(250*time.Millisecond),
	)
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pollCancel()
	var n int
	for pollCtx.Err() == nil && n == 0 {
		fs := consumer.PollFetches(pollCtx)
		fs.EachRecord(func(*kgo.Record) { n++ })
	}
	if n != 0 {
		t.Fatalf("read_committed consumer saw %d records from an aborted transaction", n)
	}
}

// TestVerifyTransactionEndUnavailable drops DescribeTransactions responses
// too: verification cannot run, so the outcome must stay unknown -- an
// unverifiable result is never upgraded to a guess.
func TestVerifyTransactionEndUnavailable(t *testing.T) {
	t.Parallel()
	topic := "txn-end-result-unverifiable"
	c := newCluster(t, kfake.NumBrokers(1), kfake.SeedTopics(1, topic))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cl := newPlainClient(t, c,
		kgo.TransactionalID("txn-end-result-unverifiable"),
		kgo.RequestRetries(2),
	)

	if err := cl.BeginTransaction(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	r := kgo.StringRecord("maybe-committed-record")
	r.Topic = topic
	produceSync(t, cl, r)
	if err := cl.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	drop := c.Fault(kfake.Fault{
		Keys:         []kmsg.Key{kmsg.EndTxn, kmsg.DescribeTransactions},
		DropResponse: true,
		Count:        -1,
	})
	res := cl.EndTransactionWithResult(ctx, kgo.TryCommit)
	if !res.Unknown() {
		t.Fatalf("dropped commit response must be unknown, got %v", res.State)
	}

	vres, err := cl.VerifyTransactionEnd(ctx)
	drop.Remove()
	if err == nil {
		t.Fatal("expected verify to fail while DescribeTransactions responses are dropped")
	}
	if !vres.Unknown() {
		t.Fatalf("unverifiable outcome must stay unknown, got %v", vres.State)
	}
}

// TestVerifyTransactionEndNothingToVerify covers the cases where the
// coordinator has no completed transaction to report: a transaction that
// ended as a no-op (Empty) and a transactional ID the broker has never seen
// (TRANSACTIONAL_ID_NOT_FOUND). Both stay unknown rather than guessing.
func TestVerifyTransactionEndNothingToVerify(t *testing.T) {
	t.Parallel()
	c := newCluster(t, kfake.NumBrokers(1))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cl := newPlainClient(t, c, kgo.TransactionalID("txn-end-result-empty"))

	// Begin loads the producer ID broker-side; ending with nothing
	// produced is a vacuous commit that sends nothing.
	if err := cl.BeginTransaction(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if res := cl.EndTransactionWithResult(ctx, kgo.TryCommit); !res.Committed() {
		t.Fatalf("vacuous commit should be committed, got %v (err %v)", res.State, res.Err)
	}

	// The broker knows the transactional ID but has no completed
	// transaction for it (Empty): nothing to verify, stays unknown.
	vres, err := cl.VerifyTransactionEnd(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vres.Unknown() {
		t.Fatalf("empty broker-side transaction must stay unknown, got %v", vres.State)
	}

	// A transactional ID the broker has never seen cannot be verified.
	cl2 := newPlainClient(t, c, kgo.TransactionalID("txn-end-result-never-seen"))
	vres2, err := cl2.VerifyTransactionEnd(ctx)
	if err == nil {
		t.Fatal("expected an error for a transactional ID the broker has never seen")
	}
	if !vres2.Unknown() {
		t.Fatalf("unverifiable outcome must stay unknown, got %v", vres2.State)
	}
}
