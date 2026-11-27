# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2018-2025, by Samuel Williams.

require_relative "condition"

module Async
	# A synchronization primitive, which allows fibers to wait until a notification is received. Does not block the task which signals the notification. Waiting tasks are resumed on next iteration of the reactor.
	# @public Since *Async v1*.
	class Notification < Condition
		# Signal to a given task that it should resume operations.
		#
		# The notification is delivered on the next iteration of the reactor. Fibers which register after this call are bound to the next notification.
		#
		# @parameter value [Object | Nil] The value to return to the waiting fibers.
		# @parameter receipt [Receipt | Boolean | Nil] An optional receipt, or `true` to allocate one (see {#last_receipt}).
		# @yields {|receipt| ...} The receipt for this round, if a block is given.
		# @returns [Boolean] if a task was signalled.
		def signal(value = nil, receipt: nil, &block)
			receipt = self.prepare_receipt(receipt, block_given?)
			
			if round = self.exchange(receipt)
				Fiber.scheduler.push Signal.new(round, value)
			else
				# There were no waiters, so this round is already settled:
				receipt&.settle!
				
				return false
			end
			
			yield(receipt) if block_given?
			
			return true
		end
		
		Signal = Struct.new(:round, :value) do
			def alive?
				true
			end
			
			def transfer
				round.deliver(value)
			end
		end
		
		private_constant :Signal
	end
end
