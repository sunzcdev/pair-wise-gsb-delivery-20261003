# frozen_string_literal: true

# This spec tests the optional FIFO deduplication observation of
# Shoryuken::Worker::InlineExecutor.
#
# Production sends go to SQS, where a FIFO queue drops a repeated
# message_deduplication_id (scoped by queue and message group) inside its
# deduplication window. Inline execution used to run every submission, so CI
# accepted re-executions production would suppress. Enabling the observation
# makes inline mirror SQS: the worker runs once per observed scope and repeats
# return a SuppressedMessage instead of a perform result.
#
# Scenario: the same dedup key is sent twice, then to another isolated queue;
# after the bounded window elapses the key is sent again. The execution count
# and the returned objects show which inputs actually executed.

sqs_client = Aws::SQS::Client.new(stub_responses: true)
Shoryuken.sqs_client = sqs_client
Shoryuken.worker_executor = Shoryuken::Worker::InlineExecutor

class InlineFifoDedupWorker
  include Shoryuken::Worker

  shoryuken_options queue: 'orders.fifo'

  def perform(sqs_msg, body)
    DT[:perform] << [sqs_msg.queue_name, body]
    "done-#{body}"
  end
end

# A controllable monotonic clock so the window can elapse without sleeping.
time = [1_000.0]
Shoryuken::Worker::InlineExecutor.enable_fifo_deduplication!(window: 300, clock: -> { time.first })

fifo_options = { message_group_id: 'order-group', message_deduplication_id: 'dedup-key' }

# 1) Same dedup key twice: the first executes, the second is suppressed like SQS.
first = InlineFifoDedupWorker.perform_async('order-1', fifo_options)
repeat = InlineFifoDedupWorker.perform_async('order-1', fifo_options)

assert_equal('done-order-1', first)
assert(repeat.is_a?(Shoryuken::Worker::InlineExecutor::SuppressedMessage))
assert(repeat.deduplicated?)
assert_equal([['orders.fifo', 'order-1']], DT[:perform])
assert_equal(1, Shoryuken::Worker::InlineExecutor.observed_deduplication_count)

# 2) The same key submitted to another isolated queue executes independently.
#    Move the clock forward but stay inside the window so the first scope lives.
time[0] += 50
isolated = InlineFifoDedupWorker.perform_async('order-1', fifo_options.merge(queue: 'other-orders.fifo'))

assert_equal('done-order-1', isolated)
assert_equal(
  [
    ['orders.fifo', 'order-1'],
    ['other-orders.fifo', 'order-1']
  ],
  DT[:perform]
)

# 3) Advancing past the window re-accepts the original scope. The original was
#    observed at t=1000 and the isolated one at t=1050, so at t=1301 only the
#    original has expired; the isolated scope stays observed.
time[0] += 251
re_executed = InlineFifoDedupWorker.perform_async('order-1', fifo_options)

assert_equal('done-order-1', re_executed)
assert_equal(
  [
    ['orders.fifo', 'order-1'],
    ['other-orders.fifo', 'order-1'],
    ['orders.fifo', 'order-1']
  ],
  DT[:perform]
)
# The expired scope was re-recorded and the isolated scope is still live.
assert_equal(2, Shoryuken::Worker::InlineExecutor.observed_deduplication_count)

# Observed scopes name exactly the inputs that were actually executed.
assert_includes(
  Shoryuken::Worker::InlineExecutor.observed_deduplication_keys,
  ['orders.fifo', 'order-group', 'dedup-key']
)
assert_includes(
  Shoryuken::Worker::InlineExecutor.observed_deduplication_keys,
  ['other-orders.fifo', 'order-group', 'dedup-key']
)

# perform_in shares the observation, and no SQS request is ever made inline.
delayed = InlineFifoDedupWorker.perform_in(60, 'order-1', fifo_options)
assert(delayed.is_a?(Shoryuken::Worker::InlineExecutor::SuppressedMessage))
assert_equal([], sqs_client.api_requests)

Shoryuken::Worker::InlineExecutor.disable_fifo_deduplication!
