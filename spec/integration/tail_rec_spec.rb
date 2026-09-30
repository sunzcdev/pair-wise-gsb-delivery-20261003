# frozen_string_literal: true

RSpec.describe(Dry::Monads::TailRec) do
  result = Dry::Monads::Result
  maybe = Dry::Monads::Maybe
  task = Dry::Monads::Task
  tail_rec = described_class
  unit = Dry::Monads::Unit

  include Dry::Monads::Result::Mixin
  include Dry::Monads::Maybe::Mixin
  include Dry::Monads::Task::Mixin
  include Dry::Monads::TailRec::Mixin

  describe "markers" do
    example "Continue carries the next state" do
      expect(Continue(1)).to eql(tail_rec::Continue.new(1))
      expect(Continue(1).value!).to be(1)
    end

    example "Done carries the final value" do
      expect(Done(:ok)).to eql(tail_rec::Done.new(:ok))
      expect(Done(:ok).value!).to eql(:ok)
    end

    example "markers default to Unit" do
      expect(Continue().value!).to be(unit)
      expect(Done().value!).to be(unit)
    end

    example "markers compare by value" do
      expect(Continue(a: 1)).to eql(Continue(a: 1))
      expect(Continue(a: 1)).not_to eql(Continue(a: 2))
      expect(Done(1)).not_to eql(Continue(1))
    end

    example "markers support pattern matching" do
      case Continue(42)
      in Dry::Monads::TailRec::Continue(Integer => n)
        expect(n).to be(42)
      end

      case Done([1, 2])
      in Dry::Monads::TailRec::Done([a, b])
        expect([a, b]).to eql([1, 2])
      end
    end

    example "markers are available from Dry::Monads" do
      expect(Dry::Monads.Continue(1)).to be_a(tail_rec::Continue)
      expect(Dry::Monads.Done(1)).to be_a(tail_rec::Done)
    end
  end

  describe "Result.tail_rec" do
    it "loops until Done" do
      value = result.tail_rec(0) do |n|
        n == 5 ? Success(Done(:done)) : Success(Continue(n + 1))
      end

      expect(value).to eql(Success(:done))
    end

    it "passes the carried state to the next step" do
      seen = []

      result.tail_rec(10) do |n|
        seen << n
        n.zero? ? Success(Done(n)) : Success(Continue(n - 1))
      end

      expect(seen).to eql((0..10).to_a.reverse)
    end

    it "threads accumulator state" do
      value = result.tail_rec(page: 1, items: []) do |state|
        if state[:page] > 3
          Success(Done(state[:items]))
        else
          Success(Continue(page: state[:page] + 1, items: state[:items] + [state[:page]]))
        end
      end

      expect(value).to eql(Success([1, 2, 3]))
    end

    it "returns Failure unchanged without running further steps" do
      calls = 0

      value = result.tail_rec(0) do |n|
        calls += 1
        n == 2 ? Failure(:oops) : Success(Continue(n + 1))
      end

      expect(value).to eql(Failure(:oops))
      expect(calls).to be(3)
    end

    it "starts from Failure without calling the block" do
      expect {
        value = Failure(:nope).tail_rec { raise "must not run" }
        expect(value).to eql(Failure(:nope))
      }.not_to raise_error
    end
  end

  describe "Maybe.tail_rec" do
    it "loops until Done" do
      value = maybe.tail_rec(0) do |n|
        n == 4 ? Some(Done(:done)) : Some(Continue(n + 1))
      end

      expect(value).to eql(Some(:done))
    end

    it "stops on None" do
      calls = 0

      value = maybe.tail_rec(0) do |n|
        calls += 1
        n == 2 ? None() : Some(Continue(n + 1))
      end

      expect(value).to be_none
      expect(calls).to be(3)
    end

    it "starts from None without calling the block" do
      expect {
        value = None().tail_rec { raise "must not run" }
        expect(value).to be_none
      }.not_to raise_error
    end

    it "works from an instance of Some" do
      value = Some(100).tail_rec do |n|
        n <= 97 ? Some(Done(n)) : Some(Continue(n - 1))
      end

      expect(value).to eql(Some(97))
    end
  end

  describe "marker misuse" do
    it "raises MarkerError (a TypeError) on a bare value inside Result" do
      expect {
        result.tail_rec(0) { Success(42) }
      }.to raise_error(
        tail_rec::MarkerError,
        /Continue\(state\) or Done\(value\).*42/
      )
    end

    it "raises MarkerError on a bare value inside Maybe" do
      expect {
        maybe.tail_rec(0) { Some(:nope) }
      }.to raise_error(tail_rec::MarkerError)
    end

    it "is a TypeError" do
      expect(tail_rec::MarkerError).to be < TypeError
    end

    it "does not turn arbitrary block errors into marker errors" do
      expect {
        result.tail_rec(0) { raise "unrelated error" }
      }.to raise_error(RuntimeError, "unrelated error")
    end
  end

  describe "stack stability" do
    iterations = 100_000

    it "keeps the Result call stack flat over #{iterations} iterations" do
      first_depth = last_depth = nil

      value = result.tail_rec(0) do |n|
        first_depth ||= caller.length
        last_depth = caller.length
        n >= iterations ? Success(Done(n)) : Success(Continue(n + 1))
      end

      expect(value).to eql(Success(iterations))
      expect(last_depth).to be <= first_depth
    end

    it "keeps the Maybe call stack flat over #{iterations} iterations" do
      first_depth = last_depth = nil

      value = maybe.tail_rec(0) do |n|
        first_depth ||= caller.length
        last_depth = caller.length
        n >= iterations ? Some(Done(n)) : Some(Continue(n + 1))
      end

      expect(value).to eql(Some(iterations))
      expect(last_depth).to be <= first_depth
    end
  end

  describe "Task.tail_rec" do
    it "loops until Done asynchronously" do
      async = task.tail_rec(0) do |n|
        n == 5 ? task.pure(Done(:done)) : task.pure(Continue(n + 1))
      end

      expect(async.wait.value!).to eql(:done)
    end

    it "does not call steps on the calling thread" do
      calling_thread = Thread.current
      step_threads = []

      async = task.tail_rec(0) do |n|
        step_threads << Thread.current
        n == 3 ? task.pure(Done(n)) : task.pure(Continue(n + 1))
      end

      async.wait

      expect(step_threads.size).to be(4)
      expect(step_threads).not_to include(calling_thread)
    end

    it "returns before any iteration is forced" do
      async = task.tail_rec(0) do
        Task { sleep 0.2 }.fmap { Done(:late) }
      end

      expect(async).not_to be_complete
      expect(async.wait.value!).to eql(:late)
    end

    it "never blocks the calling thread waiting for async steps" do
      start = Time.now

      async = task.tail_rec(0) do |page|
        Task { sleep 0.05 }.fmap do
          page >= 4 ? Done(page + 1) : Continue(page + 1)
        end
      end

      returned_after = Time.now - start

      expect(returned_after).to be < 0.02
      expect(async.wait.value!).to be(5)
    end

    it "propagates rejected tasks instead of swallowing them" do
      async = task.tail_rec(0) do |n|
        if n == 3
          task.failed(RuntimeError.new("boom"))
        else
          task.pure(Continue(n + 1))
        end
      end

      value = async.to_result

      expect(value).to be_failure
      expect(value.failure).to be_a(RuntimeError)
      expect(value.failure.message).to eql("boom")
    end

    it "propagates exceptions raised inside steps" do
      async = task.tail_rec(0) do |n|
        raise "kaboom" if n == 2
        task.pure(Continue(n + 1))
      end

      value = async.to_result

      expect(value).to be_failure
      expect(value.failure.message).to eql("kaboom")
    end

    it "rejects with MarkerError when a step resolves to a bare value" do
      async = task.tail_rec(0) { task.pure(123) }

      value = async.to_result

      expect(value).to be_failure
      expect(value.failure).to be_a(tail_rec::MarkerError)
      expect(value.failure.message).to include("123")
    end

    it "rejects with MarkerError when a block returns a non-Task" do
      async = task.tail_rec(0) { 456 }

      value = async.to_result

      expect(value).to be_failure
      expect(value.failure).to be_a(tail_rec::MarkerError)
    end

    it "short-circuits when the initial task is rejected" do
      async = task.failed(RuntimeError.new("initial")).tail_rec { raise "never" }

      expect(async.to_result.failure.message).to eql("initial")
    end

    it "works starting from an existing task" do
      async = task.pure(10).tail_rec do |n|
        n.zero? ? task.pure(Done(n)) : task.pure(Continue(n - 1))
      end

      expect(async.wait.value!).to be(0)
    end

    it "parks on genuinely pending steps and resumes correctly" do
      async = task.tail_rec(0) do |n|
        if n < 3
          Task { sleep 0.01; n + 1 }.fmap { Continue(_1) }
        else
          task.pure(Done(n))
        end
      end

      expect(async.wait.value!).to be(3)
    end

    it "keeps the call stack flat over 100 000 IO-free steps" do
      iterations = 100_000
      depth_lock = Mutex.new
      max_depth = 0

      async = task.tail_rec(0) do |n|
        depth = caller.length
        depth_lock.synchronize { max_depth = [max_depth, depth].max }
        n >= iterations ? task.pure(Done(n)) : task.pure(Continue(n + 1))
      end

      expect(async.wait.value!).to be(iterations)
      # A single executor job's call chain; must not grow with iterations.
      expect(max_depth).to be < 50
    end
  end
end
