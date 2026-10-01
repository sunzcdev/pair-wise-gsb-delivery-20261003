package kfake

// Tests for kgo's structured transaction end result (EndTransactionResult)
// and explicit outcome verification (VerifyTransactionEnd): a commit whose
// response the broker drops after applying it must be judged unknown, and
// verification must either prove the outcome or stay unknown.

import (
	"context"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// beginAndProduce starts a transaction with one produced record, so the
// transaction is definitely begun broker-side.
func beginAndProduce(t *testing.T, cl *kgo.Client, topic string) {
	t.Helper()
	if err := cl.BeginTransaction(); err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	r := &kgo.Record{Topic: topic, Value: []byte("v")}
	if err := cl.ProduceSync(context.Background(), r).FirstErr(); err != nil {
		t.Fatalf("ProduceSync: %v", err)
	}
}

// A confirmed commit is judged committed, a confirmed abort is judged
// aborted, and both carry the send-phase evidence of a completed exchange.
func TestTxnEndResultCommitAndAbort(t *testing.T) {
	t.Parallel()
	c := newCluster(t, NumBrokers(1), SeedTopics(1, "end-result-ok"))

	cl := newPlainClient(t, c, kgo.TransactionalID("end-result-ok"))

	beginAndProduce(t, cl, "end-result-ok")
	res := cl.EndTransactionResult(context.Background(), kgo.TryCommit)
	if res.Outcome != kgo.TxnEndCommitted {
		t.Fatalf("commit outcome: got %v, want committed (err %v)", res.Outcome, res.Err)
	}
	if res.Err != nil {
		t.Fatalf("commit err: %v", res.Err)
	}
	if !res.RequestSent || !res.ResponseReceived {
		t.Fatalf("commit evidence: sent=%v received=%v, want both true", res.RequestSent, res.ResponseReceived)
	}
	if res.ProducerID < 0 {
		t.Fatalf("commit evidence: producer id %d, want >= 0", res.ProducerID)
	}
	if outcome, err := cl.VerifyTransactionEnd(context.Background(), res); err != nil || outcome != kgo.TxnEndCommitted {
		t.Fatalf("verify committed: got %v, %v", outcome, err)
	}

	beginAndProduce(t, cl, "end-result-ok")
	res = cl.EndTransactionResult(context.Background(), kgo.TryAbort)
	if res.Outcome != kgo.TxnEndAborted {
		t.Fatalf("abort outcome: got %v, want aborted (err %v)", res.Outcome, res.Err)
	}
	if res.Err != nil {
		t.Fatalf("abort err: %v", res.Err)
	}
	if outcome, err := cl.VerifyTransactionEnd(context.Background(), res); err != nil || outcome != kgo.TxnEndAborted {
		t.Fatalf("verify aborted: got %v, %v", outcome, err)
	}
}

// The fake broker applies the commit and then drops the response. The
// client cannot know the commit landed, so the outcome must be unknown --
// but the explicit verification, asking the coordinator, proves it
// committed without re-sending any record.
func TestTxnEndResultDroppedCommitResponse(t *testing.T) {
	t.Parallel()
	c := newCluster(t, NumBrokers(1), SeedTopics(1, "end-result-drop"))

	cl := newPlainClient(t, c, kgo.TransactionalID("end-result-drop"))

	beginAndProduce(t, cl, "end-result-drop")

	dropped := c.Fault(Fault{Keys: []kmsg.Key{kmsg.EndTxn}, Drop: true})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	res := cl.EndTransactionResult(ctx, kgo.TryCommit)
	cancel()

	if dropped.Hits() != 1 {
		t.Fatalf("expected the EndTxn to be dropped once, got %d", dropped.Hits())
	}
	if res.Outcome != kgo.TxnEndUnknown {
		t.Fatalf("BUG: dropped commit response judged %v, must be unknown", res.Outcome)
	}
	if res.Err == nil {
		t.Fatal("dropped commit response: expected an error")
	}
	if !res.RequestSent {
		t.Fatal("evidence: the EndTxn request was sent")
	}
	if res.ResponseReceived {
		t.Fatal("evidence: no response arrived for the dropped request")
	}

	// The broker applied the commit before dropping the response;
	// verification must prove it.
	outcome, err := cl.VerifyTransactionEnd(context.Background(), res)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if outcome != kgo.TxnEndCommitted {
		t.Fatalf("verify after dropped commit: got %v, want committed", outcome)
	}

	// The documented TryAbort retry heals via producer id reload. The
	// commit may already have been applied (here: it was), so even a
	// nil error must keep the outcome unknown rather than claim an
	// abort.
	res2 := cl.EndTransactionResult(context.Background(), kgo.TryAbort)
	if res2.Err != nil {
		t.Fatalf("healing retry: %v", res2.Err)
	}
	if res2.Outcome != kgo.TxnEndUnknown {
		t.Fatalf("healed commit retry judged %v, must stay unknown", res2.Outcome)
	}
	if !res2.HealedByReload {
		t.Fatal("evidence: the retry healed via producer id reload")
	}

	// Once the client has moved on, the first attempt can no longer be
	// verified: verification refuses rather than guess.
	if outcome, err := cl.VerifyTransactionEnd(context.Background(), res); err == nil || outcome != kgo.TxnEndUnknown {
		t.Fatalf("verify after moving on: got %v, %v; want unknown with an error", outcome, err)
	}
}

// UNKNOWN_SERVER_ERROR means the broker's answer does not say whether the
// end was applied. When the coordinator cannot confirm the outcome either
// (the transaction is still ongoing), verification must stay unknown.
func TestTxnEndResultUnverifiableStaysUnknown(t *testing.T) {
	t.Parallel()
	c := newCluster(t, NumBrokers(1), SeedTopics(1, "end-result-use"))

	cl := newPlainClient(t, c, kgo.TransactionalID("end-result-use"))

	beginAndProduce(t, cl, "end-result-use")

	// The fault rejects the EndTxn before it is applied, with an error
	// that by contract says nothing about whether it was applied.
	injected := c.Fault(Fault{Keys: []kmsg.Key{kmsg.EndTxn}, Err: kerr.UnknownServerError})

	res := cl.EndTransactionResult(context.Background(), kgo.TryCommit)
	if injected.Hits() != 1 {
		t.Fatalf("expected one injected UNKNOWN_SERVER_ERROR, got %d", injected.Hits())
	}
	if res.Outcome != kgo.TxnEndUnknown {
		t.Fatalf("BUG: UNKNOWN_SERVER_ERROR judged %v, must be unknown", res.Outcome)
	}
	if !res.RequestSent || !res.ResponseReceived {
		t.Fatalf("evidence: sent=%v received=%v, want both true", res.RequestSent, res.ResponseReceived)
	}

	// The transaction is still ongoing broker-side: the coordinator
	// cannot confirm anything, and the outcome stays unknown.
	outcome, err := cl.VerifyTransactionEnd(context.Background(), res)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if outcome != kgo.TxnEndUnknown {
		t.Fatalf("BUG: unverifiable outcome judged %v, must stay unknown", outcome)
	}

	// The healing retry aborts whatever is still ongoing, but the prior
	// commit's outcome is unknowable and stays unknown.
	res2 := cl.EndTransactionResult(context.Background(), kgo.TryAbort)
	if res2.Err != nil {
		t.Fatalf("healing retry: %v", res2.Err)
	}
	if res2.Outcome != kgo.TxnEndUnknown {
		t.Fatalf("healed commit retry judged %v, must stay unknown", res2.Outcome)
	}
}

// A dropped abort response is unknown at first, but the healing retry is
// definitely aborted: either the first abort landed, or the producer id
// reload fence-aborted whatever was still ongoing.
func TestTxnEndResultDroppedAbortHealsAborted(t *testing.T) {
	t.Parallel()
	c := newCluster(t, NumBrokers(1), SeedTopics(1, "end-result-drop-abort"))

	cl := newPlainClient(t, c, kgo.TransactionalID("end-result-drop-abort"))

	beginAndProduce(t, cl, "end-result-drop-abort")

	c.Fault(Fault{Keys: []kmsg.Key{kmsg.EndTxn}, Drop: true})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	res := cl.EndTransactionResult(ctx, kgo.TryAbort)
	cancel()
	if res.Outcome != kgo.TxnEndUnknown {
		t.Fatalf("BUG: dropped abort response judged %v, must be unknown", res.Outcome)
	}

	res2 := cl.EndTransactionResult(context.Background(), kgo.TryAbort)
	if res2.Err != nil {
		t.Fatalf("healing retry: %v", res2.Err)
	}
	if res2.Outcome != kgo.TxnEndAborted {
		t.Fatalf("healed abort retry judged %v, want aborted", res2.Outcome)
	}
}

// A definitive broker rejection of the end (here: the transactional ID is
// not authorized) means the end was not applied and the transaction can
// never be committed: the outcome is aborted, not unknown.
func TestTxnEndResultDefinitiveErrorIsAborted(t *testing.T) {
	t.Parallel()
	c := newCluster(t, NumBrokers(1), SeedTopics(1, "end-result-denied"))

	cl := newPlainClient(t, c, kgo.TransactionalID("end-result-denied"))

	beginAndProduce(t, cl, "end-result-denied")

	injected := c.Fault(Fault{Keys: []kmsg.Key{kmsg.EndTxn}, Err: kerr.TransactionalIDAuthorizationFailed})

	res := cl.EndTransactionResult(context.Background(), kgo.TryCommit)
	if injected.Hits() != 1 {
		t.Fatalf("expected one injected error, got %d", injected.Hits())
	}
	if res.Outcome != kgo.TxnEndAborted {
		t.Fatalf("definitive rejection judged %v, want aborted", res.Outcome)
	}
	if res.Err == nil {
		t.Fatal("definitive rejection: expected an error")
	}
	if !res.ResponseReceived {
		t.Fatal("evidence: a definitive broker error was received")
	}
}
