# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/scheduler"

describe Async::Scheduler do
	# A selector double that records wait registrations instead of interacting
	# with real file descriptors. Events queued with #release are delivered on
	# the next #select iteration, mirroring how a readiness notification moves
	# through the real selector's ready queue.
	#
	# The object registered for a wait is the scheduler's wait registration
	# wrapper, which quacks like a fiber (#alive?, #transfer); the double only
	# needs to forward readiness to it.
	class SelectorDouble
		def initialize(loop)
			@loop = loop
			@waits = []
			@events = Thread::Queue.new
		end
		
		attr :waits
		
		def io_wait(registration, io, events)
			@waits << [registration, io, events]
			
			@loop.transfer || false
		ensure
			@waits.delete_if {|waiting| waiting.first.equal?(registration)}
		end
		
		# Queue a readiness result for delivery on the next #select call.
		def release(registration, result = nil)
			@events.push([registration, result])
		end
		
		def select(duration = nil)
			while event = (@events.pop(true) rescue nil)
				registration, result = event
				
				if registration.alive?
					registration.transfer(result)
				end
			end
		end
		
		def push(fiber)
			@events.push([fiber, nil])
		end
		
		def wakeup
		end
		
		def transfer
			@loop.transfer
		end
		
		def resume(fiber, *arguments)
			push(Fiber.current)
			fiber.transfer(*arguments)
		end
		
		def raise(fiber, *arguments, **options)
			push(Fiber.current)
			fiber.raise(*arguments, **options)
		end
		
		def yield
			push(Fiber.current)
			@loop.transfer
		end
		
		def idle_duration
			0.0
		end
		
		def close
		end
	end
	
	with "#io_wait" do
		it "does not settle a new wait with a notification queued for a previous wait" do
			# The double replaces the scheduler's selector, so the scenario runs on
			# its own thread and scheduler, independent of the one running the test.
			thread = Thread.new do
				# A real selector is only used to obtain the scheduler's event-loop
				# fiber, which the double must transfer back to:
				native = ::IO::Event::Selector.new(Fiber.current)
				selector = SelectorDouble.new(native.loop)
				scheduler = Async::Scheduler.new(selector: selector)
				Fiber.set_scheduler(scheduler)
				
				socket, peer = Socket.pair(:UNIX, :STREAM, 0)
				
				outcome = nil
				read_registration = nil
				write_registration = nil
				
				begin
					task = scheduler.async do
						begin
							outcome = scheduler.io_wait(socket, IO::READABLE)
						rescue Async::Cancel
							# The read wait is cancelled, but the same fiber
							# immediately registers a new wait for the write end:
							outcome = scheduler.io_wait(socket, IO::WRITABLE)
						end
					end
					
					# The task is parked in the read wait:
					expect(selector.waits.size).to be == 1
					read_registration = selector.waits.first.first
					
					# Queue a stale read notification before the wait is cancelled:
					selector.release(read_registration, IO::READABLE)
					task.cancel
					
					# This iteration delivers the stale read notification. It must
					# not settle the write wait the fiber registers after rescuing
					# the cancellation:
					scheduler.run_once
					
					expect(selector.waits.size).to be == 1
					write_registration = selector.waits.first.first
					expect(write_registration).not.to be_equal(read_registration)
					
					# Only a genuine write notification may resume the new wait:
					selector.release(write_registration, IO::WRITABLE)
					scheduler.run_once
					
					expect(task).to be(:completed?)
					expect(outcome).to be == IO::WRITABLE
				ensure
					socket&.close
					peer&.close
					native&.close
					Fiber.set_scheduler(nil)
					scheduler&.close
				end
				
				outcome
			end
			
			thread.report_on_exception = false
			
			expect(thread.value).to be == IO::WRITABLE
		end
		
		it "keeps the read wait cancelled while the stale notification is drained" do
			thread = Thread.new do
				native = ::IO::Event::Selector.new(Fiber.current)
				selector = SelectorDouble.new(native.loop)
				scheduler = Async::Scheduler.new(selector: selector)
				Fiber.set_scheduler(scheduler)
				
				socket, peer = Socket.pair(:UNIX, :STREAM, 0)
				
				read_task = nil
				write_task = nil
				read_registration = nil
				write_registration = nil
				
				begin
					read_task = scheduler.async do
						scheduler.io_wait(socket, IO::READABLE)
					end
					
					write_task = scheduler.async do
						scheduler.io_wait(socket, IO::WRITABLE)
					end
					
					expect(selector.waits.size).to be == 2
					read_registration = selector.waits.first.first
					write_registration = selector.waits.last.first
					
					# Hold the old read notification in the ready queue, then cancel
					# the read wait before it can be delivered:
					selector.release(read_registration, IO::READABLE)
					read_task.cancel
					scheduler.run_once
					
					# The read task stays cancelled; the stale notification cannot
					# revive it. The write task is still waiting:
					expect(read_task).to be(:cancelled?)
					expect(write_task).to be(:running?)
					
					# The write wait continues only on a genuine write notification:
					selector.release(write_registration, IO::WRITABLE)
					scheduler.run_once
					
					expect(write_task).to be(:completed?)
					expect(write_task.result).to be == IO::WRITABLE
				ensure
					socket&.close
					peer&.close
					native&.close
					Fiber.set_scheduler(nil)
					scheduler&.close
				end
				
				[read_task, write_task]
			end
			
			thread.report_on_exception = false
			read_task, write_task = thread.value
			
			expect(read_task).to be(:cancelled?)
			expect(write_task).to be(:completed?)
		end
	end
end
