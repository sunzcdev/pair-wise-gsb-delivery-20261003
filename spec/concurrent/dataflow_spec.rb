require 'concurrent/dataflow'
require 'concurrent/executor/simple_executor_service'
require 'concurrent/executor/single_thread_executor'
require 'concurrent/atomic/count_down_latch'

module Concurrent

  RSpec.describe 'dataflow' do

    let(:executor) { ImmediateExecutor.new }
    let(:root_executor) { SimpleExecutorService.new }

    # Builds an executor that counts how many tasks a dataflow node actually
    # schedules, so tests can assert a node is never scheduled more than once.
    def counting_executor
      Class.new do
        include Concurrent::SerialExecutorService

        attr_reader :posts

        def initialize
          @lock     = Mutex.new
          @posts    = 0
          @delegate = Concurrent::ImmediateExecutor.new
        end

        def post(*args, &task)
          @lock.synchronize { @posts += 1 }
          @delegate.post(*args, &task)
        end

        def running?
          @delegate.running?
        end

        def shutdown?
          @delegate.shutdown?
        end

        def shuttingdown?
          @delegate.shuttingdown?
        end

        def shutdown
          @delegate.shutdown
        end
        alias_method :kill, :shutdown

        def wait_for_termination(timeout = nil)
          @delegate.wait_for_termination(timeout)
        end
      end.new
    end

    it 'raises an exception when no block given' do
      expect { Concurrent::dataflow }.to raise_error(ArgumentError)
      expect { Concurrent::dataflow_with(root_executor) }.to raise_error(ArgumentError)
    end

    specify '#dataflow uses the global fast executor' do
      input = Future.execute{0}
      expect(Concurrent).to receive(:dataflow_with).once.
        with(Concurrent.global_io_executor, input)
      Concurrent::dataflow(input){0}
    end

    specify '#dataflow_with uses the given executor' do
      skip('flaky on truffleruby') if Concurrent.on_truffleruby?

      input = Future.execute{0}
      result = Future.new{0}

      expect(Future).to receive(:new).with(executor: root_executor).and_return(result)
      Concurrent::dataflow_with(root_executor, input){0}
    end

    specify '#dataflow_with raises an exception when no executor given' do
      expect {
        Concurrent::dataflow_with(nil){ nil }
      }.to raise_error(ArgumentError)
    end

    it 'accepts zero or more dependencies' do
      Concurrent::dataflow(){0}
      Concurrent::dataflow(Future.execute{0}){0}
      Concurrent::dataflow(Future.execute{0}, Future.execute{0}){0}

      Concurrent::dataflow_with(root_executor, ){0}
      Concurrent::dataflow_with(root_executor, Future.execute{0}){0}
      Concurrent::dataflow_with(root_executor, Future.execute{0}, Future.execute{0}){0}
    end

    it 'accepts uncompleted dependencies' do
      d = Future.new(executor: executor){0}
      Concurrent::dataflow(d){0}
      d.execute

      d = Future.new(executor: executor){0}
      Concurrent::dataflow_with(root_executor, d){0}
      d.execute
    end

    it 'accepts completed dependencies' do
      d = Future.new(executor: executor){0}
      d.execute
      Concurrent::dataflow(d){0}

      d = Future.new(executor: executor){0}
      d.execute
      Concurrent::dataflow_with(root_executor, d){0}
    end

    it 'raises an exception if any dependencies are not IVars' do
      expect { Concurrent::dataflow(nil) }.to raise_error(ArgumentError)
      expect { Concurrent::dataflow(Future.execute{0}, nil) }.to raise_error(ArgumentError)
      expect { Concurrent::dataflow(nil, Future.execute{0}) }.to raise_error(ArgumentError)

      expect { Concurrent::dataflow_with(root_executor, nil) }.to raise_error(ArgumentError)
      expect { Concurrent::dataflow_with(root_executor, Future.execute{0}, nil) }.to raise_error(ArgumentError)
      expect { Concurrent::dataflow_with(root_executor, nil, Future.execute{0}) }.to raise_error(ArgumentError)
    end

    it 'doesn\'t raise exceptions from dependencies, unless called with !' do
      d1 = Concurrent::dataflow { raise 'd1 error' }
      d2 = Concurrent::dataflow { raise 'd2 error' }
      f = Concurrent::dataflow!(d1, d2) { |d1v, d2v| [d1v, d2v] }
      expect { f.value! }.to raise_error(Concurrent::DataflowDependencyError)

      d1 = Concurrent::dataflow { raise 'd1 error' }
      d2 = Concurrent::dataflow { raise 'd2 error' }
      f = Concurrent::dataflow(d1, d2) { |d1v, d2v| [d1v, d2v] }
      expect { f.value! }.to_not raise_error
    end

    it 'returns a Future' do
      expect(Concurrent::dataflow{0}).to be_a(Future)
      expect(Concurrent::dataflow{0}).to be_a(Future)
    end

    context 'does not schedule the Future' do

      specify 'if no dependencies are completed' do
        d = Future.new(executor: executor){0}
        f = Concurrent::dataflow(d){0}
        expect(f).to be_unscheduled
        d.execute

        d = Future.new(executor: executor){0}
        f = Concurrent::dataflow_with(root_executor, d){0}
        expect(f).to be_unscheduled
        d.execute
      end

      specify 'if one dependency of two is completed' do
        d1 = Future.new(executor: executor){0}
        d2 = Future.new(executor: executor){0}
        f = Concurrent::dataflow(d1, d2){0}
        d1.execute
        expect(f).to be_unscheduled
        d2.execute

        d1 = Future.new(executor: executor){0}
        d2 = Future.new(executor: executor){0}
        f = Concurrent::dataflow_with(root_executor, d1, d2){0}
        d1.execute
        expect(f).to be_unscheduled
        d2.execute
      end
    end

    context 'schedules the Future when all dependencies are available' do

      specify 'if there is just one' do
        d = Future.new(executor: executor){0}
        f = Concurrent::dataflow(d){0}
        d.execute
        expect(f.value).to eq 0

        d = Future.new(executor: executor){0}
        f = Concurrent::dataflow_with(root_executor, d){0}
        d.execute
        expect(f.value).to eq 0
      end

      specify 'if there is more than one' do
        d1 = Future.new(executor: executor){0}
        d2 = Future.new(executor: executor){0}
        f = Concurrent::dataflow(d1, d2){0}
        d1.execute
        d2.execute
        expect(f.value).to eq 0

        d1 = Future.new(executor: executor){0}
        d2 = Future.new(executor: executor){0}
        f = Concurrent::dataflow_with(root_executor, d1, d2){0}
        d1.execute
        d2.execute
        expect(f.value).to eq 0
      end
    end

    context 'counts already executed dependencies' do

      specify 'if there is just one' do
        d = Future.new(executor: executor){0}
        d.execute
        f = Concurrent::dataflow(d){0}
        expect(f.value).to eq 0

        d = Future.new(executor: executor){0}
        d.execute
        f = Concurrent::dataflow_with(root_executor, d){0}
        expect(f.value).to eq 0
      end

      specify 'if there is more than one' do
        d1 = Future.new(executor: executor){0}
        d2 = Future.new(executor: executor){0}
        d1.execute
        d2.execute
        f = Concurrent::dataflow(d1, d2){0}
        expect(f.value).to eq 0

        d1 = Future.new(executor: executor){0}
        d2 = Future.new(executor: executor){0}
        d1.execute
        d2.execute
        f = Concurrent::dataflow_with(root_executor, d1, d2){0}
        expect(f.value).to eq 0
      end
    end

    context 'passes the values of dependencies into the block' do

      specify 'if there is just one' do
        d = Future.new(executor: executor){14}
        f = Concurrent::dataflow(d){|v| v }
        d.execute
        expect(f.value).to eq 14

        d = Future.new(executor: executor){14}
        f = Concurrent::dataflow_with(root_executor, d){|v| v }
        d.execute
        expect(f.value).to eq 14
      end

      specify 'if there is more than one' do
        d1 = Future.new(executor: executor){14}
        d2 = Future.new(executor: executor){2}
        f = Concurrent::dataflow(d1, d2) {|v1, v2| v1 + v2}
        d1.execute
        d2.execute
        expect(f.value).to eq 16

        d1 = Future.new(executor: executor){14}
        d2 = Future.new(executor: executor){2}
        f = Concurrent::dataflow_with(root_executor, d1, d2) {|v1, v2| v1 + v2}
        d1.execute
        d2.execute
        expect(f.value).to eq 16
      end
    end

    context 'module function' do

      it 'can be called as Concurrent.dataflow and Concurrent.dataflow_with' do

        def fib_with_dot(n)
          if n < 2
            Concurrent.dataflow { n }
          else
            n1 = fib_with_dot(n - 1)
            n2 = fib_with_dot(n - 2)
            Concurrent.dataflow_with(ImmediateExecutor.new, n1, n2) { n1.value + n2.value }
          end
        end

        expected = fib_with_dot(7)
        expect(expected.value).to eq 13
      end

    end

    context 'result explanation' do

      it 'is available on every dataflow result, before the dependencies settle' do
        d1 = Future.new(executor: executor) { 1 }
        d2 = Future.new(executor: executor) { 2 }
        f = Concurrent::dataflow(d1, d2) { |v1, v2| v1 + v2 }

        explanation = f.dataflow_explanation
        expect(explanation).to be_a(Concurrent::DataflowExplanation)
        expect(explanation.size).to eq 2
        expect(explanation.dependencies.map(&:state)).to eq [:pending, :pending]
        expect(explanation.started?).to be false
        expect(explanation).to be_pending
      end

      it 'reports each dependency by its input position, whatever the completion order' do
        d1 = Future.new(executor: executor) { 1 }
        d2 = Future.new(executor: executor) { 2 }
        f = Concurrent::dataflow(d1, d2) { |v1, v2| v1 + v2 }

        d2.execute # out of order on purpose
        d1.execute

        expect(f.value).to eq 3
        explanation = f.dataflow_explanation
        expect(explanation.dependencies.map(&:index)).to eq [0, 1]
        expect(explanation.dependencies.map(&:state)).to eq [:fulfilled, :fulfilled]
        expect(explanation.started?).to be true
        expect(explanation).to be_fulfilled
        expect(explanation.dependency_failure?).to be false
        expect(explanation.block_failure?).to be false
      end

      it 'keeps a fulfilled dependency with a nil value, rather than treating it as missing' do
        d1 = Concurrent::dataflow { nil }
        d2 = Concurrent::dataflow { 2 }
        f = Concurrent::dataflow(d1, d2) { |v1, v2| [v1, v2] }

        expect(f.value).to eq [nil, 2]
        explanation = f.dataflow_explanation
        expect(explanation[0]).to be_fulfilled
        expect(explanation[0].reason).to be_nil
        expect(explanation.fulfilled_dependencies.map(&:index)).to eq [0, 1]
        expect(explanation.failed_dependencies).to be_empty
      end

      it 'reports no dependencies and a started block for a dataflow without inputs' do
        f = Concurrent::dataflow { 42 }

        expect(f.value).to eq 42
        explanation = f.dataflow_explanation
        expect(explanation.size).to eq 0
        expect(explanation.started?).to be true
        expect(explanation).to be_fulfilled
      end
    end

    context 'dependency failure attribution' do

      it 'names the failed dependency instead of failing as if the block raised' do
        d1 = Concurrent::dataflow { raise 'd1 error' }
        d2 = Concurrent::dataflow { 2 }
        f = Concurrent::dataflow!(d1, d2) { |v1, v2| v1 + v2 }

        reason = nil
        expect { f.value! }.to raise_error(Concurrent::DataflowDependencyError) { |error| reason = error }

        expect(reason.failed_indexes).to eq [0]
        expect(reason.reasons.map(&:message)).to eq ['d1 error']
        expect(reason.message).to include('index 0')
        expect(reason.message).to include('d1 error')
        expect(reason.message).to include('was not executed')
      end

      it 'does not execute the block when a dependency failed' do
        executed = false
        d1 = Concurrent::dataflow { raise 'd1 error' }
        d2 = Concurrent::dataflow { 2 }
        f = Concurrent::dataflow!(d1, d2) { |v1, v2| executed = true }

        expect { f.value! }.to raise_error(Concurrent::DataflowDependencyError)
        expect(executed).to be false
      end

      it 'marks the node as not started, unlike a node whose own block raised' do
        d1 = Concurrent::dataflow { raise 'd1 error' }
        f = Concurrent::dataflow!(d1) { |v1| v1 }

        expect { f.value! }.to raise_error(Concurrent::DataflowDependencyError)
        explanation = f.dataflow_explanation
        expect(explanation.started?).to be false
        expect(explanation).to be_rejected
        expect(explanation.dependency_failure?).to be true
        expect(explanation.block_failure?).to be false
        expect(explanation.failed_indexes).to eq [0]
      end

      it 'marks a node whose own block raised as started, and keeps the block error' do
        d1 = Concurrent::dataflow { 1 }
        f = Concurrent::dataflow!(d1) { |v1| raise 'block error' }

        expect { f.value! }.to raise_error(RuntimeError, 'block error')

        explanation = f.dataflow_explanation
        expect(explanation.started?).to be true
        expect(explanation).to be_rejected
        expect(explanation.block_failure?).to be true
        expect(explanation.dependency_failure?).to be false
        expect(explanation.reason.message).to eq 'block error'
        expect(explanation.dependencies.map(&:state)).to eq [:fulfilled]
      end

      it 'orders multiple simultaneous failures by input position, not completion order' do
        d1 = Future.new(executor: executor) { raise 'first' }
        d2 = Future.new(executor: executor) { raise 'second' }
        d3 = Future.new(executor: executor) { 3 }
        f = Concurrent::dataflow!(d1, d2, d3) { |*values| values }

        d2.execute # earliest to complete, but the later input
        d1.execute
        d3.execute

        expect { f.value! }.to raise_error(Concurrent::DataflowDependencyError) { |error|
          expect(error.failed_indexes).to eq [0, 1]
          expect(error.reasons.map(&:message)).to eq ['first', 'second']
        }

        explanation = f.dataflow_explanation
        expect(explanation.failed_indexes).to eq [0, 1]
        expect(explanation.dependencies.map(&:state)).to eq [:rejected, :rejected, :fulfilled]
      end

      it 'reports a stable order across repeated explanations' do
        d1 = Concurrent::dataflow { raise 'first' }
        d2 = Concurrent::dataflow { raise 'second' }
        f = Concurrent::dataflow!(d1, d2) { |*values| values }
        f.wait

        expect(f.dataflow_explanation.failed_indexes).to eq [0, 1]
        expect(f.dataflow_explanation.failed_indexes).to eq [0, 1]
        expect(f.reason.failed_indexes).to eq [0, 1]
      end

      it 'still passes values through when nothing failed, including nil values' do
        f = Concurrent::dataflow!(Concurrent::dataflow { nil }, Concurrent::dataflow { 2 }) do |v1, v2|
          [v1, v2]
        end

        expect(f.value!).to eq [nil, 2]
        expect(f.dataflow_explanation).to be_fulfilled
      end

      it 'propagates through a chain one level at a time' do
        root = Concurrent::dataflow { raise 'root error' }
        middle = Concurrent::dataflow!(root) { |v| v }
        leaf = Concurrent::dataflow!(middle) { |v| v }

        expect { leaf.value! }.to raise_error(Concurrent::DataflowDependencyError) do |error|
          expect(error.failed_indexes).to eq [0]
          expect(error.reasons.first).to be_a(Concurrent::DataflowDependencyError)
        end

        expect(middle.dataflow_explanation.started?).to be false
        expect(middle.dataflow_explanation.block_failure?).to be false
        expect(leaf.dataflow_explanation.started?).to be false
        expect(leaf.dataflow_explanation.dependency_failure?).to be true
      end
    end

    context 'failure propagation scheduling' do

      it 'does not schedule the node more than once when several dependencies fail' do
        executor = counting_executor
        d1 = Concurrent::dataflow { raise 'first' }
        d2 = Concurrent::dataflow { raise 'second' }
        f = Concurrent::dataflow_with!(executor, d1, d2) { |*values| values }

        expect { f.value! }.to raise_error(Concurrent::DataflowDependencyError)
        expect(executor.posts).to eq 0
      end

      it 'schedules the node exactly once when all dependencies fulfill' do
        executor = counting_executor
        d1 = Concurrent::dataflow { 1 }
        d2 = Concurrent::dataflow { 2 }
        f = Concurrent::dataflow_with(executor, d1, d2) { |v1, v2| v1 + v2 }

        expect(f.value!).to eq 3
        expect(executor.posts).to eq 1
      end

      it 'schedules the node once even when it is rejected by its own block failure' do
        executor = counting_executor
        d1 = Concurrent::dataflow { 1 }
        d2 = Concurrent::dataflow { 2 }
        f = Concurrent::dataflow_with(executor, d1, d2) { raise 'block error' }

        expect { f.value! }.to raise_error(RuntimeError, 'block error')
        expect(executor.posts).to eq 1
        expect(f.dataflow_explanation.block_failure?).to be true
      end
    end

    context 'explanation lifetime' do

      it 'does not retain the dependencies after the node settles' do
        d1 = Concurrent::dataflow { 1 }
        d2 = Concurrent::dataflow { 2 }
        f = Concurrent::dataflow(d1, d2) { |v1, v2| v1 + v2 }
        expect(f.value!).to eq 3

        explanation = f.dataflow_explanation
        explanation.instance_variables.each do |name|
          value = explanation.instance_variable_get(name)
          [*value].each do |entry|
            expect(entry).not_to be(d1)
            expect(entry).not_to be(d2)
          end
        end
      end

      it 'does not close the node over the dependency array' do
        d1 = Concurrent::dataflow { 1 }
        d2 = Concurrent::dataflow { 2 }
        f = Concurrent::dataflow(d1, d2) { |v1, v2| v1 + v2 }
        expect(f.value!).to eq 3

        task = f.instance_variable_get(:@task)
        expect(task.binding.local_variables).not_to include(:inputs)
        [*task.binding.local_variables].each do |name|
          value = task.binding.local_variable_get(name)
          [*value].each do |entry|
            expect(entry).not_to be(d1)
            expect(entry).not_to be(d2)
          end
        end
      end

      it 'keeps dependency state available after the node settles' do
        d1 = Concurrent::dataflow { nil }
        d2 = Concurrent::dataflow { raise 'failed dependency' }
        f = Concurrent::dataflow!(d1, d2) { |v1, v2| [v1, v2] }
        expect { f.value! }.to raise_error(Concurrent::DataflowDependencyError)

        explanation = f.dataflow_explanation
        expect(explanation[0]).to be_fulfilled
        expect(explanation[0].reason).to be_nil
        expect(explanation[1]).to be_rejected
        expect(explanation[1].reason.message).to eq 'failed dependency'
        expect(explanation.to_h[:dependencies].map { |d| d[:state] }).to eq [:fulfilled, :rejected]
      end

      it 'is reachable from the error raised by the rejected node' do
        d1 = Concurrent::dataflow { nil }
        d2 = Concurrent::dataflow { raise 'failed dependency' }
        f = Concurrent::dataflow!(d1, d2) { |v1, v2| [v1, v2] }
        expect { f.value! }.to raise_error(Concurrent::DataflowDependencyError) do |error|
          expect(error.explanation).to be(f.dataflow_explanation)
          expect(error.failed_dependencies.map(&:index)).to eq [1]
          expect(error.failed_dependencies.first.reason.message).to eq 'failed dependency'
        end
      end
    end

    context 'cancellation' do

      it 'records the cancellation in the explanation' do
        latch = Concurrent::CountDownLatch.new
        blocking = Concurrent::SingleThreadExecutor.new
        blocking.post { latch.wait(5) }

        d1 = Concurrent::dataflow { 1 }
        f = Concurrent::dataflow_with(blocking, d1) { |v| v }
        f.wait(0.2)

        expect(f.cancel).to be true
        explanation = f.dataflow_explanation
        expect(f).to be_rejected
        expect(explanation).to be_rejected
        expect(explanation.failure_kind).to eq :cancelled
        expect(explanation.cancelled?).to be true
        expect(explanation.started?).to be false
        expect(explanation.dependency_failure?).to be false
        expect(explanation.block_failure?).to be false
        latch.count_down
        blocking.shutdown
      end

      it 'does not rewrite the explanation once the node already settled' do
        f = Concurrent::dataflow { 42 }
        expect(f.value!).to eq 42
        expect(f.cancel).to be false
        expect(f.dataflow_explanation).to be_fulfilled
      end
    end
  end
end
