# frozen_string_literal: true

require "concurrent/executor/single_thread_executor"

RSpec.describe "Dry::Monads::TaskedResult mixin" do
  before do
    module Test
      class Operation
        include Dry::Monads[:result, :task_result]

        def call
          TaskResult { Success(fetch) }.
            bind { |value| TaskResult { Success(value * 2) } }.
            or { TaskResult { Success(:fallback) } }.
            to_task.
            value!
        end

        def fetch = 21
      end
    end
  end

  let(:operation) { Test::Operation.new }

  it "chains the two effects without nesting binds" do
    expect(operation.call).to eql(Dry::Monads::Success(42))
  end

  describe "TaskResult constructor" do
    include Dry::Monads::TaskedResult::Mixin

    it "wraps a block returning a Result" do
      tr = TaskResult { Dry::Monads::Success(1) }
      expect(tr.to_result).to eql(Dry::Monads::Success(1))
    end

    it "wraps an existing Task" do
      task = Dry::Monads::Task.pure(Dry::Monads::Failure(:no))
      tr = TaskResult(task)
      expect(tr.to_result).to eql(Dry::Monads::Failure(:no))
    end

    it "passes a TaskedResult through" do
      tr = Dry::Monads::TaskedResult.pure(1)
      expect(TaskResult(tr)).to be(tr)
    end
  end

  describe "cherry-picked loading" do
    it "registers the mixin under :task_result" do
      mod = Dry::Monads[:task_result]
      expect(mod.private_instance_methods).to include(:TaskResult)
    end

    it "does not pull other monad constructors" do
      mod = Dry::Monads[:task_result]
      expect(mod).not_to respond_to(:Some)
    end

    it "is included with the all-in-one Dry::Monads module" do
      klass = Class.new { include Dry::Monads }
      expect(klass.private_instance_methods).to include(:TaskResult)
    end
  end

  describe "executor preservation" do
    include Dry::Monads::TaskedResult::Mixin

    it "keeps running on the injected executor through the chain" do
      single = Concurrent::SingleThreadExecutor.new
      threads = Queue.new

      tr = TaskResult(Dry::Monads::Task[single] { Dry::Monads::Success(1) }).
        fmap { |v| threads << Thread.current; v + 1 }.
        bind { |v| TaskResult(Dry::Monads::Task[single] { Dry::Monads::Success(v + 1) }) }

      expect(tr.to_result).to eql(Dry::Monads::Success(3))
      expect(threads.pop).not_to be(Thread.main)
    end
  end
end

