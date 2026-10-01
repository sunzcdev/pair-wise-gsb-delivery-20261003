require 'concurrent/utility/engine'
require 'concurrent/executor/ruby_work_stealing_executor'

module Concurrent

  if Concurrent.on_jruby?
    # The work-stealing executor is currently a pure-Ruby implementation on
    # every runtime; it performs identically under JRuby.
  end

  WorkStealingExecutorImplementation = RubyWorkStealingExecutor
  private_constant :WorkStealingExecutorImplementation

  # A fixed-size executor with one private task deque per worker and idle-time
  # work stealing.
  #
  # Unlike {ThreadPoolExecutor}, which funnels every submitted task through one
  # shared queue, this executor routes work according to where it is submitted:
  #
  # * tasks {#post}ed from a worker thread (for example background jobs that
  #   fan out into many smaller jobs) are pushed to that worker's own
  #   double-ended queue, so a deeply nested fan-out never serialises on a
  #   shared work queue;
  # * tasks {#post}ed from outside the pool enter through the independent
  #   external submission entrance and are deposited (round-robin) onto worker
  #   deques while holding only the lightweight parking lock - never a deque
  #   lock - so external producers do not contend with workers or thieves;
  # * a worker with no local work steals not-yet-started tasks from other
  #   workers' deques.
  #
  # == Ends and claim rules
  #
  # The owner pops its own deque LIFO (the newest forked child resumes first,
  # keeping the in-flight working set small), while thieves pop the steal end
  # FIFO (the oldest, largest available unit). Every removal is performed
  # under the target deque's own lock, so when the last item is contended by
  # the owner and several thieves at once it is nevertheless handed to exactly
  # one of them - every accepted task executes at most and at least once.
  #
  # == Shutdown and idle behaviour
  #
  # Idle threads park on a condition variable (never busy-wait) and are woken
  # per accepted task; the emptiness probe and the park are one atomic step.
  # {#shutdown} stops accepting external work and drains every accepted task,
  # including children forked by tasks that are still running during the
  # drain; {#kill} dismisses queued work and lets in-flight tasks finish.
  #
  # The submission protocol is identical to every other executor service:
  # {#post} returns +true+ while accepting work, {#<<} returns the pool, and
  # after shutdown the configured +fallback_policy+ applies
  # (+:abort+/+:discard+/+:caller_runs+).
  #
  # @example Fan-out from inside a running task
  #   pool = Concurrent::WorkStealingExecutor.new(max_threads: 4)
  #   pool.post do
  #     100.times do |i|
  #       # enters the current worker's own deque, never a shared queue
  #       pool.post(i) { |n| work(n) }
  #     end
  #   end
  #   pool.shutdown
  #   pool.wait_for_termination
  #
  # @!macro abstract_executor_service_public_api
  class WorkStealingExecutor < WorkStealingExecutorImplementation
  end
end
