# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Shopify Inc.

require "async/test/fake_clock"
require "async/scheduler"

# These tests drive a scheduler with a controllable clock so that nested
# deadline expiry can be observed deterministically. The blocking selector
# still uses real time, so blocking operations are only entered once the
# fake clock has crossed the relevant timer point.
describe "nested timeouts with a controllable clock" do
	let(:clock) {Async::FakeClock.new(1000.0)}

	# Each test gets a fresh scheduler bound to its own fake clock:
	def run
		clock = self.clock
		scheduler = Async::Scheduler.new(clock: clock)

		Fiber.blocking do
			Fiber.set_scheduler(scheduler)

			begin
				scheduler.run do |task|
					yield task, scheduler, clock
				end.wait
			ensure
				Fiber.set_scheduler(nil)
				scheduler.close
			end
		end
	end

	it "uses the same time space for the timer and the deadline" do
		run do |task, scheduler, clock|
			scheduler.with_timeout(10) do |timeout|
				expect(timeout.time).to be == 1010.0
				expect(timeout.deadline.time).to be == 1010.0
				expect(timeout.deadline.clock).to be == clock
			end
		end
	end

	it "reports the effective deadline of nested layers" do
		run do |task, scheduler, clock|
			scheduler.with_timeout(10) do |outer|
				scheduler.with_timeout(4) do |inner|
					expect(inner.time).to be == 1004.0
					expect(outer.time).to be == 1010.0

					expect(inner.effective_time).to be == 1004.0
					expect(inner).to be(:effective?)

					expect(outer.effective_time).to be == 1010.0
				end
			end
		end
	end

	it "clamps the inner effective deadline to a shorter outer layer" do
		run do |task, scheduler, clock|
			scheduler.with_timeout(2) do |outer|
				scheduler.with_timeout(10) do |inner|
					expect(inner.time).to be == 1010.0
					expect(outer.time).to be == 1002.0

					expect(inner.effective_time).to be == 1002.0
					expect(inner).to be(:constrained?)
					expect(inner).not.to be(:effective?)
				end
			end
		end
	end

	it "lets A wait under the short inner deadline while B runs in another fiber" do
		b_seen_timeouts = nil

		run do |task, scheduler, clock|
			condition = Async::Condition.new

			# A is a separate fiber that blocks under both deadlines:
			worker = task.async do
				scheduler.with_timeout(10) do |outer|
					scheduler.with_timeout(4) do |inner|
						condition.wait

						# A's stack is private to A's fiber and still contains both layers:
						expect(Async::Scheduler.timeouts).to be == [outer, inner]
					end
				end
			end

			# Let A reach the blocking wait before B runs:
			task.yield

			# B runs concurrently and must not observe A's timeout stack:
			b_seen_timeouts = Async::Scheduler.timeouts.dup
			condition.signal(:ready)

			worker.wait
		end

		# B never saw A's timeouts:
		expect(b_seen_timeouts).to be == []
	end

	it "ends the task at the inner layer and keeps the outer deadline" do
		inner_layer = nil
		outer_layer = nil

		run do |task, scheduler, clock|
			scheduler.with_timeout(10) do |outer|
				outer_layer = outer

				begin
					scheduler.with_timeout(4) do |inner|
						inner_layer = inner

						# Cross the inner expiry point while inside the block:
						clock.advance(4.1)

						# The blocking operation is interrupted by the inner timer:
						sleep(1)
					end
				rescue Async::TimeoutError => error
					expect(error.timeout).to be == inner_layer
					expect(error.timeout.parent).to be == outer
				end

				# The inner timer was cleaned up without erasing the outer deadline:
				expect(inner_layer).to be(:cancelled?)
				expect(outer_layer).not.to be(:cancelled?)
				expect(Async::Scheduler.timeouts).to be == [outer_layer]
				expect(outer_layer.deadline.time).to be == 1010.0
				expect(outer_layer.deadline.remaining).to be_within(0.001).of(5.9)
			end
		end
	end

	it "still fires the outer deadline after crossing the stale inner timer point" do
		fired_layers = []

		run do |task, scheduler, clock|
			scheduler.with_timeout(10) do |outer|
				begin
					scheduler.with_timeout(4) do |inner|
						clock.advance(4.1)
						sleep(1)
					end
				rescue Async::TimeoutError => error
					fired_layers << error.timeout
				end

				# Move well past the old inner point (1004.1) up to the outer point (1010):
				clock.advance(6.0)

				begin
					sleep(1)
				rescue Async::TimeoutError => error
					fired_layers << error.timeout
					expect(error.timeout).to be == outer
					expect(error.timeout.parent).to be_nil
				end
			end
		end

		expect(fired_layers.size).to be == 2
		expect(fired_layers.first.time).to be == 1004.0
		expect(fired_layers.last.time).to be == 1010.0
	end

	it "keeps the first business exception recognizable" do
		business_error = Class.new(StandardError)
		seen = nil

		run do |task, scheduler, clock|
			begin
				scheduler.with_timeout(10) do
					raise business_error, "business failure"
				end
			rescue => error
				seen = error
			end
		end

		expect(seen).to be_a(business_error)
		expect(seen.message).to be == "business failure"
	end

	it "distinguishes a business exception from a later timeout" do
		business_error = Class.new(StandardError)
		events = []

		run do |task, scheduler, clock|
			begin
				scheduler.with_timeout(10) do
					begin
						raise business_error, "first business exception"
					rescue => error
						# The first business exception remains identifiable even though a timeout is pending:
						events << error
						clock.advance(11.0)
						sleep(1)
					end
				end
			rescue Async::TimeoutError => error
				events << error
			end
		end

		expect(events.first).to be_a(business_error)
		expect(events.first.message).to be == "first business exception"
		expect(events.last).to be_a(Async::TimeoutError)
	end
end
