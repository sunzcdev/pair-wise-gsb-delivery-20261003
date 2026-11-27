# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2017-2026, by Samuel Williams.
# Copyright, 2017, by Kent Gruber.
# Copyright, 2026, by Robert Mosolgo.

require "fiber"
require_relative "list"

module Async
	# A synchronization primitive, which allows fibers to wait until a particular condition is (edge) triggered.
	#
	# Each call to {#signal} starts a *round*: every fiber which is waiting when the call is made is resumed with the signalled value, while a fiber which registers after that point is bound to the next round instead. This keeps the existing edge-triggered behaviour - a notification is never queued up for a fiber which has not asked for it yet.
	#
	# The caller may optionally observe the outcome of a round using a {Receipt}. Observing a receipt never changes the behaviour of the waiters: it only records whether each of them actually received the value, was cancelled before it could receive it, or received the value and immediately registered for the next notification.
	#
	# @public Since *Async v1*.
	class Condition
		# A record of what happened to each of the waiters during a single notification round.
		#
		# A receipt is optional: pass it to {Condition#signal} to observe how the waiters registered for that round finished. Waiting for a receipt to settle is independent of the waiters themselves, so cancelling the task which is observing the receipt does not disturb any of them.
		class Receipt
			# The waiter has not reached a terminal state yet.
			WAITING = :waiting

			# The waiter received the signalled value.
			SIGNALLED = :signalled

			# The waiter was cancelled before it received the signalled value.
			CANCELLED = :cancelled

			# The waiter received the signalled value and registered for the next notification.
			RE_REGISTERED = :re_registered

			# The terminal state of a single waiter within a notification round.
			class Entry
				def initialize(receipt, index, fiber)
					@receipt = receipt
					@index = index
					@fiber = fiber
					@state = WAITING
					@value = nil
					@reason = nil
				end

				# @attribute [Integer] The position of the waiter within the round, in registration order.
				attr :index

				# @attribute [Receipt] The receipt this entry belongs to.
				attr :receipt

				# @attribute [Fiber] The fiber which was waiting.
				attr :fiber

				# @attribute [Object | Nil] The value which was delivered to the waiter, if any.
				attr :value

				# @attribute [Exception | Nil] The reason the waiter was cancelled, if any.
				attr :reason
				alias cause reason

				# @returns [Symbol] One of {WAITING}, {SIGNALLED}, {CANCELLED} or {RE_REGISTERED}.
				def state
					@state
				end
				alias status state

				# @returns [Boolean] Whether the waiter reached a terminal state.
				def settled?
					@state != WAITING
				end
				alias terminal? settled?
				alias complete? settled?

				# @returns [Boolean] Whether the waiter is still waiting.
				def waiting?
					@state == WAITING
				end
				alias pending? waiting?

				# @returns [Boolean] Whether the waiter received the signalled value.
				def signalled?
					@state == SIGNALLED or @state == RE_REGISTERED
				end
				alias delivered? signalled?
				alias received? signalled?
				alias successful? signalled?

				# @returns [Boolean] Whether the waiter was cancelled before it received the value.
				def cancelled?
					@state == CANCELLED
				end

				# @returns [Boolean] Whether the waiter received the value and registered for the next notification.
				def re_registered?
					@state == RE_REGISTERED
				end
				alias reregistered? re_registered?

				# @returns [Hash] A machine-readable representation of the entry.
				def to_h
					{
						index: @index,
						state: @state,
						signalled: signalled?,
						cancelled: cancelled?,
						re_registered: re_registered?,
						value: @value,
						reason: @reason,
					}
				end
				alias as_json to_h

				# Access entry fields by symbol or string.
				def [](key)
					to_h[key.to_sym]
				end

				# @private
				def enter(state, value = nil, reason = nil)
					@state = state
					@value = value
					@reason = reason

					return self
				end
			end

			# Create a new, unbound receipt.
			def initialize
				@mutex = Mutex.new
				@entries = []
				@pending = 0
				@bound = false

				@settled = ::Thread::Queue.new
			end

			# @returns [Boolean] Whether this receipt has been bound to a notification round.
			def bound?
				@mutex.synchronize{@bound}
			end

			# @returns [Array(Entry)] The waiters observed by this receipt, in registration order.
			def entries
				@mutex.synchronize{@entries.dup}
			end
			alias waiters entries

			# @returns [Integer] The number of waiters observed by this receipt.
			def size
				@mutex.synchronize{@entries.size}
			end
			alias count size
			alias length size

			# @returns [Boolean] Whether this receipt has not observed any waiter.
			def empty?
				size.zero?
			end

			# @returns [Boolean] Whether every observed waiter reached a terminal state.
			def settled?
				@mutex.synchronize{@bound and @pending.zero?}
			end
			alias complete? settled?
			alias resolved? settled?

			# Wait until every observed waiter reached a terminal state.
			#
			# An unbound receipt is not settled yet: it settles once a {Condition#signal} call binds a round to it, or immediately if that round has no waiters.
			#
			# Any number of tasks may observe the same receipt, and cancelling the observing task does not affect the waiters.
			#
			# @parameter timeout [Numeric | Nil] The maximum time to wait.
			# @returns [Receipt | Nil] Itself once settled, or `nil` if the timeout expired first.
			def wait(timeout: nil)
				unless settled?
					@settled.pop(timeout: timeout)
				end

				return self if settled?
			end
			alias wait_until_settled wait

			# @returns [Array(Entry)] The waiters which received the signalled value.
			def signalled
				entries.select(&:signalled?)
			end
			alias delivered signalled
			alias successful signalled

			# @returns [Array(Entry)] The waiters which were cancelled before receiving the signalled value.
			def cancelled
				entries.select(&:cancelled?)
			end

			# @returns [Array(Entry)] The waiters which received the value and registered for the next notification.
			def re_registered
				entries.select(&:re_registered?)
			end
			alias reregistered re_registered

			# @returns [Array(Entry)] The waiters which have not reached a terminal state yet.
			def waiting
				entries.select(&:waiting?)
			end
			alias pending waiting

			# @returns [Array(Symbol)] The state of every observed waiter, in registration order.
			def states
				entries.map(&:state)
			end
			alias statuses states

			# @returns [Array(Object)] The value delivered to every observed waiter, in registration order.
			def values
				entries.map(&:value)
			end

			# @returns [Hash] A machine-readable representation of the receipt.
			def to_h
				{
					size: size,
					settled: settled?,
					states: states,
					values: values,
					signalled: signalled.size,
					cancelled: cancelled.size,
					re_registered: re_registered.size,
					entries: entries.map(&:to_h),
				}
			end
			alias as_json to_h

			# Bind the entries of a notification round to this receipt.
			#
			# A receipt observes exactly one round: binding it again would mix the waiters of different notifications into one receipt.
			#
			# @parameter entries [Array(Entry)] The entries registered for the round.
			# @raises [RuntimeError] If the receipt is already bound to a round.
			# @private
			def bind(entries)
				@mutex.synchronize do
					if @bound
						raise RuntimeError, "Receipt is already bound to a notification round!"
					end

					@bound = true
					@entries.concat(entries)
					@pending += entries.size

					@settled.close if @pending.zero?
				end

				return self
			end

			# Settle the receipt, indicating that no waiters will be observed after all.
			# @private
			def settle!
				@mutex.synchronize do
					@bound = true
					@pending = 0
					@settled.close unless @settled.closed?
				end

				return self
			end

			# Apply a state transition to an entry.
			#
			# Only a waiting entry can be signalled or cancelled, and only a signalled entry can be re-registered, which makes every transition idempotent.
			#
			# @parameter entry [Entry] The entry to update.
			# @parameter state [Symbol] The new state.
			# @parameter value [Object | Nil] The delivered value, if any.
			# @parameter reason [Exception | Nil] The cancellation reason, if any.
			# @private
			def transition(entry, state, value = nil, reason = nil)
				@mutex.synchronize do
					case state
					when SIGNALLED, CANCELLED
						return self unless entry.waiting?

						@pending -= 1
					when RE_REGISTERED
						return self unless entry.state == SIGNALLED
					end

					entry.enter(state, value, reason)

					@settled.close if @pending.zero? and !@settled.closed?
				end

				return self
			end
		end

		# A single registration of a fiber waiting for a notification.
		#
		# The waiter remembers the round it registered for, so a value can never be consumed by a fiber which registered for a later round.
		# @private
		class Waiter < List::Node
			def initialize(fiber, ready)
				@fiber = fiber
				@ready = ready
				@entry = nil
				@received = false
			end

			# @attribute [Fiber] The fiber which is waiting.
			attr :fiber

			# @attribute [Receipt::Entry | Nil] The receipt entry bound to this waiter, if any.
			attr_accessor :entry

			# Deliver the signalled value to this waiter.
			# @parameter value [Object | Nil] The signalled value.
			def deliver(value)
				@ready.push(value)
			end

			# Wait for the signalled value.
			# @returns [Object | Nil] The signalled value.
			def receive
				@ready.pop
			end

			# Record that the waiter received the signalled value.
			# @parameter value [Object | Nil] The signalled value.
			def signalled(value)
				@received = true

				@entry&.receipt&.transition(@entry, Receipt::SIGNALLED, value)
			end

			# Record that the waiter was cancelled before it received the signalled value.
			# @parameter reason [Exception | Nil] The reason the waiter was cancelled.
			def cancelled(reason)
				return if @received

				@entry&.receipt&.transition(@entry, Receipt::CANCELLED, nil, reason)
			end
		end

		# The set of waiters which were registered when a condition was signalled.
		#
		# The round owns the queue through which the value is delivered. A waiter which is cancelled before it consumes its value does not transfer it to a later round: the queue belongs to this round only, and is discarded once delivery is complete.
		# @private
		class Round
			def initialize(waiters, ready, receipt = nil)
				@waiters = waiters
				@ready = ready

				if receipt
					entries = @waiters.each_with_index.map do |waiter, index|
						waiter.entry = Receipt::Entry.new(receipt, index, waiter.fiber)
					end

					receipt.bind(entries)
				end
			end

			# @returns [Integer] The number of waiters in this round.
			def size
				@waiters.size
			end

			# Deliver the value to every waiter in this round, then close the round so that late waiters cannot consume a value they were not signalled for.
			# @parameter value [Object | Nil] The signalled value.
			def deliver(value)
				@waiters.each do |waiter|
					waiter.deliver(value)
				end

				@ready.close

				return self
			end
		end

		private_constant :Waiter
		private_constant :Round

		# Create a new condition.
		def initialize
			@mutex = Mutex.new
			@waiting = List.new
			@ready = ::Thread::Queue.new

			# Tracks the round entry which was delivered to a fiber but not yet consumed by its next call to {#wait}:
			@delivered = ObjectSpace::WeakMap.new

			@last_receipt = nil
		end

		# @attribute [Receipt | Nil] The most recent receipt created by {#signal}, if any.
		attr :last_receipt

		# Create a new receipt for observing a notification.
		# @returns [Receipt]
		def receipt
			Receipt.new
		end

		# Queue up the current fiber and wait on yielding the task.
		#
		# The value passed to the matching {#signal} is returned. If the waiting fiber is cancelled before that value is delivered, the cancellation propagates and the waiter is recorded as cancelled rather than consuming a later value.
		#
		# A fiber which was signalled by a receipt-observed round and comes back to wait again is recorded as re-registered for that round, which binds it to the next notification.
		#
		# @returns [Object | Nil] The signalled value.
		def wait
			waiter = nil

			@mutex.synchronize do
				# If a value from a receipt-observed round was delivered to this fiber, coming back to wait registers it for the next notification:
				if entry = @delivered.delete(Fiber.current)
					entry.receipt.transition(entry, Receipt::RE_REGISTERED, entry.value)
				end

				waiter = Waiter.new(Fiber.current, @ready)
				@waiting.append(waiter)
			end

			value = waiter.receive
			waiter.signalled(value)

			if entry = waiter.entry
				@mutex.synchronize{@delivered[waiter.fiber] = entry}
			end

			return value
		rescue ::Exception => error
			waiter&.cancelled(error)

			raise
		ensure
			if waiter
				@mutex.synchronize{@waiting.remove?(waiter)}
			end
		end

		# @returns [Boolean] If there are no fibers waiting on this condition.
		def empty?
			@mutex.synchronize{@waiting.empty?}
		end

		# @returns [Boolean] Is any fiber waiting on this notification?
		def waiting?
			!self.empty?
		end

		# @returns [Integer] Number of fibers waiting on this condition.
		def waiting_count
			@mutex.synchronize{@waiting.size}
		end

		# Signal to all currently waiting fibers that they should resume operations.
		#
		# Fibers which register after this call are bound to the next notification. The optional receipt observes the outcome of this round only.
		#
		# @parameter value [Object | Nil] The value to return to the waiting fibers.
		# @parameter receipt [Receipt | Boolean | Nil] An optional receipt, or `true` to allocate one (see {#last_receipt}).
		# @yields {|receipt| ...} The receipt for this round, if a block is given.
		# @returns [Nil]
		def signal(value = nil, receipt: nil, &block)
			receipt = self.prepare_receipt(receipt, block_given?)

			if round = self.exchange(receipt)
				round.deliver(value)
			else
				# There were no waiters, so this round is already settled:
				receipt&.settle!
			end

			yield(receipt) if block_given?

			return nil
		end

		protected

		# Prepare the receipt for a notification round.
		# @parameter receipt [Receipt | Boolean | Nil] The requested receipt.
		# @parameter required [Boolean] Whether a receipt must be allocated.
		# @returns [Receipt | Nil]
		def prepare_receipt(receipt, required = false)
			if receipt == true or (receipt.nil? and required)
				receipt = Receipt.new
			end

			unless receipt.nil? or receipt.is_a?(Receipt)
				raise ArgumentError, "Invalid receipt: #{receipt.inspect}!"
			end

			@last_receipt = receipt if receipt

			return receipt
		end

		# Exchange the current set of waiters and delivery queue for new ones, so that waiters registering during delivery are bound to the next notification.
		# @parameter receipt [Receipt | Nil] The optional receipt used to observe the round.
		# @returns [Round | Nil] The round of waiters, or `nil` if there were none.
		def exchange(receipt = nil)
			waiters = []
			ready = nil

			@mutex.synchronize do
				# Detach each waiter, so that its own cleanup does not disturb the list of the next round:
				while waiter = @waiting.shift
					waiters << waiter
				end

				# The round owns the queue the detached waiters registered with, so rotate it while holding the lock:
				ready = @ready
				@ready = ::Thread::Queue.new
			end

			return nil if waiters.empty?

			return Round.new(waiters, ready, receipt)
		end
	end
end
