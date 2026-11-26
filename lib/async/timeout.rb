# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025, by Samuel Williams.
# Copyright, 2026, by Shopify Inc.

require "io/event"

require_relative "clock"
require_relative "deadline"

module Async
	# Timer collection whose notion of current time comes from an injectable clock.
	#
	# The default {IO::Event::Timers} reads the monotonic clock directly. Subclassing it allows the scheduler to share one clock with {Deadline}, and to drive deterministic tests with a controllable fake clock.
	class Timers < ::IO::Event::Timers
		# Create a new timer collection.
		#
		# @parameter clock [Module] The clock used to read the current time.
		def initialize(clock = Clock)
			super()

			@clock = clock
		end

		# @returns [Float] The current time according to the injected clock.
		def now
			@clock.now
		end
	end

	# Represents a flexible timeout that can be rescheduled or extended.
	# @public Since *Async v2.24*.
	class Timeout
		# Initialize a new timeout.
		#
		# @parameter timers [IO::Event::Timers] The timer collection used by the scheduler.
		# @parameter handle [IO::Event::Timers::Handle] The underlying timer handle.
		# @parameter deadline [Deadline | Deadline::Zero | Nil] The deadline associated with this timeout, if any.
		# @parameter parent [Timeout | Nil] The enclosing timeout that constrains this one, if nested.
		def initialize(timers, handle, deadline: nil, parent: nil)
			@timers = timers
			@handle = handle
			@deadline = deadline
			@parent = parent
		end

		# @returns [Deadline | Deadline::Zero | Nil] The deadline associated with this timeout.
		attr :deadline

		# @returns [Timeout | Nil] The enclosing timeout, if this timeout is nested within another one.
		attr :parent

		# The absolute time at which this timeout will fire, in the scheduler's monotonic time space.
		# @returns [Numeric] The scheduled fire time.
		def time
			@handle.time
		end

		# The absolute time at which the enclosing scope will actually expire.
		#
		# When this timeout is nested within another one, the effective time is the earlier of this timeout's own fire time and its parent's effective time. This makes it possible to tell which layer is going to end the task.
		#
		# @returns [Numeric] The effective expiry time.
		def effective_time
			if @parent
				[self.time, @parent.effective_time].min
			else
				self.time
			end
		end

		# Whether this timeout is the one that will actually expire first for
		# the enclosing scope, i.e. its own fire time is no later than the
		# effective deadline of any enclosing timeout.
		#
		# @returns [Boolean] Whether this timeout is the effective (binding) timeout.
		def effective?
			if @parent
				self.time <= @parent.effective_time
			else
				true
			end
		end

		# Whether an enclosing timeout will expire before this one, so this
		# timeout's own timer cannot be the one that ends the task.
		#
		# @returns [Boolean] Whether an outer deadline constrains this timeout.
		def constrained?
			!!@parent && @parent.effective_time < self.time
		end

		# @returns [Numeric] The time remaining until the timeout occurs, in seconds.
		def duration
			@handle.time - @timers.now
		end

		# The time remaining until the enclosing scope's effective deadline,
		# which may be shorter than this timeout's own duration when nested.
		#
		# @returns [Numeric] The remaining effective duration, in seconds.
		def effective_duration
			self.effective_time - @timers.now
		end

		# Update the duration of the timeout.
		#
		# The duration is relative to the current time, e.g. setting the duration to 5 means the timeout will occur in 5 seconds from now.
		#
		# @parameter value [Numeric] The new duration to assign to the timeout, in seconds.
		def duration=(value)
			self.reschedule(@timers.now + value)
		end

		# Adjust the timeout by the specified duration.
		#
		# The duration is relative to the timeout time, e.g. adjusting the timeout by 5 increases the current duration by 5 seconds.
		#
		# @parameter duration [Numeric] The duration to adjust the timeout by, in seconds.
		# @returns [Numeric] The new time at which the timeout will occur.
		def adjust(duration)
			self.reschedule(time + duration)
		end

		# Assign a new time to the timeout, rescheduling it if necessary.
		#
		# @parameter value [Numeric] The new time to assign to the timeout.
		# @returns [Numeric] The new time at which the timeout will occur.
		def time=(value)
			self.reschedule(value)
		end

		# @returns [Numeric] The current time in the scheduler, relative to the time of this timeout, in seconds.
		def now
			@timers.now
		end

		# Cancel the timeout, preventing it from executing.
		def cancel!
			@handle.cancel!
		end

		# @returns [Boolean] Whether the timeout has been cancelled.
		def cancelled?
			@handle.cancelled?
		end

		# Raised when attempting to reschedule a cancelled timeout.
		class CancelledError < RuntimeError
		end

		# Reschedule the timeout to occur at the specified time.
		#
		# @parameter time [Numeric] The new time to schedule the timeout for.
		# @returns [Numeric] The new time at which the timeout will occur.
		private def reschedule(time)
			if block = @handle&.block
				@handle.cancel!

				@handle = @timers.schedule(time, block)

				return time
			else
				raise CancelledError, "Cannot reschedule a cancelled timeout!"
			end
		end
	end
end
