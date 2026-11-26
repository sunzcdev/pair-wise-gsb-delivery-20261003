# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025, by Shopify Inc.
# Copyright, 2025, by Samuel Williams.

require_relative "clock"

# @namespace
module Async
	# Represents a deadline timeout with decrementing remaining time.
	# Includes an efficient representation for zero (non-blocking) timeouts.
	# @public Since *Async v2.31*.
	class Deadline
		# Singleton module for immediate timeouts (zero or negative).
		# Avoids object allocation for fast path (non-blocking) timeouts.
		module Zero
			# Check if the deadline has expired.
			# @returns [Boolean] Always returns true since zero timeouts are immediately expired.
			def self.expired?
				true
			end

			# Get the remaining time.
			# @returns [Integer] Always returns 0 since zero timeouts have no remaining time.
			def self.remaining
				0
			end

			# Get the absolute time at which the deadline expires.
			# A zero timeout expires immediately, i.e. at the current time.
			# @parameter clock [Module] The clock used to read the current time.
			# @returns [Numeric] The current time according to the given clock.
			def self.time(clock = Clock)
				clock.now
			end

			# A zero deadline has no meaningful duration.
			# @returns [Integer] Always returns 0.
			def self.duration
				0
			end
		end

		# Create a deadline for the given timeout.
		# @parameter timeout [Numeric | Nil] The timeout duration, or nil for no timeout.
		# @parameter clock [Module] The clock used to measure elapsed time.
		# @returns [Deadline | Nil] A deadline instance, Zero singleton, or nil.
		def self.start(timeout, clock: Clock)
			if timeout.nil?
				nil
			elsif timeout <= 0
				Zero
			else
				self.new(timeout, clock: clock)
			end
		end

		# Create a new deadline with the specified remaining time.
		# @parameter remaining [Numeric] The initial remaining time.
		# @parameter clock [Module] The clock used to measure elapsed time.
		def initialize(remaining, clock: Clock)
			@remaining = remaining
			@duration = remaining
			@clock = clock
			@start = clock.now
		end

		# @returns [Module] The clock used to measure elapsed time.
		attr :clock

		# Get the absolute time at which the deadline expires, in the time
		# space of the associated clock. Unlike {#remaining}, reading this
		# value does not advance or otherwise mutate the deadline.
		# @returns [Numeric] The absolute expiry time.
		def time
			@start + @remaining
		end

		# @returns [Numeric] The initial timeout duration given when the deadline was created.
		attr :duration

		# Get the remaining time, updating internal state.
		# Each call to this method advances the internal clock and reduces
		# the remaining time by the elapsed duration since the last call.
		# @returns [Numeric] The remaining time (may be negative if expired).
		def remaining
			now = @clock.now
			delta = now - @start
			@start = now

			@remaining -= delta

			return @remaining
		end

		# Check if the deadline has expired.
		# @returns [Boolean] True if no time remains.
		def expired?
			self.remaining <= 0
		end
	end
end
