# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025, by Shopify Inc.
# Copyright, 2025, by Samuel Williams.

require "io/event/priority_heap"
require "thread"

require_relative "queue"

module Async
	# A queue which allows items to be processed in priority order of consumers.
	#
	# Unlike a traditional priority queue where items have priorities, this queue 
	# assigns priorities to consumers (fibers waiting to dequeue). Higher priority
	# consumers are served first when items become available.
	#
	# @public Since *Async v2*.
	class PriorityQueue
		ClosedError = Queue::ClosedError
		
		# A waiter represents a fiber waiting to dequeue with a given priority.
		Waiter = Struct.new(:fiber, :priority, :sequence, :condition, :value) do
			include Comparable

			def <=>(other)
				# Higher priority comes first, then FIFO for equal priorities:
				if priority == other.priority
					# Use sequence for FIFO behavior (lower sequence = earlier):
					sequence <=> other.sequence
				else
					other.priority <=> priority  # Reverse for max-heap behavior
				end
			end

			# Promise the given value to this waiter and schedule its fiber to resume.
			#
			# The fiber does not resume immediately, so between this call and the fiber
			# actually running, the waiter can still be cancelled. The queue tracks the
			# in-flight delivery separately so the value can be reclaimed if that happens.
			def signal(value)
				self.value = value
				condition.signal
			end

			def wait_for_value(mutex, timeout = nil)
				condition.wait(mutex, timeout)
				return self.value
			end

			# Mark the promised value as actually claimed by the waiting fiber.
			# Once taken, a later cancellation must not reclaim the value.
			def take!
				@taken = true
			end

			# @returns [Boolean] Whether the waiting fiber has actually claimed the promised value.
			def taken?
				@taken == true
			end

			# Invalidate this waiter, making it unusable and detectable as abandoned.
			def invalidate!
				self.fiber = nil
			end

			# Check if this waiter has been invalidated.
			def valid?
				self.fiber&.alive?
			end
		end
		
		private_constant :Waiter
		
		# Create a new priority queue.
		#
		# @parameter parent [Interface(:async) | Nil] The parent task to use for async operations.
		def initialize(parent: nil)
			@items = []
			@closed = false
			@parent = parent
			# Waiters that have not yet been promised an item, ordered by priority/FIFO:
			@waiting = IO::Event::PriorityHeap.new
			# Waiters which have been promised an item (signaled) but whose fibers have
			# not resumed and claimed it yet. This covers the cancellation window between
			# selection by a producer and the waiter actually continuing:
			@pending = {}
			@sequence = 0

			@mutex = Mutex.new
		end

		# Close the queue, causing all waiting tasks to return `nil`.
		# Any subsequent calls to {enqueue} will raise an exception.
		def close
			@mutex.synchronize do
				@closed = true

				# Release every waiter that is still blocked, skipping waiters that have
				# already been stopped. Waiters in the middle of a delivery (@pending) keep
				# their promised item; if they are cancelled before claiming it, the item is
				# redelivered to the next waiter (or left in the queue) via {#redeliver}.
				while waiter = @waiting.pop
					next unless waiter.valid?

					waiter.signal(nil)
				end
			end
		end
		
		# @returns [Boolean] Whether the queue is closed.
		def closed?
			@closed
		end
		
		# @attribute [Array] The items in the queue.
		attr :items
		
		# @returns [Integer] The number of items in the queue.
		def size
			@items.size
		end
		
		# @returns [Boolean] Whether the queue is empty.
		def empty?
			@items.empty?
		end
		
		# @returns [Integer] The number of fibers waiting to dequeue.
		def waiting_count
			@mutex.synchronize do
				# @pending waiters are still blocked until their resumed fiber runs, so
				# they count as waiting too:
				@waiting.size + @pending.size
			end
		end
		
		# @deprecated Use {#waiting_count} instead.
		alias waiting waiting_count
		
		# Add an item to the queue.
		#
		# @parameter item [Object] The item to add to the queue.
		def push(item)
			@mutex.synchronize do
				if @closed
					raise ClosedError, "Cannot push items to a closed queue."
				end

				@items << item

				dispatch_next_item
			end
		end

		# Compatibility with {::Queue#push}.
		def <<(item)
			self.push(item)
		end

		# Add multiple items to the queue.
		#
		# @parameter items [Array] The items to add to the queue.
		def enqueue(*items)
			@mutex.synchronize do
				if @closed
					raise ClosedError, "Cannot enqueue items to a closed queue."
				end

				@items.concat(items)

				dispatch_available_items
			end
		end
		
		# Remove and return the next item from the queue.
		#
		# If the queue is empty, this method will block until an item is available or timeout expires.
		# Fibers are served in priority order, with higher priority fibers receiving
		# items first.
		#
		# @parameter priority [Numeric] The priority of this consumer (higher = served first).
		# @parameter timeout [Numeric, nil] Maximum time to wait for an item. If nil, waits indefinitely. If 0, returns immediately.
		# @returns [Object, nil] The next item in the queue, or nil if timeout expires.
		def dequeue(priority: 0, timeout: nil)
			@mutex.synchronize do
				# If queue is closed and empty, return nil immediately:
				if @closed && @items.empty?
					return nil
				end

				# Fast path: if items available and either no waiters or we have higher priority.
				# A waiter that timed out or was stopped before being served stays in the heap
				# until reaped; drop any such dead head first so it cannot block this dequeue:
				unless @items.empty?
					while (head = @waiting.peek) && !head.valid?
						@waiting.pop
					end

					head = @waiting.peek
					if head.nil? or priority > head.priority
						return @items.shift
					end
				end

				# Handle immediate timeout (non-blocking)
				return nil if timeout == 0

				# Need to wait - create our own condition variable and add to waiting queue:
				sequence = @sequence
				@sequence += 1

				condition = ConditionVariable.new
				waiter = Waiter.new(Fiber.current, priority, sequence, condition, nil)
				@waiting.push(waiter)

				begin
					value = waiter.wait_for_value(@mutex, timeout)

					# We resumed. A promise recorded in @pending means a real item was
					# delivered to us (as opposed to waking from a timeout or from close,
					# which are never tracked in @pending). Claim it and remove the record.
					if @pending.delete(waiter)
						waiter.take!
						return value
					end

					# Woken without a promised item (timeout or queue closed):
					return nil
				ensure
					# Runs while holding @mutex (ConditionVariable#wait reacquires it). If we
					# were cancelled between being promised an item and claiming it, the
					# promise is still in @pending: hand the item to the next eligible waiter,
					# otherwise put it back. A value we already took is never reclaimed.
					redeliver(waiter) unless waiter.taken?
					waiter.invalidate!
				end
			end
		end
		
		# Compatibility with {::Queue#pop}.
		#
		# @parameter priority [Numeric] The priority of this consumer.
		# @parameter timeout [Numeric, nil] Maximum time to wait for an item. If nil, waits indefinitely. If 0, returns immediately.
		# @returns [Object, nil] The dequeued item, or nil if timeout expires.
		def pop(priority: 0, timeout: nil)
			self.dequeue(priority: priority, timeout: timeout)
		end
		
		# Process each item in the queue.
		#
		# @asynchronous Executes the given block concurrently for each item.
		#
		# @parameter priority [Numeric] The priority for processing items.
		# @parameter parent [Interface(:async) | Nil] The parent task to use for async operations.
		# @parameter options [Hash] The options to pass to the task.
		# @yields {|task| ...} When the system is idle, the block will be executed in a new task.
		def async(priority: 0, parent: (@parent or Task.current), **options, &block)
			while item = self.dequeue(priority: priority)
				parent.async(item, **options, &block)
			end
		end
		
		# Enumerate each item in the queue.
		#
		# @parameter priority [Numeric] The priority for dequeuing items.
		def each(priority: 0)
			while item = self.dequeue(priority: priority)
				yield item
			end
		end
		
		# Signal the queue with a value, the same as {#enqueue}.
		def signal(value = nil)
			self.enqueue(value)
		end
		
		# Wait for an item to be available, the same as {#dequeue}.
		#
		# @parameter priority [Numeric] The priority of this consumer.
		def wait(priority: 0)
			self.dequeue(priority: priority)
		end

		private

		# Pop the highest priority waiter that is still alive.
		#
		# Stopped/invalidated waiters are discarded and never receive an item.
		#
		# @parameter heap [IO::Event::PriorityHeap] The heap to draw from.
		# @returns [Waiter | Nil] The next valid waiter, or nil if none remain.
		def pop_valid_waiter(heap)
			while waiter = heap.pop
				return waiter if waiter.valid?
				# Dead/invalid waiter discarded, try the next one in waiting order.
			end

			nil
		end

		# Promise one queued item to the next eligible waiter, if any.
		#
		# The chosen waiter is removed from the waiting heap and recorded in @pending
		# until its fiber resumes and claims the item. This makes the delivery explicit:
		# if the waiter is cancelled before claiming it, {#redeliver} can find the item
		# and offer it to the next waiter in the current waiting order.
		def dispatch_next_item
			return if @items.empty?

			waiter = pop_valid_waiter(@waiting)
			return unless waiter

			item = @items.shift
			waiter.signal(item)
			@pending[waiter] = true
		end

		# Dispatch as many queued items as there are eligible waiters for.
		# {#dispatch_next_item} pops (and therefore drains) at least one waiter per call,
		# so this terminates once the heap is empty even when dead waiters remain but no
		# eligible waiter is left to take the remaining items.
		def dispatch_available_items
			dispatch_next_item until @items.empty? || @waiting.empty?
		end

		# Reclaim an item promised to a waiter that did not claim it (cancelled or timed
		# out while the delivery was still in flight) and continue the delivery.
		#
		# The item is offered to waiters in the current priority/FIFO waiting order. If
		# no eligible waiter remains (or the queue was closed in the meantime), the item
		# is returned to the front of the queue so it is not lost.
		#
		# Must be called while holding @mutex.
		def redeliver(waiter)
			# Only deliveries recorded in @pending carry an item that must be reclaimed:
			return unless @pending.delete(waiter)

			item = waiter.value

			if !@closed && (next_waiter = pop_valid_waiter(@waiting))
				# Hand the reclaimed item straight to the next waiter in waiting order:
				next_waiter.signal(item)
				@pending[next_waiter] = true
			else
				# No one else can take it right now; keep it available at the front:
				@items.unshift(item)
			end
		end
	end
end
