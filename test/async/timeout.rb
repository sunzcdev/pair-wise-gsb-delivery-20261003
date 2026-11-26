# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025, by Samuel Williams.
# Copyright, 2026, by Shopify Inc.

require "sus/fixtures/async"

describe Async::Timeout do
	include Sus::Fixtures::Async::ReactorContext

	it "can schedule a timeout" do
		scheduler.with_timeout(1) do |timeout|
			expect(timeout.time).to be >= 0
			expect(timeout.duration).to (be > 0).and(be <= 1)
		end
	end

	with "#deadline" do
		it "exposes an associated deadline" do
			scheduler.with_timeout(2) do |timeout|
				expect(timeout.deadline).to be_a(Async::Deadline)
				expect(timeout.deadline.duration).to be == 2
			end
		end

		it "matches the timer's fire time" do
			scheduler.with_timeout(1) do |timeout|
				expect(timeout.deadline.time).to be_within(0.001).of(timeout.time)
			end
		end
	end

	with "#parent" do
		it "is nil for an outer timeout" do
			scheduler.with_timeout(1) do |timeout|
				expect(timeout.parent).to be_nil
			end
		end

		it "references the enclosing timeout when nested" do
			scheduler.with_timeout(10) do |outer|
				scheduler.with_timeout(1) do |inner|
					expect(inner.parent).to be == outer
					expect(outer.parent).to be_nil
				end
			end
		end
	end

	with "#effective_time" do
		it "equals its own time without an enclosing timeout" do
			scheduler.with_timeout(1) do |timeout|
				expect(timeout.effective_time).to be == timeout.time
			end
		end

		it "is the inner time when the inner timeout is shorter" do
			scheduler.with_timeout(10) do |outer|
				scheduler.with_timeout(1) do |inner|
					expect(inner.effective_time).to be == inner.time
					expect(inner.effective_time).to be < outer.time
				end
			end
		end

		it "is clamped to the outer deadline when the inner timeout is longer" do
			scheduler.with_timeout(1) do |outer|
				scheduler.with_timeout(10) do |inner|
					expect(inner.effective_time).to be_within(0.001).of(outer.time)
					expect(inner.effective_time).to be < inner.time
				end
			end
		end
	end

	with "#effective? and #constrained?" do
		it "is effective without an enclosing timeout" do
			scheduler.with_timeout(1) do |timeout|
				expect(timeout).to be(:effective?)
				expect(timeout).not.to be(:constrained?)
			end
		end

		it "marks a shorter inner timeout as the binding one" do
			scheduler.with_timeout(10) do |outer|
				scheduler.with_timeout(1) do |inner|
					expect(inner).to be(:effective?)
					expect(inner).not.to be(:constrained?)
					expect(outer).not.to be(:constrained?)
				end
			end
		end

		it "marks a longer inner timeout as constrained by the outer deadline" do
			scheduler.with_timeout(1) do |outer|
				scheduler.with_timeout(10) do |inner|
					expect(inner).not.to be(:effective?)
					expect(inner).to be(:constrained?)
					expect(outer).to be(:effective?)
				end
			end
		end
	end

	with "#effective_duration" do
		it "reports the remaining effective budget" do
			scheduler.with_timeout(10) do |outer|
				scheduler.with_timeout(2) do |inner|
					# The inner timeout is the binding one:
					expect(inner.effective_time).to be == inner.time
					expect(inner.effective_duration).to (be > 0).and(be <= 2)
				end
			end
		end

		it "is bounded by the outer duration" do
			scheduler.with_timeout(1) do |outer|
				scheduler.with_timeout(10) do |inner|
					expect(inner.effective_duration).to (be > 0).and(be <= 1)
				end
			end
		end
	end

	with "nested cleanup" do
		it "removes only the inner layer when it exits early" do
			outer = nil
			inner = nil

			scheduler.with_timeout(10) do |timeout|
				outer = timeout
				scheduler.with_timeout(1) do |nested|
					inner = nested
					expect(Async::Scheduler.timeouts).to be == [outer, inner]
				end

				expect(Async::Scheduler.timeouts).to be == [outer]
				expect(inner).to be(:cancelled?)
				expect(outer).not.to be(:cancelled?)
			end

			expect(Async::Scheduler.timeouts).to be == []
		end

		it "keeps the outer deadline readable after the inner timer is cleaned up" do
			outer_time = nil

			scheduler.with_timeout(10) do |outer|
				outer_time = outer.time

				scheduler.with_timeout(1) do |inner|
					inner_time = inner.time
					expect(inner_time).to be < outer_time
				end

				# The outer timer and deadline are untouched:
				expect(outer.time).to be == outer_time
				expect(outer.deadline.time).to be_within(0.0001).of(outer_time)
			end
		end

		it "keeps the outer deadline after an inner timeout is rescued" do
			scheduler.with_timeout(10) do |outer|
				begin
					scheduler.with_timeout(0.001) do
						sleep(1)
					end
				rescue Async::TimeoutError
					# Expected: the inner operation ended early.
				end

				expect(Async::Scheduler.timeouts).to be == [outer]
				expect(outer).not.to be(:cancelled?)
			end
		end
	end

	with "timeout attribution" do
		it "attaches the firing timeout to the raised exception" do
			error = nil

			begin
				scheduler.with_timeout(0.001) do
					sleep(1)
				end
			rescue Async::TimeoutError => error
			end

			expect(error).to be_a(Async::TimeoutError)
			expect(error.timeout).to be_a(Async::Timeout)
		end

		it "attributes an inner timeout to the inner layer while the outer survives" do
			inner_timeout = nil
			outer_timeout = nil
			fired = nil

			scheduler.with_timeout(10) do |outer|
				outer_timeout = outer

				begin
					scheduler.with_timeout(0.001) do |inner|
						inner_timeout = inner
						sleep(1)
					end
				rescue Async::TimeoutError => error
					fired = error.timeout
				end

				# The inner layer ended the operation; the outer layer is still in effect:
				expect(fired).to be == inner_timeout
				expect(inner_timeout.parent).to be == outer_timeout
				expect(Async::Scheduler.timeouts).to be == [outer_timeout]
				expect(outer_timeout).not.to be(:cancelled?)
			end
		end

		it "still raises a plain business exception without a timeout attached" do
			business_error = Class.new(StandardError)

			expect do
				scheduler.with_timeout(1) do
					raise business_error, "boom"
				end
			end.to raise_exception(business_error, message: be == "boom")
		end

		it "works with a custom exception class" do
			custom_error = Class.new(RuntimeError)

			expect do
				scheduler.with_timeout(0.001, custom_error, "custom deadline") do
					sleep(1)
				end
			end.to raise_exception(custom_error, message: be == "custom deadline")
		end
	end

	with "#now" do
		it "can get the current time" do
			scheduler.with_timeout(1) do |timeout|
				expect(timeout.now).to be >= 0
				expect(timeout.now).to be <= timeout.time
			end
		end
	end

	with "#adjust" do
		it "can adjust the timeout" do
			scheduler.with_timeout(1) do |timeout|
				timeout.adjust(1)
				expect(timeout.duration).to (be > 1).and(be <= 2)
			end
		end
	end

	with "#duration=" do
		it "can set the timeout duration" do
			scheduler.with_timeout(1) do |timeout|
				timeout.duration = 2
				expect(timeout.duration).to (be > 1).and(be <= 2)
			end
		end

		it "can increase the timeout duration" do
			scheduler.with_timeout(1) do |timeout|
				timeout.duration += 2
				expect(timeout.duration).to (be > 2).and(be <= 3)
			end
		end
	end

	with "#time=" do
		it "can set the timeout time" do
			scheduler.with_timeout(1) do |timeout|
				timeout.time = timeout.time + 1
				expect(timeout.duration).to (be > 1).and(be < 2)
			end
		end
	end

	with "#cancel!" do
		it "can cancel the timeout" do
			scheduler.with_timeout(1) do |timeout|
				timeout.cancel!
				expect(timeout).to be(:cancelled?)
			end
		end

		it "can't reschedule a cancelled timeout" do
			scheduler.with_timeout(1) do |timeout|
				timeout.cancel!
				expect do
					timeout.adjust(1)
				end.to raise_exception(Async::Timeout::CancelledError)
			end
		end
	end
end
