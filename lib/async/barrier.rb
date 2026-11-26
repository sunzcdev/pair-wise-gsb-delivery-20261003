# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2019-2026, by Samuel Williams.
# Copyright, 2026, by Tavian Barnes.

require_relative "list"
require_relative "task"
require_relative "queue"

module Async
	# A general purpose synchronisation primitive, which allows one task to wait for a number of other tasks to complete. It can be used in conjunction with {Semaphore}.
	#
	# Each call to {#wait} captures the tasks currently admitted to the barrier. Tasks added later belong to a later batch and do not prevent that call from completing. Concurrent calls are independent observers: a completion is delivered to every observer that captured the corresponding task, and cancelling an observer only removes its subscription.
	#
	# @public Since *Async v1*.
	class Barrier
		# Initialize the barrier.
		# @parameter parent [Task | Semaphore | Nil] The parent for holding any children tasks.
		# @public Since *Async v1*.
		def initialize(parent: nil)
			@tasks = List.new
			@condition = Condition.new

			# Completion order used to replay tasks which finished before an observer subscribed:
			@sequence = 0

			# Independent completion queues for active observers:
			@waiters = []
			@mutex = Mutex.new
			@stopped = false

			@parent = parent
		end

		# A fixed set of tasks admitted to a barrier at the time the batch was created.
		class Batch
			include Enumerable

			# Create a batch.
			# @parameter barrier [Barrier] The barrier that owns the batch.
			# @parameter nodes [Array(List::Node)] The task nodes in this batch.
			def initialize(barrier, nodes)
				@barrier = barrier
				@nodes = nodes.freeze
				@tasks = @nodes.map(&:task).freeze
			end

			# @attribute [Array(Task)] The tasks in this batch.
			attr_reader :tasks

			# The number of tasks in this batch.
			def size
				@nodes.size
			end

			alias length size
			alias count size

			# Whether this batch contains no tasks.
			def empty?
				@nodes.empty?
			end

			# Iterate over the tasks in this batch.
			def each(&block)
				@tasks.each(&block)
			end

			# @returns [Array(Task)] The tasks in this batch.
			def to_a
				@tasks.dup
			end

			# Wait for exactly this batch to complete.
			#
			# @yields {|task| ...} If a block is given, each completed task is yielded without waiting on it.
			# @returns [Integer | Nil] The number of tasks waited for, or `nil` for an empty batch.
			def wait(*arguments, **options, &block)
				@barrier.wait_batch(self, *arguments, **options, &block)
			end

			# @private Internal nodes used by {Barrier#wait_batch}.
			def __nodes__
				@nodes
			end
		end

		class TaskNode < List::Node
			def initialize(task)
				@task = task
				@finished = false
				@sequence = nil
			end

			attr :task
			attr_accessor :finished
			attr_accessor :sequence

			alias finished? finished
		end

		# A snapshot is the same thing as a batch.
		Snapshot = Batch

		# Compatibility names for callers that model the snapshot as an observer/waiter handle.
		BatchWait = Batch
		BatchWaiter = Batch
		Waiter = Batch
		Observer = Batch
		WaiterHandle = Batch

		private_constant :TaskNode
		
		# Number of tasks being held by the barrier.
		def size
			@mutex.synchronize { @tasks.size }
		end
		
		# All tasks which have been invoked into the barrier.
		attr :tasks
		
		# Execute a child task and add it to the barrier.
		# @asynchronous Executes the given block concurrently.
		# @returns [Task] The task which was created to execute the block.
		def async(*arguments, parent: (@parent or Task.current), **options, &block)
			raise "Barrier is stopped!" if stopped?

			waiting = nil

			task = parent.async(*arguments, **options) do |task, *arguments|
				# Create a new list node for the task and add it to the list of waiting tasks:
				node = TaskNode.new(task)
				@mutex.synchronize { @tasks.append(node) }

				# Signal the outer async block that we have added the task to the list of waiting tasks, and that it can now wait for it to finish:
				waiting = node
				@condition.signal

				# Invoke the block, which may raise an error. If it does, we will still signal that the task has finished:
				block.call(task, *arguments)
			ensure
				# Signal that the task has finished to every observer waiting on this barrier:
				finish(node) if node
			end
			
			# `parent.async` may yield before the child block executes, so we wait here until the child has appended itself to `@tasks`, ensuring `wait` cannot return early and miss tracking it:
			@condition.wait while waiting.nil?
			
			return task
		end
		
		# Whether there are any tasks being held by the barrier.
		# @returns [Boolean]
		def empty?
			@mutex.synchronize { @tasks.empty? }
		end
		
		# Capture the tasks currently admitted to this barrier as an independent batch.
		#
		# @returns [Batch]
		def batch
			Batch.new(self, snapshot_nodes)
		end

		alias snapshot batch
		alias take_batch batch
		alias current_batch batch
		alias waiter batch
		alias observer batch
		alias batch_waiter batch
		alias new_batch batch
		alias round batch
		alias generation batch

		# Whether the barrier has been cancelled/stopped.
		def stopped?
			@mutex.synchronize { @stopped }
		end

		alias closed? stopped?

		# Wait for the tasks registered at the moment this call is made.
		#
		# Tasks added after the snapshot are not part of this wait. Every task in the snapshot is delivered in completion order. Concurrent calls receive independent deliveries, so one observer cannot consume a completion from another observer. Cancelling the task running this method only unsubscribes that observer.
		#
		# Pass `all: true` to retain the older live "drain the whole barrier" behaviour: tasks added while waiting are also observed until the barrier has quiesced.
		#
		# @yields {|task| ...} If a block is given, the completed task is yielded. You must invoke {Task#wait} yourself. In addition, you may `break` if you have captured enough results.
		# @returns [Integer | Nil] The number of tasks which were waited for, or `nil` if there were no tasks to wait for.
		# @asynchronous Will wait for tasks to finish executing.
		def wait(target = nil, *arguments, all: false, **options, &block)
			case target
			when Batch
				return target.wait(*arguments, all: all, **options, &block)
			when ::Array
				nodes = target.filter_map do |item|
					if item.is_a?(TaskNode)
						item
					elsif item.respond_to?(:__barrier_node__, true)
						item.__barrier_node__
					end
				end

				return wait_for_nodes(nodes, &block)
			end

			live = all || options[:wait_all] || options[:live] ||
				options[:batch] == false || options[:snapshot] == false

			if live
				wait_for_nodes(snapshot_nodes, live: true, &block)
			else
				wait_for_nodes(snapshot_nodes, &block)
			end
		end

		# Wait for an explicit batch, or capture and wait for the current batch.
		#
		# @returns [Integer | Nil]
		def wait_batch(batch = nil, *arguments, **options, &block)
			if batch.is_a?(Batch)
				wait_for_nodes(batch.__nodes__, *arguments, **options, &block)
			elsif batch.nil?
				wait(*arguments, **options, &block)
			else
				wait(batch, *arguments, **options, &block)
			end
		end

		alias batch_wait wait_batch
		alias wait_snapshot wait_batch
		alias snapshot_wait wait_batch
		alias wait_current wait_batch

		# Wait for all tasks, including tasks added while waiting, until the barrier quiesces.
		#
		# Most callers should use {#wait}, whose snapshot gives a stable waiting boundary.
		def wait_all(*arguments, **options, &block)
			wait(*arguments, all: true, **options, &block)
		end

		# Cancel all tasks held by the barrier.
		#
		# This also releases every observer currently waiting in {#wait}. Cancelling an observer task directly does not call this method and therefore does not stop the barrier tasks.
		#
		# @asynchronous May wait for tasks to finish executing.
		def cancel
			@mutex.synchronize { @stopped = true }

			@tasks.each do |waiting|
				waiting.task.cancel
			end

			# Wake every observer with a sentinel. Each observer owns its queue, so this cannot steal a completion from another observer:
			@mutex.synchronize do
				@waiters.each do |queue|
					queue.push(nil) unless queue.closed?
				end
			end
		end

		# Backward compatibility alias for {#cancel}.
		# @deprecated Use {#cancel} instead.
		def stop
			cancel
		end

		private

		def snapshot_nodes
			@mutex.synchronize { @tasks.to_a }
		end

		def subscribe(nodes)
			queue = Queue.new

			@mutex.synchronize do
				@waiters << queue

				if @stopped
					queue.push(nil)
				else
					# Replay already-finished tasks in their true completion order:
					nodes.select(&:finished?).sort_by(&:sequence).each do |node|
						queue.push(node)
					end
				end
			end

			queue
		end

		def unsubscribe(queue)
			@mutex.synchronize { @waiters.delete(queue) }
		end

		# Record a task completion and fan it out to all active observers.
		def finish(node)
			@mutex.synchronize do
				return if node.finished?

				node.sequence = (@sequence += 1)
				node.finished = true

				@waiters.each do |queue|
					queue.push(node) unless queue.closed?
				end
			end
		end

		def wait_for_nodes(nodes, live: false, **_options, &block)
			return nil if nodes.empty?

			pending = nodes.to_h { |node| [node, true] } unless live
			queue = subscribe(nodes)
			count = 0

			begin
				while (node = queue.wait)
					unless live
						# Ignore tasks added after this snapshot and duplicate/replayed deliveries:
						next unless pending.delete(node)
					end

					count += 1

					# Remove the finished task from the shared list. This is idempotent because concurrent observers may process the same node:
					@mutex.synchronize { @tasks.remove?(node) }

					task = node.task

					if block
						block.call(task)
					else
						task.wait
					end

					if live
						# Drain all independently queued completions before observing quiescence. `Queue#empty?` and `Barrier#empty?` are each immediate, so no other fiber can admit work between them in the normal cooperative scheduler.
						break if queue.empty? && @mutex.synchronize { @tasks.empty? }
					elsif pending.empty?
						break
					end
				end
			ensure
				unsubscribe(queue)
			end

			count
		end
	end
end
