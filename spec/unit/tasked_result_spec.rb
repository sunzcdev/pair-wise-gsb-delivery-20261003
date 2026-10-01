# frozen_string_literal: true

RSpec.describe(Dry::Monads::TaskedResult) do
  result = Dry::Monads::Result
  success = result::Success.method(:new)
  failure = result::Failure.method(:new)

  described = described_class
  task = Dry::Monads::Task

  def task_result(&block)
    described_class.new(&block)
  end

  subject { task_result { success[1] } }

  it_behaves_like "a monad"

  describe ".new" do
    it "runs the block asynchronously and keeps a Success" do
      tr = task_result { success[1] }
      expect(tr.wait).to be_complete
      expect(tr.to_result).to eql(success[1])
    end

    it "wraps a plain block value into Success" do
      tr = task_result { 1 }
      expect(tr.to_result).to eql(success[1])
    end

    it "routes an exception raised in the block to the task channel" do
      tr = task_result { raise ZeroDivisionError, "boom" }
      tr.wait
      expect { tr.value! }.to raise_error(ZeroDivisionError)
    end

    it "runs the block on the given executor" do
      immediate = Concurrent::ImmediateExecutor.new
      tr = described_class.new(immediate) { success[Thread.current] }
      expect(tr.to_result).to eql(success[Thread.main])
    end

    it "accepts a Result value" do
      tr = described_class.new(success[1])
      expect(tr.to_result).to eql(success[1])
    end

    it "accepts a Task fulfilled with a Result" do
      tr = described_class.new(task.pure(success[1]))
      expect(tr.to_result).to eql(success[1])
    end

    it "rejects anything else" do
      expect { described_class.new(1) }.to raise_error(TypeError, /Task or a Result/)
    end
  end

  describe ".pure / .success" do
    it "builds a completed Success" do
      expect(described.pure(1).to_result).to eql(success[1])
    end

    it "accepts a block" do
      expect(described.pure { 1 }.to_result).to eql(success[1])
    end

    it "is aliased as .success" do
      expect(described.success(1).to_result).to eql(success[1])
    end
  end

  describe ".failure" do
    it "builds a completed Failure" do
      expect(described.failure(:no).to_result).to eql(failure[:no])
    end
  end

  describe ".failed" do
    it "builds a rejected task" do
      error = RuntimeError.new("boom")
      tr = described.failed(error)
      expect { tr.to_result }.to raise_error(error)
    end
  end

  describe ".from_task" do
    it "wraps a Task" do
      tr = described.from_task(task.pure(success[1]))
      expect(tr.to_result).to eql(success[1])
    end

    it "requires a Task" do
      expect { described.from_task(1) }.to raise_error(TypeError, /expected Task/)
    end
  end

  describe "#bind" do
    it "unwraps the business value" do
      tr = subject.bind { |v| described.pure(v * 2) }
      expect(tr.to_result).to eql(success[2])
    end

    it "accepts a plain Result returned by the callback" do
      tr = subject.bind { |v| success[v * 2] }
      expect(tr.to_result).to eql(success[2])
    end

    it "accepts a Task returned by the callback" do
      tr = subject.bind { |v| task.pure(success[v * 2]) }
      expect(tr.to_result).to eql(success[2])
    end

    it "accepts a plain value and wraps it in Success" do
      tr = subject.bind { |v| v * 2 }
      expect(tr.to_result).to eql(success[2])
    end

    it "delivers nil from Success(nil) without confusing it with no result" do
      received = nil
      tr = task_result { success[nil] }.bind { |v| received = v; described.pure(:ok) }
      tr.wait
      expect(received).to be(nil)
      expect(tr.to_result).to eql(success[:ok])
    end

    it "skips the callback on a business Failure" do
      called = false
      tr = described.failure(:no).bind { called = true; described.pure(1) }
      tr.wait
      expect(called).to be(false)
      expect(tr.to_result).to eql(failure[:no])
    end

    it "skips the callback when the task was rejected" do
      called = false
      tr = described.failed(RuntimeError.new).bind {
        called = true; described.pure(1)
      }
      tr.wait
      expect(called).to be(false)
    end

    it "routes a Failure returned by the callback through the failure channel" do
      tr = subject.bind { failure[:rejected_by_callback] }
      expect(tr.wait).to be_complete
      expect(tr.to_result).to eql(failure[:rejected_by_callback])
    end

    it "routes a rejected Task returned by the callback to the exception channel" do
      error = RuntimeError.new("inner")
      tr = subject.bind { task.failed(error) }
      tr.wait
      expect { tr.to_result }.to raise_error(error)
    end

    it "routes an exception raised in the callback to the exception channel" do
      tr = subject.bind { raise RuntimeError, "boom" }
      tr.wait
      expect { tr.to_result }.to raise_error(RuntimeError, "boom")
    end
  end

  describe "#fmap" do
    it "transforms the business value" do
      tr = subject.fmap { _1 * 2 }
      expect(tr.to_result).to eql(success[2])
    end

    it "wraps nil as Success(nil)" do
      tr = subject.fmap { nil }
      expect(tr.to_result).to eql(success[nil])
      expect(tr.to_result).to be_success
    end

    it "does not run on Failure" do
      called = false
      tr = described.failure(:no).fmap { called = true }
      tr.wait
      expect(called).to be(false)
    end

    it "honors a Failure returned by the callback" do
      tr = subject.fmap { failure[:no] }
      expect(tr.to_result).to eql(failure[:no])
    end

    it "routes a raised error to the exception channel" do
      tr = subject.fmap { raise "boom" }
      expect { tr.wait.to_result }.to raise_error("boom")
    end
  end

  describe "#or" do
    it "recovers a business Failure" do
      tr = described.failure(:no).or { described.pure(:recovered) }
      expect(tr.to_result).to eql(success[:recovered])
    end

    it "receives the failure value" do
      tr = described.failure(:no).or { |error| described.failure([error, :again]) }
      expect(tr.to_result).to eql(failure[[:no, :again]])
    end

    it "does not run on Success" do
      called = false
      tr = subject.or { called = true; described.pure(2) }
      tr.wait
      expect(called).to be(false)
      expect(tr.to_result).to eql(success[1])
    end

    it "does not run when the task was rejected" do
      called = false
      error = RuntimeError.new("boom")
      tr = described.failed(error).or { called = true; described.pure(2) }
      tr.wait
      expect(called).to be(false)
      expect { tr.value! }.to raise_error(error)
    end

    it "routes a Failure returned by the recovery through the failure channel" do
      tr = described.failure(:no).or { failure[:still_no] }
      expect(tr.to_result).to eql(failure[:still_no])
    end

    it "routes an exception raised in the recovery to the exception channel" do
      tr = described.failure(:no).or { raise "recovery failed" }
      expect { tr.wait.to_result }.to raise_error("recovery failed")
    end
  end

  describe "#or_fmap" do
    it "recovers with the block value wrapped in Success" do
      tr = described.failure(:no).or_fmap { :recovered }
      expect(tr.to_result).to eql(success[:recovered])
    end

    it "does not run on Success" do
      tr = subject.or_fmap { 2 }
      expect(tr.to_result).to eql(success[1])
    end
  end

  describe "#or_exception" do
    it "recovers an asynchronously raised error" do
      tr = task_result { raise "boom" }.or_exception { described.pure(:recovered) }
      expect(tr.to_result).to eql(success[:recovered])
    end

    it "recovers an error raised by a bind callback" do
      tr = subject.bind { raise "boom" }.or_exception {
        described.pure(:recovered)
      }
      expect(tr.to_result).to eql(success[:recovered])
    end

    it "receives the exception" do
      tr = described.failed(RuntimeError.new("boom")).or_exception { |error|
        described.failure(error.message)
      }
      expect(tr.to_result).to eql(failure["boom"])
    end

    it "does not run on a business Failure" do
      called = false
      tr = described.failure(:no).or_exception { called = true; described.pure(1) }
      tr.wait
      expect(called).to be(false)
      expect(tr.to_result).to eql(failure[:no])
    end

    it "does not run on Success" do
      called = false
      tr = subject.or_exception { called = true; described.pure(1) }
      tr.wait
      expect(called).to be(false)
    end

    it "routes an exception raised in the recovery to the exception channel" do
      tr = described.failed(RuntimeError.new("a")).or_exception { raise "b" }
      expect { tr.wait.to_result }.to raise_error("b")
    end
  end

  describe "the acceptance chain" do
    # Business rejection, an async error and a recovery all in one chain.
    # We assert which callbacks actually execute.
    it "keeps the three channels separate and never hangs" do
      log = []

      tr = described.
        pure(1).
        fmap { |v| log << [:fmap1, v]; v + 1 }.
        bind { |v|
          log << [:bind1, v]
          # business rejection: delivered via the failure channel
          failure[:not_found]
        }.
        fmap { |v| log << [:fmap2_skipped, v]; v }.
        bind { |v| log << [:bind2_skipped, v]; described.pure(v) }.
        or { |error|
          log << [:or1, error]
          # the recovery itself blows up asynchronously
          raise RuntimeError, "recovery exploded"
        }.
        fmap { |v| log << [:fmap3_skipped, v]; v }.
        bind { |v| log << [:bind3_skipped, v]; described.pure(v) }.
        or { |error| log << [:or2_skipped, error]; described.pure(9) }.
        or_exception { |exception|
          log << [:or_exception, exception.message]
          described.pure(:saved)
        }.
        or { |_error| raise "must not run after recovery" }.
        fmap { |v| log << [:fmap4, v]; v }

      expect(tr.wait(10)).to be_complete
      expect(tr.to_result).to eql(success[:saved])
      expect(log).to eql([
        [:fmap1, 1],
        [:bind1, 2],
        [:or1, :not_found],
        [:or_exception, "recovery exploded"],
        [:fmap4, :saved]
      ])
    end

    it "stops success callbacks after a business Failure and resumes after #or" do
      log = []
      tr = described.pure(1).
        fmap { |v| log << :a; v }.
        bind { |_v| failure[:no] }.
        fmap { |v| log << :b; v }.
        or { |error| log << [:c, error]; described.pure(2) }.
        fmap { |v| log << [:d, v]; v }

      tr.wait
      expect(tr.to_result).to eql(success[2])
      expect(log).to eql([:a, [:c, :no], [:d, 2]])
    end
  end

  describe "#to_task" do
    it "returns a plain Task" do
      expect(subject.to_task).to be_a(task)
    end

    it "is lossless when the TaskedResult was built from a Task" do
      plain = task.pure(success[1])
      tr = described.from_task(plain)
      expect(tr.to_task).to be(plain)
    end

    it "does not change the executor already in use" do
      immediate = Concurrent::ImmediateExecutor.new
      plain = task[immediate] { success[Thread.current] }
      tr = described.from_task(plain)

      executor = tr.to_task.send(:promise).instance_variable_get(:@executor)
      expect(executor).to be(immediate)
      expect(tr.to_result).to eql(success[Thread.main])
    end

    it "can be consumed as a normal Task<Result> with two-level bind" do
      plain = subject.bind { |v| described.pure(v * 2) }.to_task
      unfolded = plain.bind { |result|
        task.pure(result.bind { |v| Dry::Monads::Success(v + 1) })
      }
      expect(unfolded.value!).to eql(success[3])
    end
  end

  describe "#wait / #complete?" do
    it "blocks until complete and returns self" do
      tr = task_result { sleep 0.01; success[1] }
      expect(tr).not_to be_complete
      expect(tr.wait).to be(tr)
      expect(tr).to be_complete
    end
  end

  describe "#inspect" do
    it "inspects a Success" do
      expect(described.pure(1).inspect).to eql("TaskedResult(Success(1))")
    end

    it "inspects a Failure" do
      expect(described.failure(:no).inspect).to eql("TaskedResult(Failure(:no))")
    end

    it "inspects a rejected task" do
      error = RuntimeError.new("boom")
      expect(described.failed(error).inspect).to(
        eql("TaskedResult(error=#{error.inspect})")
      )
    end

    it "inspects a pending task without blocking" do
      tr = task_result { sleep 1 }
      expect(tr.inspect).to eql("TaskedResult(?)")
    end
  end
end


