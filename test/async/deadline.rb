# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025, by Shopify Inc.
# Copyright, 2025, by Samuel Williams.

require "async/deadline"
require "async/test/fake_clock"

describe Async::Deadline do
	let(:clock) {Async::FakeClock.new(100.0)}

	with ".start" do
		it "returns nil for nil timeout" do
			deadline = subject.start(nil)
			expect(deadline).to be_nil
		end

		it "returns Zero module for zero timeout" do
			deadline = subject.start(0)
			expect(deadline).to be == Async::Deadline::Zero
		end

		it "returns Zero module for negative timeout" do
			deadline = subject.start(-1)
			expect(deadline).to be == Async::Deadline::Zero
		end

		it "returns new instance for positive timeout" do
			deadline = subject.start(5.0)
			expect(deadline).to be_a(Async::Deadline)
		end

		it "uses the given clock" do
			deadline = subject.start(5.0, clock: clock)
			expect(deadline.clock).to be == clock
			expect(deadline.time).to be == 105.0
		end
	end

	describe Async::Deadline::Zero do
		let(:zero) {subject}

		it "is always expired" do
			expect(zero.expired?).to be == true
		end

		it "has zero remaining time" do
			expect(zero.remaining).to be == 0
		end

		it "reports an immediate expiry time" do
			expect(zero.time(clock)).to be == 100.0
		end

		it "defaults the expiry time to the real clock" do
			expect(zero.time).to be_a(Numeric)
		end

		it "has zero duration" do
			expect(zero.duration).to be == 0
		end
	end

	with "#time" do
		it "exposes the absolute deadline without mutating state" do
			deadline = subject.start(5.0, clock: clock)

			expect(deadline.time).to be == 105.0

			# Reading the time must not consume any remaining budget:
			clock.advance(2.0)
			expect(deadline.time).to be == 105.0
			expect(deadline.remaining).to be == 3.0
		end
	end

	with "#duration" do
		it "reports the original timeout even as time passes" do
			deadline = subject.start(7.0, clock: clock)

			expect(deadline.duration).to be == 7.0

			clock.advance(3.0)
			deadline.remaining

			expect(deadline.duration).to be == 7.0
		end
	end

	with "#remaining" do
		it "initializes with reasonable remaining time" do
			deadline = subject.new(5.0)
			expect(deadline.remaining).to be <= 5.0
		end

		it "decreases remaining time as time passes" do
			deadline = subject.new(1.0)

			# Get initial remaining time
			first_remaining = deadline.remaining
			expect(first_remaining).to be <= 1.0

			# Wait a tiny bit and check again
			sleep(0.001)

			second_remaining = deadline.remaining
			expect(second_remaining).to be < first_remaining
		end

		it "can return negative remaining time when expired" do
			# Create a deadline with very short timeout
			deadline = subject.new(0.001)  # 1 millisecond

			# Wait longer than the timeout
			sleep(0.002)

			remaining = deadline.remaining
			expect(remaining).to be < 0  # Should be negative
		end

		it "tracks remaining time against the injected clock" do
			deadline = subject.start(10.0, clock: clock)

			expect(deadline.remaining).to be == 10.0

			clock.advance(3.0)
			expect(deadline.remaining).to be == 7.0

			clock.advance(8.0)
			expect(deadline.remaining).to be == -1.0
		end
	end

	with "#expired?" do
		it "returns false for fresh deadline" do
			deadline = subject.new(2.0)
			expect(deadline.expired?).to be == false
		end

		it "returns true when deadline has expired" do
			# Create very short deadline that will expire quickly
			deadline = subject.new(0.001)

			# Wait for it to expire
			sleep(0.01)

			expect(deadline.expired?).to be == true
		end

		it "updates remaining time when checked" do
			deadline = subject.new(1.0)

			# Get initial remaining time
			first_remaining = deadline.remaining

			# Check if expired (which calls remaining internally)
			expired = deadline.expired?
			expect(expired).to be == false

			# Get remaining time again - should be less
			second_remaining = deadline.remaining
			expect(second_remaining).to be < first_remaining
		end

		it "is driven by the injected clock" do
			deadline = subject.start(2.0, clock: clock)

			clock.advance(1.5)
			expect(deadline.expired?).to be == false

			clock.advance(0.6)
			expect(deadline.expired?).to be == true
		end
	end

	it "handles sequential operations correctly" do
		deadline = subject.new(1.0)  # 1 second timeout

		# First check - should have close to full time
		first_remaining = deadline.remaining
		expect(first_remaining).to be <= 1.0
		expect(first_remaining).to be > 0.5  # Should still have most of the time

		# Short delay
		sleep(0.001)

		# Second check - should be less
		second_remaining = deadline.remaining
		expect(second_remaining).to be < first_remaining

		# Should still not be expired
		expect(deadline.expired?).to be == false
	end

	it "preserves a readable deadline across nested segments" do
		# The outer deadline created for a long operation:
		outer = subject.start(10.0, clock: clock)

		# A short inner sub-operation runs and exits early:
		clock.advance(3.0)
		inner = subject.start(2.0, clock: clock)

		expect(inner.time).to be == 105.0
		expect(outer.time).to be == 110.0

		# Inner expires and is discarded while the outer deadline survives:
		clock.advance(2.5)
		expect(inner.expired?).to be == true
		expect(outer.expired?).to be == false
		expect(outer.time).to be == 110.0
	end
end
