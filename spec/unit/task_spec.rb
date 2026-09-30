# frozen_string_literal: true

# rubocop:disable Style/RescueModifier

require "English"

RSpec.describe(Dry::Monads::Task) do
  result = Dry::Monads::Result
  success = result::Success.method(:new)
  failure = result::Failure.method(:new)

  maybe = Dry::Monads::Maybe
  some = maybe::Some.method(:new)
  unit = Dry::Monads::Unit

  task = described_class

  def task(&block)
    described_class.new(&block)
  end

  def deferred(&block)
    lambda do
      expect(Thread.current).not_to be(Thread.main)
      block.call
    end
  end

  subject do
    task(&deferred { 1 })
  end

  it_behaves_like "a monad"

  describe ".new" do
    it "delays the execution" do
      expect(subject.value!).to be 1
    end
  end

  describe "#fmap" do
    it "chains transformations" do
      chain = subject.fmap { _1 * 2 }

      expect(chain.value!).to be 2
    end

    it "runs a block only on success" do
      called = false
      t = task { 1 / 0 }.fmap { called = true }
      t.to_result

      expect(called).to be(false)
    end
  end

  describe "#bind" do
    it "combines tasks" do
      chain = subject.bind { |v| task { v * 2 } }

      expect(chain.value!).to be 2
    end
  end

  describe "#then" do
    it "is an alias of #bind", :suppress_deprecations do
      chain = subject.then { |v| task { v * 2 } }

      expect(chain.value!).to be 2
    end
  end

  describe "#bind_if" do
    it "calls the factory only after the condition task completes, exactly once" do
      condition_gate = Concurrent::Event.new
      condition_calls = 0
      factory_calls = 0

      condition = ->(v) {
        condition_calls += 1
        task {
          condition_gate.wait
          v > 0
        }
      }
      chain = task { 2 }.bind_if(condition, :skipped) { |v|
        factory_calls += 1
        task { v * 10 }
      }

      1000.times { condition_calls == 1 ? break : sleep(0.005) }

      expect(condition_calls).to be(1)
      expect(factory_calls).to be(0)
      expect(chain).not_to be_complete

      condition_gate.set

      expect(chain.value!).to be(20)
      expect(chain.value!).to be(20)
      expect(condition_calls).to be(1)
      expect(factory_calls).to be(1)
    end

    it "completes with the fallback value without calling the factory" do
      factory_calls = 0
      chain = task { 3 }.bind_if(-> v { task { v.even? } }, :skipped) { |_|
        factory_calls += 1
        task { :unreachable }
      }

      expect(chain.value!).to be(:skipped)
      expect(factory_calls).to be(0)
    end

    it "uses Unit as the default fallback" do
      chain = task { 3 }.bind_if(-> _ { task { false } }) { task { :unreachable } }

      expect(chain.value!).to be(unit)
    end

    it "rejects when the condition task fails without calling the factory" do
      factory_calls = 0
      chain = task { 1 }.bind_if(-> _ { described_class.failed(RuntimeError.new("closed")) }) { |_|
        factory_calls += 1
        task { :unreachable }
      }

      expect { chain.value! }.to raise_error(RuntimeError, "closed")
      expect(factory_calls).to be(0)
    end

    it "rejects without calling the condition or the factory when the upstream task fails" do
      condition_calls = 0
      factory_calls = 0
      chain = task { 1 / 0 }.bind_if(-> _ { condition_calls += 1; task { true } }) { |_|
        factory_calls += 1
        task { :unreachable }
      }

      expect { chain.value! }.to raise_error(ZeroDivisionError)
      expect(condition_calls).to be(0)
      expect(factory_calls).to be(0)
    end

    it "rejects when the factory raises" do
      chain = task { 1 }.bind_if(-> _ { task { true } }) { raise ArgumentError, "boom" }

      expect { chain.value! }.to raise_error(ArgumentError, "boom")
    end

    it "propagates rejection of the task returned by the factory" do
      chain = task { 1 }.bind_if(-> _ { task { true } }) { |_|
        task { 1 / 0 }
      }

      expect { chain.value! }.to raise_error(ZeroDivisionError)
    end

    it "rejects with a TypeError when the condition doesn't return a Task" do
      chain = task { 1 }.bind_if(-> _ { true }) { task { :unreachable } }

      expect { chain.value! }.to raise_error(TypeError, /condition must return a Task/)
    end

    it "rejects with a TypeError when the factory doesn't return a Task" do
      chain = task { 1 }.bind_if(-> _ { task { true } }) { :not_a_task }

      expect { chain.value! }.to raise_error(TypeError, /factory must return a Task/)
    end

    it "requires a factory block" do
      expect {
        task { 1 }.bind_if(-> _ { task { true } })
      }.to raise_error(ArgumentError, /factory block/)
    end
  end

  describe "#to_result" do
    it "transforms a successful execution to Result" do
      expect(subject.to_result).to eql(success[1])
    end

    it "transforms an unsuccessful result to a Failure" do
      error = task { 1 / 0 }.to_result
      expect(error).to be_a_failure
      expect(error.failure).to be_a(ZeroDivisionError)
    end

    it "tracks the caller" do
      error = task { 1 / 0 }.to_result
      expect(error.trace).to include("spec/unit/task_spec.rb")
    end
  end

  describe "#to_maybe" do
    it "transforms a successful execution to Some" do
      expect(subject.to_maybe).to eql(some[1])
    end

    it "transforms an unsuccessful result to None" do
      error = task { 1 / 0 }.to_maybe
      expect(error).to be_none
    end

    it "tracks the caller" do
      error = task { 1 / 0 }.to_maybe
      expect(error.trace).to include("spec/unit/task_spec.rb")
    end
  end

  describe "#value!" do
    it "unwraps the value" do
      expect(subject.value!).to be 1
    end

    it "raises an error on unsuccessful computation" do
      expect { task { 1 / 0 }.value! }.to raise_error(ZeroDivisionError)
    end
  end

  describe "#inspect" do
    it "inspects pending" do
      t = task { sleep 0.01 }
      expect(t.inspect).to eql("Task(?)")
    end

    it "inspects complete" do
      t = task { :something }.tap(&:value!)
      expect(t.inspect).to eql("Task(value=:something)")
    end

    it "inspects failed" do
      begin
        1 / 0
      rescue StandardError
        err = $ERROR_INFO
      end
      t = task { raise err }.tap(&:to_result)
      expect(t.inspect).to eql("Task(error=#{err.inspect})")
    end

    it "shows empty parens for unit" do
      expect(task { unit }.tap(&:value!).to_s).to eql("Task(value=())")
    end
  end

  describe "#to_s" do
    it "is an alias for inspect" do
      expect(subject.method(:to_s)).to eql(subject.method(:inspect))
    end
  end

  describe "#or" do
    it "runs a block on failure" do
      m = task { 1 / 0 }.or { task { :success } }
      expect(m.wait).to be == task { :success }.wait
    end

    it "ignores blocks on success" do
      m = subject.or { task { :success } }
      expect(m.wait).to be == task { 1 }.wait
    end
  end

  describe "#or_fmap" do
    it "runs a block on failure" do
      m = task { 1 / 0 }.or_fmap { :success }.to_result
      expect(m).to eql(success[:success])
    end
  end

  describe "#value_or" do
    specify "if success unwraps the value" do
      expect(subject.value_or { 2 }).to be(1)
    end

    specify "if failure calls the given block" do
      expect(task { 1 / 0 }.value_or { 2 }).to be(2)
    end
  end

  describe "#complete?" do
    it "checks whether the task is complete" do
      expect(task { sleep 0.01 }.wait).to be_complete
      expect(task { sleep 0.01 }).not_to be_complete
    end
  end

  describe "#wait" do
    it "waits for resolution" do
      expect(task { sleep 0.01 }.wait).to be_complete
      expect(task { sleep 0.01 }).not_to be_complete
    end

    it "accepts a timeout" do
      expect(task { sleep 10 }.wait(0.01)).not_to be_complete
    end
  end

  describe ".[]" do
    let(:immediate) { Concurrent::ImmediateExecutor.new }

    it "allows to inject an underlying executor" do
      expect(task[immediate, &-> { Thread.current }].to_result).to eql(success[Thread.main])
    end

    it "supports global pools" do
      expect(task[:immediate, &-> { Thread.current }].to_result).to eql(success[Thread.main])
      expect(task[:io, &-> { Thread.current }].to_result).not_to eql(success[Thread.main])
      expect(task[:fast, &-> { Thread.current }].to_result).not_to eql(success[Thread.main])
    end
  end

  describe ".pure" do
    it "creates a resolved task" do
      expect(task.pure(1)).to be == subject.wait
    end

    it "accepts a block too" do
      one = -> { 1 }
      expect(task.pure(&one)).to be == task { one }.wait
    end
  end

  describe ".failed" do
    it "creates a failed task" do
      error = 1 / 0 rescue $ERROR_INFO
      expect(task.failed(error).to_result).to eql(failure[error])
    end
  end

  describe "#apply" do
    let(:two) { task { 2 } }
    let(:three) { task { 3 } }

    it "applies arguments to the underlying callable" do
      lifted = task.pure(-> x { x * 2 })
      expect(lifted.apply(two).to_result).to eql(success[4])
    end

    it "curries the callable" do
      lifted = task.pure(-> x, y { x * y * 2 })
      expect(lifted.apply(two).apply(three).to_result).to eql(success[12])
    end

    it "can be used via pure with block" do
      lifted = task.pure { _1 * _2 * 2 }
      expect(lifted.apply(two).apply(three).to_result).to eql(success[12])
    end
  end
end

# rubocop:enable Style/RescueModifier
