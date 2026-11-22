# Keep-alive fairness experiments

Background: see Puma::Server#process_client (lib/puma/server.rb). After each
keep-alive request, if the next request is already buffered on the socket
(back-to-back / pipelined):

    if @thread_pool.waiting > 0
      can_loop = true              # same thread keeps the connection
    else
      @thread_pool << client       # hand the connection back to the pool queue
    end

There is no numeric "max consecutive requests" knob in this snapshot:
`max_keep_alive` (formerly `max_fast_inline`) only caps total requests per
connection before it is closed (lib/puma/response.rb:157). The fairness
mechanism is purely "spare thread available?". When no thread is waiting, the
Client object re-enters the ThreadPool @todo FIFO queue, behind any other
ready connections; pipelined responses still come back in request order.

## Setup (native extension)

    ridk exec ruby ext/puma_http11/extconf.rb   # run from inside ext/puma_http11
    ridk exec make -C ext/puma_http11
    mkdir -p ext/puma_http11/puma
    cp ext/puma_http11/puma_http11.so ext/puma_http11/puma/

## Scripts

    # 1. End-to-end: client A pipelines 6 requests, client B sends 1
    ruby -Ilib -Iext/puma_http11 experiments/fairness.rb 1   # strict thread
    ruby -Ilib -Iext/puma_http11 experiments/fairness.rb 2   # spare thread

    # 2. Trace every ThreadPool#<< enqueue (initial accept vs requeue)
    ruby -Ilib -Iext/puma_http11 experiments/fairness_traced.rb

    # 3. Trace each keep-alive decision: INLINE loop / REQUEUE / REACTOR
    ruby -Ilib -Iext/puma_http11 experiments/fairness_decision.rb 1
    ruby -Ilib -Iext/puma_http11 experiments/fairness_decision.rb 2

## Expected observation

- max_threads=1: after A's first response, B is served before A's remaining
  responses; every A transition is REQUEUE; A responses stay in order.
- max_threads=2: first A requeue happens while both threads are busy (B is
  queued); once B drains, later A requests run INLINE on one thread, and the
  connection parks in the Reactor when the pipelined buffer is exhausted.
