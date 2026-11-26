# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025, by Shopify Inc.
# Copyright, 2025-2026, by Samuel Williams.
# Copyright, 2026, by Kyle Tate.

require_relative "error"
require_relative "deadline"
require_relative "cancel"

module Async
	# A promise represents a value that will be available in the future.
	# Unlike Condition, once resolved (or rejected), all future waits return immediately
	# with the stored value or raise the stored exception.
	#
	# This is thread-safe and integrates with the fiber scheduler.
	#
	# @public Since *Async v2*.
	class Promise
		# A non-raising, attributable result of observing a promise.
		#
		# Timeouts produce `:timeout` observations and never change the state of the
		# promise. Rejections produce `:failed` observations which can be explicitly
		# marked as handled by the observer that consumed the error.
		class Observation
			TERMINAL_STATES = %i[completed failed cancelled].freeze
			ERROR_STATES = %i[failed cancelled].freeze

			# @returns [Promise] The observed promise.
			attr_reader :promise

			# @returns [Integer | Nil] The delivery identifier, or nil for a timeout.
			attr_reader :id

			# @returns [Symbol] `:pending`, `:timeout`, `:completed`, `:failed`, or `:cancelled`.
			attr_reader :state

			# @returns [Object | Exception | Nil] The result, stored exception, or nil on timeout.
			attr_reader :value

			def initialize(promise, state, value = nil, id = nil, handled = false)
				@promise = promise
				@id = id
				@state = state || :pending
				@value = value
				@handled = handled
			end

			# @returns [Boolean] Whether the observation timed out.
			def timeout?
				@state == :timeout
			end
			alias timed_out? timeout?

			# @returns [Boolean] Whether the promise was still pending when observed.
			def pending?
				@state == :pending
			end

			# @returns [Boolean] Whether the promise had reached a terminal state.
			def resolved?
				TERMINAL_STATES.include?(@state)
			end

			# @returns [Boolean] Whether the promise completed successfully.
			def completed?
				@state == :completed
			end
			alias fulfilled? completed?
			alias successful? completed?

			# @returns [Boolean] Whether the promise was rejected.
			def failed?
				@state == :failed
			end
			alias rejected? failed?

			# @returns [Boolean] Whether the promise was cancelled.
			def cancelled?
				@state == :cancelled
			end

			# @returns [Boolean] Whether this observation carries an exception or cancellation.
			def error?
				ERROR_STATES.include?(@state)
			end
			alias exceptional? error?

			# @returns [Exception | Nil] The rejected exception.
			def error
				@value if failed?
			end
			alias exception error

			# @returns [Exception | Nil] The rejection or cancellation reason.
			def reason
				@value if error?
			end

			# @returns [Boolean] Whether this observer explicitly marked the result handled.
			def handled?
				@handled
			end
			alias marked_handled? handled?

			# Explicitly mark this observation as handled.
			#
			# Timeout and successful observations cannot mark a later rejection as handled.
			#
			# @returns [self]
			def handle!
				return self unless error?

				@promise.send(:mark_observation_handled!, self)
				@handled = true
				self
			end
			alias mark_handled! handle!
			alias mark_as_handled! handle!
			alias handled! handle!
			alias mark_handled handle!
			alias mark_as_handled handle!
			alias handle handle!

			# Raise the stored failure, mirroring {Promise#wait}.
			#
			# @returns [Object] The successful value.
			# @raises [Exception] The stored failure or cancellation reason.
			def unwrap!
				if error? && @value
					raise @value
				end

				@value
			end
			alias raise! unwrap!
			alias value! unwrap!

			# @returns [Hash] A machine-readable representation of the observation.
			def to_h
				{
					id: @id,
					state: @state,
					status: @state,
					value: @value,
					error: error,
					reason: reason,
					timeout: timeout?,
					resolved: resolved?,
					completed: completed?,
					failed: failed?,
					rejected: failed?,
					cancelled: cancelled?,
					handled: handled?,
				}
			end
			alias as_json to_h

			# Access observation fields by symbol or string.
			def [](key)
				to_h[key.to_sym]
			end

			# @private
			alias status state

			# @private
			alias kind state
		end

		# Compatibility names for callers that refer to the attributable result as an observer/result.
		Observer = Observation
		Result = Observation

		# Create a new promise.
		def initialize
			# nil = pending, :completed = success, :failed = failure, :cancelled = cancelled:
			@resolved = nil

			# Stores either the result value or the exception:
			@value = nil

			# Track how many fibers are currently waiting:
			@waiting = 0

			# Monotonic delivery identity and observation/handling counters:
			@observation_serial = 0
			@observations = 0
			@failure_observations = 0
			@handled_observations = {}
			@handled_count = 0
			@warning_reported = false

			@mutex = Mutex.new
			@condition = ConditionVariable.new
		end

		# @returns [Boolean] Whether the promise has been resolved or rejected.
		def resolved?
			@mutex.synchronize{!!@resolved}
		end
		
		# @returns [Symbol | Nil] The internal resolved state (:completed, :failed, :cancelled, or nil if pending).
		# @private For internal use by Task.
		def resolved
			@mutex.synchronize{@resolved}
		end
		
		# @returns [Boolean] Whether the promise has been cancelled.
		def cancelled?
			@mutex.synchronize{@resolved == :cancelled}
		end
		
		# @returns [Boolean] Whether the promise failed with an exception.
		def failed?
			@mutex.synchronize{@resolved == :failed}
		end
		
		# @returns [Boolean] Whether the promise has completed successfully.
		def completed?
			@mutex.synchronize{@resolved == :completed}
		end
		
		# @returns [Boolean] Whether any fibers are currently waiting for this promise.
		def waiting?
			@mutex.synchronize{@waiting > 0}
		end

		# @returns [Integer] Number of current waiters, including artificial warning suppressions.
		def waiting_count
			@mutex.synchronize{@waiting}
		end
		
		# Artificially mark that someone is waiting (useful for suppressing warnings).
		# @private Internal use only.
		def suppress_warnings!
			@mutex.synchronize{@waiting += 1}
		end

		# @returns [Integer] Number of terminal deliveries observed through {#wait}, {#wait?}, or {#observe}.
		def observation_count
			@mutex.synchronize{@observations}
		end

		# @returns [Integer] Number of rejected deliveries observed through waiting or observation.
		def failure_observation_count
			@mutex.synchronize{@failure_observations}
		end

		# Whether a rejection has actually been delivered to at least one caller.
		#
		# A timeout that returns before rejection does not count.
		#
		# @returns [Boolean]
		def observed?
			@mutex.synchronize{@failure_observations > 0}
		end
		alias seen? observed?

		# Whether at least one observer explicitly marked a result as handled.
		#
		# @returns [Boolean]
		def handled?
			@mutex.synchronize{@handled_count > 0}
		end
		alias marked_handled? handled?
		alias processed? handled?

		# @returns [Integer] Number of explicit handling marks.
		def handled_count
			@mutex.synchronize{@handled_count}
		end

		# Explicitly mark the terminal failure as handled without attributing the mark
		# to a specific {Observation}.
		#
		# @returns [self]
		def mark_handled!
			@mutex.synchronize{@handled_count += 1}
			self
		end
		alias handle! mark_handled!
		alias handled! mark_handled!
		alias mark_as_handled! mark_handled!
		alias mark_handled mark_handled!
		alias handle mark_handled!
		alias mark_as_handled mark_handled!

		# Whether a rejection still lacks an explicit handling mark.
		#
		# Note that a caller may have observed the exception and rescued it without
		# calling {#mark_handled!}; warning decisions should normally use {#unobserved?}.
		#
		# @returns [Boolean]
		def unhandled?
			@mutex.synchronize{@resolved == :failed && @handled_count == 0}
		end

		# Whether there is no current waiter, no delivered rejection observation, and
		# no explicit handling mark.
		#
		# This intentionally does not treat an earlier timeout as observation of a
		# rejection: that waiter left before the rejection existed.
		#
		# @returns [Boolean]
		def unobserved?
			@mutex.synchronize{unobserved_locked?}
		end

		# Whether this is a failed promise with no current or delivered observer.
		#
		# @returns [Boolean]
		def unobserved_rejection?
			@mutex.synchronize{@resolved == :failed && unobserved_locked?}
		end

		# Whether an unobserved rejection should produce a warning.
		#
		# @returns [Boolean]
		def should_warn?
			unobserved_rejection? && !warning_reported?
		end
		alias warning? should_warn?

		# @returns [Boolean] Whether {#warn_if_unobserved!} has already emitted a warning.
		def warning_reported?
			@mutex.synchronize{@warning_reported}
		end

		# A compact terminal report suitable for diagnostics or supervisor reports.
		#
		# @returns [Hash]
		def observation_report
			@mutex.synchronize do
				{
					state: @resolved || :pending,
					resolved: !@resolved.nil?,
					waiting: @waiting,
					observations: @observations,
					failure_observations: @failure_observations,
					handled: @handled_count > 0,
					handled_count: @handled_count,
					observed: @failure_observations > 0,
					unobserved: unobserved_locked?,
					unhandled: @resolved == :failed && @handled_count == 0,
					warning_reported: @warning_reported,
				}
			end
		end
		alias report observation_report
		alias terminal_report observation_report
		alias state_report observation_report

		# Emit a warning if this rejected promise has no observer or handling mark.
		#
		# @returns [Boolean] Whether a warning was emitted.
		def warn_if_unobserved!
			error = nil

			@mutex.synchronize do
				return false unless @resolved == :failed
				return false if @warning_reported
				return false unless unobserved_locked?

				@warning_reported = true
				error = @value
			end

			Kernel.warn(format_unobserved_warning(error))
			true
		end
		alias warn_unhandled! warn_if_unobserved!
		alias warn_unhandled_rejection! warn_if_unobserved!
		
		# Non-blocking access to the current value. Returns nil if not yet resolved.
		# Does not raise exceptions even if the promise was rejected or cancelled.
		# For resolved promises, returns the raw stored value (result, exception, or cancel exception).
		#
		# @returns [Object | Nil] The stored value, or nil if pending.
		def value
			@mutex.synchronize{@resolved ? @value : nil}
		end
		
		# Wait for a result and assign the delivery identity used by {Observation}.
		#
		# @returns [Array(Symbol | Nil, Integer | Nil)]
		private def wait_for_observation(timeout: nil)
			@mutex.synchronize do
				unless @resolved
					# Increment waiting count:
					@waiting += 1
					timed_out = false

					begin
						# Wait for resolution if not already resolved:
						unless @resolved
							if timeout.nil?
								wait_indefinitely
							else
								timed_out = !wait_with_timeout(timeout)
							end
						end
					ensure
						# Decrement waiting count when done:
						@waiting -= 1
					end

					# Return inside the mutex so timeout cannot race with a state transition:
					return [nil, nil] if timed_out
				end

				state = @resolved
				id = (@observation_serial += 1)
				@observations += 1
				@failure_observations += 1 if state == :failed

				[state, id]
			end
		end

		# Wait indefinitely for the promise to be resolved.
		private def wait_indefinitely
			until @resolved
				@condition.wait(@mutex)
			end
		end
		
		# Wait for the promise to be resolved, respecting the deadline timeout.
		# @parameter timeout [Numeric] The timeout duration.
		# @returns [Boolean] True if resolved, false if timeout expires.
		private def wait_with_timeout(timeout)
			# Create deadline for timeout tracking:
			deadline = Deadline.start(timeout)
			
			# Handle immediate timeout (non-blocking):
			if deadline == Deadline::Zero && !@resolved
				return false
			end
			
			# Wait with deadline tracking:
			loop do
				break if @resolved
				
				# Get remaining time for this wait iteration:
				remaining = deadline.remaining
				
				# Check if deadline has expired before waiting:
				if remaining <= 0
					return false
				end
				
				@condition.wait(@mutex, remaining)
			end
			
			return true
		end
		
		# Wait for the promise to be resolved (without raising exceptions).
		#
		# If already resolved, returns immediately. Otherwise, waits until resolution or timeout.
		#
		# @parameter timeout [Numeric | Nil] Maximum time to wait. If nil, waits indefinitely. If 0, returns immediately if not resolved.
		# @returns [Symbol | Nil] `:completed`, `:failed`, or `:cancelled` when resolved; nil if timeout expires.
		def wait?(timeout: nil)
			state, _id = wait_for_observation(timeout: timeout)
			state
		end

		# Wait and return an attributable, non-raising {Observation}.
		#
		# A timeout observation has state `:timeout` and does not mutate the promise.
		# A failed observation retains the original rejection and can be marked handled.
		#
		# @parameter timeout [Numeric | Nil] Maximum time to wait.
		# @parameter handle [Boolean] Immediately mark a terminal error observation as handled.
		# @returns [Observation]
		def observe(timeout: nil, handle: false)
			state, id = wait_for_observation(timeout: timeout)

			if state.nil?
				observation = Observation.new(self, :timeout, nil)
			else
				observation = Observation.new(self, state, @value, id)
				observation.handle! if handle && observation.error?
			end

			observation
		end
		alias wait_result observe
		alias observe_result observe

		# Wait for the promise to be resolved and return the value.
		#
		# If already resolved, returns immediately. If rejected, raises the stored exception.
		#
		# @returns [Object] The resolved value.
		# @raises [Exception] The rejected or cancelled exception.
		# @raises [Async::TimeoutError] If timeout expires before the promise is resolved.
		def wait(...)
			resolved, _id = wait_for_observation(...)

			if resolved.nil?
				raise TimeoutError, "Timeout while waiting for promise!"
			elsif resolved == :completed
				return @value
			elsif @value
				# If we aren't completed, we should have an exception or cancel reason stored:
				raise @value
			end
		end
		
		# Resolve the promise with a value.
		# All current and future waiters will receive this value.
		# Can only be called once - subsequent calls are ignored.
		#
		# @parameter value [Object] The value to resolve the promise with.
		def resolve(value)
			@mutex.synchronize do
				return if @resolved
				
				@resolved = :completed
				@value = value
				
				# Wake up all waiting fibers:
				@condition.broadcast
			end
			
			return value
		end
		
		# Reject the promise with an exception.
		# All current and future waiters will receive this exception.
		# Can only be called once - subsequent calls are ignored.
		#
		# @parameter exception [Exception] The exception to reject the promise with.
		def reject(exception)
			@mutex.synchronize do
				return if @resolved
				
				@resolved = :failed
				@value = exception
				
				# Wake up all waiting fibers:
				@condition.broadcast
			end
			
			return nil
		end
		
		# Cancel the promise, indicating cancellation.
		# All current and future waiters will receive nil.
		# Can only be called on pending promises - no-op if already resolved.
		def cancel(exception = Cancel.new("Promise cancelled!"))
			@mutex.synchronize do
				# No-op if already in any final state
				return if @resolved
				
				@resolved = :cancelled
				@value = exception
				
				# Wake up all waiting fibers:
				@condition.broadcast
			end
			
			return nil
		end
		
		# Resolve the promise with the result of the block.
		# If the block raises an exception, the promise will be rejected.
		# If the promise was already resolved, the block will not be called.
		# @yields {...} The block to call to resolve the promise.
		# @returns [Object] The result of the block.
		def fulfill(&block)
			raise "Promise already resolved!" if @resolved
			
			begin
				return self.resolve(yield)
			rescue Cancel => exception
				return self.cancel(exception)
			rescue => error
				return self.reject(error)
			rescue Exception => exception
				self.reject(exception)
				raise
			ensure
				# Handle non-local exits (throw, etc.) that bypass normal flow:
				self.resolve(nil) unless @resolved
			end
		end
		
		# If a promise is given, fulfill it with the result of the block.
		# If no promise is given, simply yield to the block.
		# This is useful for methods that may optionally take a promise to fulfill.
		# @parameter promise [Promise | Nil] The optional promise to fulfill.
		# @yields {...} The block to call to resolve the promise or return a value.
		# @returns [Object] The result of the block.
		def self.fulfill(promise, &block)
			if promise
				return promise.fulfill(&block)
			else
				return yield
			end
		end

		private

		# Must be called while holding `@mutex`.
		def unobserved_locked?
			@waiting == 0 && @failure_observations == 0 && @handled_count == 0
		end

		# Attribute an explicit handling mark to one observation, making duplicate
		# calls on the same observation idempotent.
		def mark_observation_handled!(observation)
			@mutex.synchronize do
				id = observation.id

				if id && !@handled_observations.key?(id)
					@handled_observations[id] = true
					@handled_count += 1
				end
			end
		end

		def format_unobserved_warning(error)
			if error
				"Async::Promise rejection was not observed or handled: #{error.class}: #{error.message}"
			else
				"Async::Promise rejection was not observed or handled."
			end
		end
	end
end
