# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "sus/fixtures/async"

require "async/scheduler"

# A minimal selector which mimics the `io-event` ready list semantics while exposing a per-iteration cap on the number of ready fibers that are resumed. It also records, on every selector pass, the point in time at which I/O was serviced and how much ready work remained queued.
class CappedFakeSelector
	# A single-shot timer that becomes ready after the given number of selector iterations.
	Timer = Struct.new(:remaining_iterations, :fiber) do
		def ready?
			self.remaining_iterations <= 0
		end
	end

	# A fiber blocked waiting for an IO to become readable.
	Waiter = Struct.new(:io, :fiber, :events, :serviced)

	attr :ready
	attr :waiting
	attr :timers
	attr :idle_duration

	# Records one entry per selector pass: the monotonic time and the number of ready fibers still queued after the capped portion was processed.
	attr :select_snapshots

	# Records the time at which a registered read was serviced.
	attr :io_serviced_at

	def initialize(loop)
		@loop = loop

		@ready = []
		@waiting = []
		@timers = []

		@idle_duration = 0.0
		@select_snapshots = []
		@io_serviced_at = []
	end

	def close
		@loop = nil
		@ready.clear
		@waiting.clear
		@timers.clear
	end

	def closed?
		@loop.nil?
	end

	def loop
		@loop
	end

	def wakeup
		false
	end

	# Append the given fiber to the end of the ready list, preserving relative ordering between ready fibers.
	def push(fiber)
		@ready.push(fiber)
	end

	def transfer
		@loop.transfer
	end

	def resume(fiber, *arguments)
		@ready.push(Fiber.current)
		fiber.transfer(*arguments)
	end

	def yield
		@ready.push(Fiber.current)
		@loop.transfer
	end

	def ready?
		!@ready.empty?
	end

	# Register a timer that fires after the given number of selector iterations.
	def after(iterations, &block)
		fiber = Fiber.new(blocking: false, &block)
		timer = Timer.new(iterations, fiber)
		@timers.push(timer)
		return timer
	end

	# Register interest in readability of the given IO. Returns the ready event mask once the IO is serviced, mirroring the `io-event` selector contract.
	#
	# The scheduler retries the wait when it receives a falsy result. A subsequent non-blocking read may itself re-enter `#io_wait` once the data has been drained, which is a fresh wait rather than a spurious wake-up, so a new waiter is registered in that case.
	def io_wait(fiber, io, events)
		waiter = @waiting.find { |candidate| candidate.io.equal?(io) && candidate.fiber.equal?(fiber) }

		if waiter.nil?
			waiter = Waiter.new(io, fiber, events, false)
			@waiting.push(waiter)
			@loop.transfer
		end

		# Only report the events once the waiter has actually been serviced:
		if waiter.serviced
			@waiting.delete(waiter)
			return events
		end

		return false
	end

	# Perform one non-blocking selector pass.
	#
	# When `max_ready_count` is given, at most that many ready fibers are resumed in FIFO order and any remaining ready work is deferred to the next pass. Registered IOs are still polled and timers are still advanced on every pass, so neither can be starved by fibers that keep re-queuing themselves.
	def select(duration = nil, max_ready_count: nil)
		# Advance and fire any timers that are due:
		@timers.each do |timer|
			timer.remaining_iterations -= 1
		end

		due_timers = @timers.select(&:ready?)
		@timers -= due_timers
		due_timers.each do |timer|
			@ready.push(timer.fiber) if timer.fiber.alive?
		end

		# Service any registered IOs that are currently ready without blocking. A waiter is only moved back onto the ready list once; subsequent polls while it is awaiting its retry are ignored.
		readable, _, _ = ::IO.select(@waiting.map(&:io), nil, nil, 0)
		if readable
			readable.each do |io|
				waiter = @waiting.find { |candidate| candidate.io.equal?(io) && !candidate.serviced }
				next unless waiter

				waiter.serviced = true
				@io_serviced_at.push(::Process.clock_gettime(::Process::CLOCK_MONOTONIC))
				@ready.push(waiter.fiber) if waiter.fiber.alive?
			end
		end

		# Resume up to the cap of ready fibers while preserving their relative order:
		count = if max_ready_count
			[@ready.size, max_ready_count].min
		else
			@ready.size
		end

		count.times do
			fiber = @ready.shift
			fiber.transfer if fiber.alive?
		end

		# Record how much ready work remained after this pass so the test can observe progress:
		@select_snapshots.push([
			::Process.clock_gettime(::Process::CLOCK_MONOTONIC),
			@ready.size,
		])

		return count
	end
end

describe Async::Scheduler do
	with "max_ready_count" do
		let(:selector) {CappedFakeSelector.new(Fiber.current)}
		let(:scheduler) {Async::Scheduler.new(selector: selector, max_ready_count: 1)}

		after do
			Fiber.set_scheduler(nil)
			scheduler.close
		end

		it "services I/O and timers while a fiber keeps re-queuing itself" do
			Fiber.set_scheduler(scheduler)

			input, output = ::IO.pipe

			# Make the read immediately serviceable:
			output.write(".")
			output.close

			readable_io = nil
			busy_iterations = 0
			timer_fired = false

			scheduler.run do |task|
				# A read registered with the fake selector. `wait_readable` performs exactly one `io_wait` and returns the IO once it has been serviced:
				task.async do
					readable_io = input.wait_readable
				end

				# A fiber that perpetually pushes its next chunk of work back onto the ready list:
				task.async do
					10.times do
						busy_iterations += 1
						Fiber.scheduler.yield
					end
				end

				# A timer registered with the fake selector:
				selector.after(2) do
					timer_fired = true
				end
			end

			# The read was serviced even though another fiber was continuously re-queued:
			expect(readable_io).to be_equal(input)
			expect(selector.io_serviced_at).not.to be(:empty?)

			# The timer fired:
			expect(timer_fired).to be == true

			# The re-queuing fiber made forward progress on every chunk of work:
			expect(busy_iterations).to be == 10

			# The selector was entered on multiple passes, each handling at most the cap of one ready fiber while leaving the rest queued:
			expect(selector.select_snapshots.size).to be > 1

			# Ready work was left over on some passes and fully drained by the end, proving both starvation protection and forward progress:
			remaining = selector.select_snapshots.map(&:last)
			expect(remaining).to be(:any?) { |count| count > 0 }
			expect(remaining.last).to be == 0
		ensure
			input&.close
		end

		it "falls back to a plain duration select when the selector does not support a cap" do
			selector = Object.new
			arguments = nil

			selector.define_singleton_method(:select) do |duration|
				arguments = duration
			end
			selector.define_singleton_method(:idle_duration) {0.0}
			selector.define_singleton_method(:close) {}

			scheduler = Async::Scheduler.new(selector: selector, max_ready_count: 4)

			scheduler.send(:run_once!)

			# No keyword was forwarded, and the duration-based select path was used:
			expect(arguments).to be_nil
		ensure
			scheduler&.close
		end

		it "detects support through a keyword rest argument" do
			selector = Object.new
			received = nil

			selector.define_singleton_method(:select) do |duration, **options|
				received = [duration, options]
			end
			selector.define_singleton_method(:idle_duration) {0.0}
			selector.define_singleton_method(:close) {}

			scheduler = Async::Scheduler.new(selector: selector, max_ready_count: 4)

			scheduler.send(:run_once!)

			expect(received).to be == [0, {max_ready_count: 4}]
		ensure
			scheduler&.close
		end
	end
end
