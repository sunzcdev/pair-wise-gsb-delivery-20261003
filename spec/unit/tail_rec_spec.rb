# frozen_string_literal: true

RSpec.describe(Dry::Monads::TailRec) do
  continue = described_class::Continue.method(:new)
  done = described_class::Done.method(:new)

  describe "control markers" do
    it "provides Continue/Done constructors via Dry::Monads" do
      expect(Dry::Monads.Continue(1)).to eql(continue.(1))
      expect(Dry::Monads.Done(:ok)).to eql(done.(:ok))
    end

    it "includes Continue/Done constructors into monad mixins" do
      obj = Object.new.extend(Dry::Monads[:result, :maybe, :task])

      expect(obj.Continue(1)).to eql(continue.(1))
      expect(obj.Done(:ok)).to eql(done.(:ok))
    end

    it "has a readable inspection" do
      expect(continue.(1).inspect).to eql("Continue(1)")
      expect(done.(:ok).inspect).to eql("Done(:ok)")
    end
  end

  describe Dry::Monads::Result do
    include Dry::Monads[:result]

    def tail_rec(initial, &block)
      Dry::Monads::Result.tail_rec(initial, &block)
    end

    it "loops until Done and returns Success of the final value" do
      result = tail_rec(0) { |i| Success(i < 10 ? Continue(i + 1) : Done(i)) }

      expect(result).to eql(Success(10))
    end

    it "supports paginated-sync-style state threading" do
      pages = {nil => %w[a b], "b" => %w[c], "c" => nil}

      result = tail_rec(cursor: nil, acc: []) do |state|
        page = pages.fetch(state[:cursor])
        acc = state[:acc] + (page || [])

        Success(page ? Continue(cursor: page.last, acc: acc) : Done(acc))
      end

      expect(result).to eql(Success(%w[a b c]))
    end

    it "runs 100_000 iterations without growing the stack" do
      result = tail_rec(0) { |i| Success(i < 100_000 ? Continue(i + 1) : Done(i)) }

      expect(result).to eql(Success(100_000))
    end

    it "stops on Failure and returns it as is" do
      calls = 0
      failure = Failure(:boom)

      result = tail_rec(0) do |i|
        calls += 1
        i < 3 ? Success(Continue(i + 1)) : failure
      end

      expect(result).to be(failure)
      expect(calls).to be(4)
    end

    it "raises a type error when the step value is not a control marker" do
      expect { tail_rec(0) { Success(:not_a_marker) } }.to raise_error(
        Dry::Monads::InvalidTailRecStepError,
        /must return Continue\(state\).*Done\(value\).*:not_a_marker/
      )
    end

    it "raises a type error when the block does not return a Result" do
      expect { tail_rec(0) { :nope } }.to raise_error(
        TypeError, /expects the block to return a Result/
      )
    end

    it "is a TypeError for marker misuse" do
      expect(Dry::Monads::InvalidTailRecStepError.ancestors).to include(TypeError)
    end
  end

  describe Dry::Monads::Maybe do
    include Dry::Monads[:maybe]

    def tail_rec(initial, &block)
      Dry::Monads::Maybe.tail_rec(initial, &block)
    end

    it "loops until Done and returns Some of the final value" do
      result = tail_rec(0) { |i| Some(i < 10 ? Continue(i + 1) : Done(i)) }

      expect(result).to eql(Some(10))
    end

    it "runs 100_000 iterations without growing the stack" do
      result = tail_rec(0) { |i| Some(i < 100_000 ? Continue(i + 1) : Done(i)) }

      expect(result).to eql(Some(100_000))
    end

    it "stops on None and returns it as is" do
      calls = 0
      none = None()

      result = tail_rec(0) do |i|
        calls += 1
        i < 3 ? Some(Continue(i + 1)) : none
      end

      expect(result).to be(none)
      expect(calls).to be(4)
    end

    it "coerces Done(nil) to None" do
      expect(tail_rec(0) { Some(Done(nil)) }).to eql(None())
    end

    it "raises a type error when the step value is not a control marker" do
      expect { tail_rec(0) { Some(42) } }.to raise_error(
        Dry::Monads::InvalidTailRecStepError, /got: 42/
      )
    end

    it "raises a type error when the block does not return a Maybe" do
      expect { tail_rec(0) { nil } }.to raise_error(
        TypeError, /expects the block to return a Maybe/
      )
    end
  end

  describe Dry::Monads::Task do
    include Dry::Monads[:task]

    def tail_rec(initial, &block)
      Dry::Monads::Task.tail_rec(initial, &block)
    end

    it "loops until Done and resolves with the final value" do
      task = tail_rec(0) { |i| Task { i < 10 ? Continue(i + 1) : Done(i) } }

      expect(task.value!).to be(10)
    end

    it "runs 100_000 completed steps without growing the stack" do
      task = tail_rec(0) do |i|
        Dry::Monads::Task.pure(i < 100_000 ? Continue(i + 1) : Done(i))
      end

      expect(task.value!).to be(100_000)
    end

    it "propagates rejections and stops the loop" do
      error = StandardError.new("boom")
      calls = 0

      task = tail_rec(0) do |i|
        calls += 1
        if i < 3
          Dry::Monads::Task.pure(Continue(i + 1))
        else
          Dry::Monads::Task.failed(error)
        end
      end

      expect { task.value! }.to raise_error(error)
      expect(calls).to be(4)
    end

    it "propagates exceptions raised by async steps" do
      calls = 0

      task = tail_rec(0) do |i|
        calls += 1

        Dry::Monads::Task[:io] do
          i < 3 ? Continue(i + 1) : raise("async boom")
        end
      end

      expect { task.value! }.to raise_error(RuntimeError, "async boom")
      expect(calls).to be(4)
    end

    it "does not block the calling thread on pending steps" do
      release = Queue.new

      task = tail_rec(0) do |i|
        Task do
          release.pop
          Done(i)
        end
      end

      # tail_rec returned even though the step is still blocked,
      # so the loop is advanced asynchronously
      expect(task).not_to be_complete

      release << true
      expect(task.value!).to be(0)
    end

    it "does not run async steps on the calling thread" do
      calling_thread = Thread.current
      threads = Queue.new

      task = tail_rec(0) do |i|
        Dry::Monads::Task[:io] do
          threads << Thread.current
          i < 5 ? Continue(i + 1) : Done(:done)
        end
      end

      expect(task.value!).to be(:done)

      ran_on = 6.times.map { threads.pop }
      expect(ran_on).to all(satisfy { |t| !t.equal?(calling_thread) })
    end

    it "raises a type error when the block does not return a Task" do
      expect { tail_rec(0) { :nope } }.to raise_error(
        TypeError, /expects the block to return a Task/
      )
    end

    it "raises a type error when a completed step is not a control marker" do
      expect { tail_rec(0) { Dry::Monads::Task.pure(:nope) } }.to raise_error(
        Dry::Monads::InvalidTailRecStepError, /got: :nope/
      )
    end

    it "surfaces marker misuse in async steps as a rejection" do
      task = tail_rec(0) { Dry::Monads::Task[:io] { :nope } }

      expect { task.value! }.to raise_error(Dry::Monads::InvalidTailRecStepError)
    end
  end
end
