# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async"

module Async
	# A controllable monotonic clock for deterministic timeout tests.
	#
	# Time only moves when {#advance} is called, so tests can cross timer
	# expiry points without sleeping for real wall-clock time. Pair it with a
	# scheduler constructed with `clock: fake_clock` so that both the timer
	# collection and any {Deadline} share the same frozen time source.
	#
	# Because the underlying selector still waits for real time, blocking
	# operations must only be entered after the clock has been advanced past
	# the relevant timer point, at which moment the timer fires immediately.
	class FakeClock
		# Create a new fake clock.
		#
		# @parameter now [Numeric] The initial time reported by the clock.
		def initialize(now = 0.0)
			@now = now.to_f
		end

		# @returns [Float] The current fake time.
		attr_reader :now

		# Move the clock forward by the given duration.
		#
		# @parameter duration [Numeric] The number of seconds to advance.
		# @returns [Float] The new current time.
		def advance(duration)
			@now += duration.to_f
		end

		# Reset the clock to the given time.
		#
		# @parameter now [Numeric] The time to reset to.
		# @returns [Float] The new current time.
		def reset!(now = 0.0)
			@now = now.to_f
		end
	end
end
