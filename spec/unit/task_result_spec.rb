# frozen_string_literal: true

# rubocop:disable Style/RescueModifier

require "English"

RSpec.describe(Dry::Monads::TaskResult) do
  result = Dry::Monads::Result
  success = result::Success.method(:new)
  failure = result::Failure.method(:new)

  task = Dry::Monads::Task
  task_result = described_class

  def task_result(&block)
    described_class.new(&block)
  end

  def deferred(&block)
    lambda do
      expect(Thread.current).not_to be(Thread.main)
      block.call
    end
  end

  subject { task_result(&deferred { success[1] }) }

  describe ".new" do
    it "wraps an existing Task of Result" do
      t = task.pure(success[1])
      expect(task_result.new(t).to_task).to be(t)
    end

    it "builds a Task of Result from a block" do
      expect(task_result { success[1] }.value!).to eql(success[1])
    end
  end

  describe ".pure" do
    it "lifts a raw value into a success" do
      expect(task_result.pure(1).value!).to eql(success[1])
    end

    it "accepts a block too" do
      one = -> { 1 }
      expect(task_result.pure(&one).value!).to eql(success[one])
    end
  end

  describe ".failure" do
    it "builds a resolved business failure" do
      expect(task_result.failure(:nope).value!).to eql(failure[:nope])
    end
  end

  describe ".failed" do
    it "builds a task-level failure" do
      error = 1 / 0 rescue $ERROR_INFO
      expect { task_result.failed(error).value! }.to raise_error(error.class)
    end
  end

  describe "#fmap" do
    it "maps the business value" do
      expect(subject.fmap { _1 * 2 }.value!).to eql(success[2])
    end

    it "skips business failures" do
      called = false
      m = task_result.failure(:nope).fmap { called = true }
      m.wait

      expect(called).to be(false)
      expect(m.value!).to eql(failure[:nope])
    end

    it "does not rescue task-level exceptions" do
      m = task_result { 1 / 0 }.fmap { _1 }
      expect(m.wait.to_task.to_result.failure).to be_a(ZeroDivisionError)
    end
  end

  describe "#bind" do
    it "hands the business value to the callback" do
      chain = subject.bind { |v| task_result.pure(v * 2) }

      expect(chain.value!).to eql(success[2])
    end

    it "accepts a plain Task of Result from the callback" do
      chain = subject.bind { |v| task.pure(success[v * 2]) }

      expect(chain.value!).to eql(success[2])
    end

    it "accepts a plain Result from the callback" do
      chain = subject.bind { |v| success[v * 2] }

      expect(chain.value!).to eql(success[2])
    end

    it "reports a returned Failure through the business failure channel" do
      chain = subject.bind { failure[:rejected] }

      expect(chain.wait(1)).to be_complete
      expect(chain.value!).to eql(failure[:rejected])
    end

    it "short-circuits subsequent steps on business failure" do
      called = false
      chain = task_result.failure(:nope).bind { called = true }
      chain.wait

      expect(called).to be(false)
    end

    it "treats Success(nil) as a success, not as a missing result" do
      seen = :not_called
      chain = task_result.pure(nil).bind { |v| seen = v; task_result.pure(:next) }

      expect(chain.value!).to eql(success[:next])
      expect(seen).to be_nil
    end

    it "routes exceptions raised in the callback to the task channel" do
      error = nil
      chain = subject.bind { raise "boom" }
      begin
        chain.value!
      rescue RuntimeError => e
        error = e
      end

      expect(chain.wait(1)).to be_complete
      expect(error.message).to eql("boom")
    end

    it "does not hang waiters when the callback returns garbage" do
      chain = subject.bind { :not_a_monad }

      expect(chain.wait(1)).to be_complete
      expect(chain.to_task.to_result.failure).to be_a(TypeError)
    end
  end

  describe "#or" do
    it "recovers a business failure with another computation" do
      chain = task_result.failure(:nope).or { |err| task_result.pure("recovered #{err}") }

      expect(chain.value!).to eql(success["recovered nope"])
    end

    it "accepts a plain Result from the callback" do
      chain = task_result.failure(:nope).or { success[:recovered] }

      expect(chain.value!).to eql(success[:recovered])
    end

    it "is skipped on success" do
      called = false
      chain = subject.or { called = true }
      chain.wait

      expect(called).to be(false)
      expect(chain.value!).to eql(success[1])
    end

    it "does not recover task-level exceptions" do
      called = false
      chain = task_result { 1 / 0 }.or { called = true; task_result.pure(:x) }

      expect(chain.wait.to_task.to_result.failure).to be_a(ZeroDivisionError)
      expect(called).to be(false)
    end
  end

  describe "#or_fmap" do
    it "maps a business failure back to a success" do
      chain = task_result.failure(:nope).or_fmap { |err| "recovered #{err}" }

      expect(chain.value!).to eql(success["recovered nope"])
    end

    it "is skipped on success" do
      called = false
      chain = subject.or_fmap { called = true }
      chain.wait

      expect(called).to be(false)
    end

    it "does not recover task-level exceptions" do
      called = false
      chain = task_result { 1 / 0 }.or_fmap { called = true }

      expect(chain.wait.to_task.to_result.failure).to be_a(ZeroDivisionError)
      expect(called).to be(false)
    end
  end

  describe "#to_task" do
    it "losslessly returns the underlying Task of Result" do
      t = task.pure(success[1])
      expect(task_result.new(t).to_task).to be(t)
    end

    it "round-trips through the chain" do
      chain = subject.fmap { _1 * 2 }.to_task

      expect(chain).to be_a(task)
      expect(chain.to_result).to eql(success[success[2]])
    end
  end

  describe "executor handling" do
    let(:immediate) { Concurrent::ImmediateExecutor.new }

    it "runs the chain on the injected executor" do
      threads = []
      chain = task_result[immediate] { threads << Thread.current; success[1] }
                .bind { |v| threads << Thread.current; task_result[immediate] { success[v + 1] } }
                .fmap { |v| threads << Thread.current; v + 1 }
                .or_fmap { threads << Thread.current }
      chain.wait

      expect(threads).to all(be(Thread.main))
      expect(chain.value!).to eql(success[3])
    end

    it "introduces no executor hops beyond Task's own semantics" do
      on_main = ->(thread) { thread.equal?(Thread.main) }

      tr_threads = []
      task_result[immediate] { success[1] }
        .bind { |v| tr_threads << Thread.current; task_result.pure(v) }
        .fmap { |v| tr_threads << Thread.current; v }
        .wait

      task_threads = []
      task[immediate] { 1 }
        .bind { |v| task_threads << Thread.current; task.pure(v) }
        .fmap { |v| task_threads << Thread.current; v }
        .wait

      expect(tr_threads.map(&on_main)).to eql(task_threads.map(&on_main))
    end

    it "supports global pools" do
      m = task_result[:immediate, &-> { success[Thread.current] }]
      expect(m.value!).to eql(success[Thread.main])
    end
  end

  describe "#value!" do
    it "returns the Result" do
      expect(subject.value!).to eql(success[1])
    end

    it "raises on task-level failure" do
      expect { task_result { 1 / 0 }.value! }.to raise_error(ZeroDivisionError)
    end
  end

  describe "#to_s" do
    it "inspects pending" do
      expect(task_result { sleep 0.01 }.inspect).to eql("TaskResult(?)")
    end

    it "inspects complete" do
      m = task_result.pure(1).tap(&:wait)
      expect(m.inspect).to eql("TaskResult(value=Success(1))")
    end

    it "inspects failed" do
      err = 1 / 0 rescue $ERROR_INFO
      m = task_result { raise err }.tap(&:wait)
      expect(m.inspect).to eql("TaskResult(error=#{err.inspect})")
    end
  end

  describe "#==" do
    it "compares two complete task results" do
      expect(task_result.pure(1).wait).to eq(task_result.pure(1).wait)
      expect(task_result.pure(1).wait).not_to eq(task_result.pure(2).wait)
    end
  end

  describe "mixed chain of business rejections, async errors and recovery" do
    it "runs exactly the callbacks reachable through the channels" do
      calls = []
      boom = RuntimeError.new("boom")

      chain =
        task_result[:io] { calls << :fetch; success[2] }
          .bind { |v| calls << [:validate, v]; task_result.pure(v + 1) }
          .bind { |v| calls << [:persist, v]; failure[:conflict] }
          .bind { |v| calls << [:skipped_after_rejection, v]; task_result.pure(v) }
          .fmap { |v| calls << [:skipped_fmap, v] }
          .or_fmap { |err| calls << [:recovered, err]; 10 }
          .bind { |v| calls << [:notify, v]; task_result { raise boom } }
          .bind { |v| calls << [:skipped_after_throw, v]; task_result.pure(v) }
          .or_fmap { |err| calls << [:business_recovery_skipped, err] }
          .or { |err| calls << [:business_or_skipped, err] }

      outcome = chain.to_task.to_result

      expect(outcome).to be_a_failure
      expect(outcome.failure).to be(boom)
      expect(calls).to eql(
        [
          :fetch,
          [:validate, 2],
          [:persist, 3],
          [:recovered, :conflict],
          [:notify, 10]
        ]
      )
    end

    it "recovers a business rejection mid-chain and continues" do
      calls = []

      chain =
        task_result[:io] { calls << :fetch; success[1] }
          .bind { |v| calls << [:check, v]; failure[:denied] }
          .or { |err| calls << [:fallback, err]; task_result.pure(42) }
          .fmap { |v| calls << [:render, v]; v * 2 }

      expect(chain.value!).to eql(success[84])
      expect(calls).to eql([:fetch, [:check, 1], [:fallback, :denied], [:render, 42]])
    end
  end
end

# rubocop:enable Style/RescueModifier
