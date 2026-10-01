require 'concurrent/executor/ruby_work_stealing_executor'

module Concurrent

  # @!macro work_stealing_executor
  #
  #   A thread pool with a fixed number of threads where each worker owns a
  #   private deque of tasks. Tasks {#post}ed from within a running task enter
  #   the current worker's local deque (taken LIFO by the owner, stolen FIFO by
  #   idle workers), while externally posted tasks enter a separate shared
  #   inbox that is consulted only after all local deques are empty. It supports
  #   the same submission and lifecycle protocol ({#post}, {#shutdown},
  #   {#kill}, {#wait_for_termination}, +fallback_policy+) as the other
  #   executor services.
  #
  #   It is aimed at workloads whose tasks recursively split into many smaller
  #   tasks; with a single shared queue that queue becomes the contention point.
  #
  # @example recursively splitting work
  #   pool = Concurrent::WorkStealingExecutor.new(4)
  #   def work(pool, items)
  #     if items.size <= 1
  #       process(items)
  #     else
  #       left, right = items.each_slice((items.size / 2.0).ceil).to_a
  #       pool.post { work(pool, left) }
  #       pool.post { work(pool, right) }
  #     end
  #   end
  #   pool.post { work(pool, items) }
  #   pool.shutdown
  #   pool.wait_for_termination
  class WorkStealingExecutor < RubyWorkStealingExecutor
  end
end
