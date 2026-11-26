# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

module Async
	# Raised if a timeout occurs on a specific Fiber. Handled gracefully by `Task`.
	# @public Since *Async v1*.
	class TimeoutError < StandardError
		# Create a new timeout error.
		#
		# @parameter message [String] The error message.
		# @parameter timeout [Async::Timeout | Nil] The timeout which caused the error, if available.
		def initialize(message = "execution expired", timeout: nil)
			super(message)

			@timeout = timeout
		end

		# The timeout which caused this error.
		#
		# When timeouts are nested, this can be used to determine which layer
		# actually ended the task, and whether an outer deadline is still in
		# effect via {Async::Timeout#parent}.
		#
		# @returns [Async::Timeout | Nil] The timeout which caused the error.
		attr_accessor :timeout
	end
end
