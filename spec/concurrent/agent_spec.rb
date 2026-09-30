require 'concurrent/agent'
require 'concurrent/executor/single_thread_executor'
require 'concurrent/executor/immediate_executor'
require 'concurrent/executor/fixed_thread_pool'
require 'concurrent/atomic/count_down_latch'
require 'concurrent/atomic/event'
require 'concurrent/atomic/atomic_boolean'

require_relative 'concern/observable_shared'

module Concurrent

  RSpec.describe Agent do

    let!(:immediate) { Concurrent::ImmediateExecutor.new }
    let!(:executor) { Concurrent::SingleThreadExecutor.new }

    context 'initialization' do

      it 'sets the initial value' do
        subject = Agent.new(42)
        expect(subject.value).to eq 42
      end

      it 'sets the initial error to nil' do
        subject = Agent.new(42)
        expect(subject.error).to be nil
      end

      it 'sets the error mode when given a valid value' do
        subject = Agent.new(42, error_mode: :fail)
        expect(subject.error_mode).to eq :fail
      end

      it 'defaults the error mode to :continue when an error handler is given' do
        subject = Agent.new(42, error_handler: ->(value) { true })
        expect(subject.error_mode).to eq :continue
      end

      it 'defaults the error mode to :fail when no error handler is given' do
        subject = Agent.new(42)
        expect(subject.error_mode).to eq :fail
      end

      it 'raises an error when given an invalid error mode' do
        expect {
          Agent.new(42, error_mode: :bogus)
        }.to raise_error(ArgumentError)
      end

      it 'sets #failed? to false' do
        subject = Agent.new(42)
        expect(subject).to_not be_failed
        expect(subject).to_not be_stopped
      end
    end

    context 'action processing' do

      specify 'the given block will be passed the current value' do
        actual   = nil
        expected = 0
        subject  = Agent.new(expected)
        subject.send_via(immediate) { |value| actual = value }
        expect(actual).to eq expected
      end

      specify 'the given block will be passed any provided arguments' do
        actual   = nil
        expected = [1, 2, 3, 4]
        subject  = Agent.new(0)
        subject.send_via(immediate, *expected) { |_, *args| actual = args }
        expect(actual).to eq expected
      end

      specify 'the return value will be passed to the validator function' do
        actual    = nil
        expected  = 42
        validator = ->(new_value) { actual = new_value; true }
        subject   = Agent.new(0, validator: validator)
        subject.send_via(immediate) { expected }
        expect(actual).to eq expected
      end

      specify 'upon validation the new value will be set to the block return value' do
        expected  = 42
        validator = ->(_new_value) { true }
        subject   = Agent.new(0, validator: validator)
        subject.send_via(immediate) { expected }
        expect(subject.value).to eq expected
      end

      specify 'on success all observers will be notified' do
        observer_class = Class.new do
          def initialize(bucket)
            @bucket = bucket
          end

          def update(time, old_value, new_value)
            @bucket.concat([time, old_value, new_value])
          end
        end

        bucket  = []
        subject = Agent.new(0)
        subject.add_observer(observer_class.new(bucket))
        subject.send_via(immediate) { 42 }

        expect(bucket[0]).to be_a Time
        expect(bucket[1]).to eq 0
        expect(bucket[2]).to eq 42
      end

      specify 'any recursive action dispatches will run after the value has been updated' do
        subject = Agent.new(0)

        subject.send_via(executor, subject) do |v1, a1|
          expect(v1).to eq 0
          a1.send_via(executor, a1) do |v2, a2|
            expect(v2).to eq 1
            a1.send_via(executor, a2) do |v3, a3|
              expect(v3).to eq 2
              3
            end
            2
          end
          1
        end
        expect(subject.await_for(1)).to eq true
        expect(subject).to_not be_failed
      end

      specify 'when the action raises an error the value will not change' do
        expected = 0
        subject  = Agent.new(expected)
        subject.send_via(immediate) { raise StandardError }
        expect(subject.value).to eq expected
      end

      specify 'when the action raises an error the validator will not be called' do
        validator_called = false
        validator        = ->(new_value) { validator_called = true }
        subject          = Agent.new(0, validator: validator)
        subject.send_via(immediate) { raise StandareError }
        expect(validator_called).to be false
      end

      specify 'when validation returns false the value will not change' do
        expected  = 0
        validator = ->(new_value) { false }
        subject   = Agent.new(0, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(subject.value).to eq expected
      end

      specify 'when validation raises an error the value will not change' do
        expected  = 0
        validator = ->(new_value) { raise StandareError }
        subject   = Agent.new(0, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(subject.value).to eq expected
      end

      specify 'when the action raises an error the handler will be called' do
        error_handler_called = false
        error_handler        = ->(agent, exception) { error_handler_called = true }
        subject              = Agent.new(0, error_handler: error_handler)
        subject.send_via(immediate) { raise StandardError }
        expect(error_handler_called).to be true
      end

      specify 'when validation fails the handler will be called' do
        error_handler_called = false
        error_handler        = ->(agent, exception) { error_handler_called = true }
        validator            = ->(new_value) { false }
        subject              = Agent.new(0, error_handler: error_handler, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(error_handler_called).to be true
      end

      specify 'when validation raises an error the handler will be called' do
        error_handler_called = false
        error_handler        = ->(agent, exception) { error_handler_called = true }
        validator            = ->(new_value) { raise StandardError }
        subject              = Agent.new(0, error_handler: error_handler, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(error_handler_called).to be true
      end
    end

    context 'validation' do

      it 'sets the new value when the validator returns true' do
        expected  = 42
        validator = ->(new_value) { true }
        subject   = Agent.new(0, validator: validator)
        subject.send_via(immediate) { expected }
        expect(subject.value).to eq expected
      end

      it 'rejects the new value when the validator returns false' do
        expected  = 0
        validator = ->(new_value) { false }
        subject   = Agent.new(expected, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(subject.value).to eq expected
      end

      it 'rejects the new value when the validator raises an error' do
        expected  = 0
        validator = ->(new_value) { raise StandardError }
        subject   = Agent.new(expected, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(subject.value).to eq expected
      end

      it 'sets the error when the error mode is :fail and the validator returns false' do
        validator = ->(new_value) { false }
        subject   = Agent.new(0, error_mode: :fail, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(subject.error).to be_a Agent::ValidationError
      end

      it 'sets the error when the error mode is :fail and the validator raises an error' do
        validator = ->(new_value) { raise expected }
        subject   = Agent.new(0, error_mode: :fail, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(subject.error).to be_a Agent::ValidationError
      end

      it 'does not set an error when the error mode is :continue and the validator returns false' do
        validator = ->(new_value) { false }
        subject   = Agent.new(0, error_mode: :continue, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(subject.error).to be nil
      end

      it 'does not set an error when the error mode is :continue and the validator raises an error' do
        validator = ->(new_value) { raise StandardError }
        subject   = Agent.new(0, error_mode: :continue, validator: validator)
        subject.send_via(immediate) { 42 }
        expect(subject.error).to be nil
      end

      it 'does not trigger observation when validation fails' do
        observer_class = Class.new do
          attr_reader :count

          def initialize
            @count = 0
          end

          def update(time, old_value, new_value)
            @count += 1
          end
        end

        observer = observer_class.new
        subject  = Agent.new(0, validator: ->(new_value) { false })
        subject.add_observer(observer)
        subject.send_via(immediate) { 42 }

        expect(observer.count).to eq 0
      end
    end

    context 'error handling' do

      specify 'the agent will be passed to the handler' do
        actual        = nil
        error_handler = ->(agent, error) { actual = agent }
        subject       = Agent.new(0, error_handler: error_handler)
        subject.send_via(immediate) { raise StandardError }
        expect(actual).to eq subject
      end

      specify 'the exception will be passed to the handler' do
        expected      = StandardError.new
        actual        = nil
        error_handler = ->(agent, error) { actual = error }
        subject       = Agent.new(0, error_handler: error_handler)
        subject.send_via(immediate) { raise expected }
        expect(actual).to eq expected
      end

      specify 'does not trigger observation' do
        observer_class = Class.new do
          attr_reader :count

          def initialize
            @count = 0
          end

          def update(time, old_value, new_value)
            @count += 1
          end
        end

        observer = observer_class.new
        subject  = Agent.new(0)
        subject.add_observer(observer)
        subject.send_via(immediate) { raise StandardError }

        expect(observer.count).to eq 0
      end
    end

    context 'error mode' do

      context ':continue' do

        it 'does not set an error when the validator returns false' do
          validator = ->(new_value) { false }
          subject   = Agent.new(0, error_mode: :continue, validator: validator)
          subject.send_via(immediate) { 42 }
          expect(subject.error).to be nil
        end

        it 'does not set an error when the validator raises an error' do
          validator = ->(new_value) { raise StandardError }
          subject   = Agent.new(0, error_mode: :continue, validator: validator)
          subject.send_via(immediate) { 42 }
          expect(subject.error).to be nil
        end

        it 'does not set an error when the action raises an error' do
          subject = Agent.new(0, error_mode: :continue)
          subject.send_via(immediate) { raise StandardError }
          expect(subject.error).to be nil
        end

        it 'does not block further action processing' do
          subject  = Agent.new(0, error_mode: :continue)
          subject.send_via(immediate) { raise StandardError }
          subject.send_via(immediate) { 42 }
          expect(subject.value).to eq 42
        end

        it 'sets #failed? to false' do
          subject = Agent.new(0, error_mode: :continue)
          subject.send_via(immediate) { raise StandardError }
          expect(subject).to_not be_failed
        end
      end

      context ':fail' do

        it 'sets the error when the validator returns false' do
          validator = ->(new_value) { false }
          subject   = Agent.new(0, error_mode: :fail, validator: validator)
          subject.send_via(immediate) { 42 }
          expect(subject.error).to be_a Agent::ValidationError
        end

        it 'sets the error when the validator raises an error' do
          validator = ->(new_value) { raise expected }
          subject   = Agent.new(0, error_mode: :fail, validator: validator)
          subject.send_via(immediate) { 42 }
          expect(subject.error).to be_a Agent::ValidationError
        end

        it 'sets the error when the action raises an error' do
          expected = StandardError.new
          subject  = Agent.new(0, error_mode: :fail)
          subject.send_via(immediate) { raise expected }
          expect(subject.error).to eq expected
        end

        it 'blocks all further action processing until a restart' do
          latch    = Concurrent::CountDownLatch.new
          expected = 42

          subject = Agent.new(0, error_mode: :fail)
          subject.send_via(immediate) { raise StandardError }
          subject.send_via(executor) { latch.count_down; expected }

          latch.wait(0.1)
          expect(subject.value).to eq 0

          subject.restart(42)
          latch.wait(0.1)
          expect(subject.await_for(1)).to eq true
          expect(subject.value).to eq expected
        end

        it 'sets #failed? to true' do
          subject = Agent.new(0, error_mode: :fail)
          subject.send_via(immediate) { raise StandardError }
          expect(subject).to be_failed
        end
      end
    end

    context 'nested actions' do

      specify 'occur in the order they ar post' do
        actual   = []
        expected = [0, 1, 2, 3, 4]
        latch    = Concurrent::CountDownLatch.new
        subject  = Agent.new(0)

        subject.send_via(executor, subject) do |v1, a1|
          a1.send_via(executor, a1) do |v2, a2|
            a1.send_via(executor, a2) do |v3, a3|
              a1.send_via(executor, a3) do |v4, a4|
                a1.send_via(executor, a4) do |v5, a5|
                  actual << v5; latch.count_down
                end
                actual << v4; v4 + 1
              end
              actual << v3; v3 + 1
            end
            actual << v2; v2 + 1
          end
          actual << v1; v1 + 1
        end

        latch.wait(2)
        expect(subject.await_for(1)).to eq true
        expect(actual).to eq expected
      end

      specify 'work with immediate execution' do
        actual   = []
        expected = [0, 1, 2]
        subject  = Agent.new(0)

        subject.send_via(immediate) do |v1|
          subject.send_via(immediate) do |v2|
            subject.send_via(immediate) do |v3|
              actual << v3
            end
            actual << v2; v2 + 1
          end
          actual << v1; v1 + 1
        end

        expect(actual).to eq expected
      end
    end

    context 'posting' do

      context 'with #send' do

        it 'returns true when the job is post' do
          subject = Agent.new(0)
          expect(subject.send { nil }).to be true
          expect(subject.await_for(1)).to eq true
        end

        it 'returns false when #failed?' do
          subject = Agent.new(0)
          allow(subject).to receive(:failed?).and_return(true)
          expect(subject.send { nil }).to be false
          expect(subject.await_for(1)).to eq true
        end

        it 'posts to the global fast executor' do
          subject = Agent.new(0)
          expect(subject).to receive(:enqueue_action_job).with(anything, anything, Concurrent.global_fast_executor).and_call_original
          subject.send { nil }
          expect(subject.await_for(1)).to eq true
        end

        it 'does not wait for the action to process' do
          job_done = false
          subject  = Agent.new(0)
          latch = CountDownLatch.new
          subject.send { latch.wait; job_done = true }
          expect(job_done).to be false
          latch.count_down
          expect(subject.await_for(1)).to eq true
        end
      end

      context 'with #send!' do

        it 'returns true when the job is post' do
          subject = Agent.new(0)
          expect(subject.send! { nil }).to be true
          expect(subject.await_for(1)).to eq true
        end

        it 'raises an error when #failed?' do
          subject = Agent.new(0)
          allow(subject).to receive(:failed?).and_return(true)
          expect {
            subject.send! { nil }
          }.to raise_error(Agent::Error)
        end

        it 'posts to the global fast executor' do
          subject = Agent.new(0)
          expect(subject).to receive(:enqueue_action_job).with(anything, anything, Concurrent.global_fast_executor).and_call_original
          subject.send! { nil }
          expect(subject.await_for(1)).to eq true
        end

        it 'does not wait for the action to process' do
          job_done = false
          subject  = Agent.new(0)
          latch = CountDownLatch.new
          subject.send! { latch.wait; job_done = true }
          expect(job_done).to be false
          latch.count_down
          expect(subject.await_for(1)).to eq true
        end
      end

      context 'with #send_off' do

        it 'returns true when the job is post' do
          subject = Agent.new(0)
          expect(subject.send_off { nil }).to be true
          expect(subject.await_for(1)).to eq true
        end

        it 'returns false when #failed?' do
          subject = Agent.new(0)
          allow(subject).to receive(:failed?).and_return(true)
          expect(subject.send_off { nil }).to be false
        end

        it 'posts to the global io executor' do
          subject = Agent.new(0)
          expect(subject).to receive(:enqueue_action_job).with(anything, anything, Concurrent.global_io_executor).and_call_original
          subject.send_off { nil }
          expect(subject.await_for(1)).to eq true
        end

        it 'does not wait for the action to process' do
          job_done = false
          subject  = Agent.new(0)
          latch = CountDownLatch.new
          subject.send_off { latch.wait; job_done = true }
          expect(job_done).to be false
          latch.count_down
          expect(subject.await_for(1)).to eq true
        end
      end

      context 'with #send_off!' do

        it 'returns true when the job is post' do
          subject = Agent.new(0)
          expect(subject.send_off! { nil }).to be true
          expect(subject.await_for(1)).to eq true
        end

        it 'raises an error when #failed?' do
          subject = Agent.new(0)
          allow(subject).to receive(:failed?).and_return(true)
          expect {
            subject.send_off! { nil }
          }.to raise_error(Agent::Error)
        end

        it 'posts to the global io executor' do
          subject = Agent.new(0)
          expect(subject).to receive(:enqueue_action_job).with(anything, anything, Concurrent.global_io_executor).and_call_original
          subject.send_off! { nil }
          expect(subject.await_for(1)).to eq true
        end

        it 'does not wait for the action to process' do
          job_done = false
          subject  = Agent.new(0)
          latch = CountDownLatch.new
          subject.send_off! { latch.wait; job_done = true }
          expect(job_done).to be false
          latch.count_down
          expect(subject.await_for(1)).to eq true
        end
      end

      context 'with #send_via' do

        it 'returns true when the job is post' do
          subject = Agent.new(0)
          expect(subject.send_via(immediate) { nil }).to be true
        end

        it 'returns false when #failed?' do
          subject = Agent.new(0)
          allow(subject).to receive(:failed?).and_return(true)
          expect(subject.send_via(immediate) { nil }).to be false
        end

        it 'posts to the given executor' do
          expect(immediate).to receive(:post).with(any_args).and_call_original
          subject = Agent.new(0)
          subject.send_via(immediate) { nil }
        end
      end

      context 'with #send_via!' do

        it 'returns true when the job is post' do
          subject = Agent.new(0)
          expect(subject.send_via!(immediate) { nil }).to be true
        end

        it 'raises an error when #failed?' do
          subject = Agent.new(0)
          allow(subject).to receive(:failed?).and_return(true)
          expect {
            subject.send_via!(immediate) { nil }
          }.to raise_error(Agent::Error)
        end

        it 'posts to the given executor' do
          expect(immediate).to receive(:post).with(any_args).and_call_original
          subject = Agent.new(0)
          subject.send_via!(immediate) { nil }
        end
      end

      context 'with #post' do

        it 'returns true when the job is post' do
          subject = Agent.new(0)
          expect(subject.post { nil }).to be true
          expect(subject.await_for(1)).to eq true
        end

        it 'returns false when #failed?' do
          subject = Agent.new(0)
          allow(subject).to receive(:failed?).and_return(true)
          expect(subject.post { nil }).to be false
        end

        it 'posts to the global io executor' do
          subject = Agent.new(0)
          expect(subject).to receive(:enqueue_action_job).with(anything, anything, Concurrent.global_io_executor).and_call_original
          subject.post { nil }
          expect(subject.await_for(1)).to eq true
        end

        it 'does not wait for the action to process' do
          job_done = false
          subject  = Agent.new(0)
          latch = CountDownLatch.new
          subject.post { latch.wait; job_done = true }
          expect(job_done).to be false
          latch.count_down
          expect(subject.await_for(1)).to eq true
        end
      end

      context 'with #<<' do

        it 'returns self when the job is post' do
          subject = Agent.new(0)
          expect(subject << proc { nil }).to be subject
          expect(subject.await_for(1)).to eq true
        end

        it 'returns self when #failed?' do
          subject = Agent.new(0)
          allow(subject).to receive(:failed?).and_return(true)
          expect(subject << proc { nil }).to be subject
        end

        it 'posts to the global io executor' do
          subject = Agent.new(0)
          expect(subject).to receive(:enqueue_action_job).with(anything, anything, Concurrent.global_io_executor).and_call_original
          subject << proc { nil }
          expect(subject.await_for(1)).to eq true
        end

        it 'does not wait for the action to process' do
          job_done = false
          subject  = Agent.new(0)
          latch = CountDownLatch.new
          subject << proc { latch.wait; job_done = true }
          expect(job_done).to be false
          latch.count_down
          expect(subject.await_for(1)).to eq true
        end
      end
    end

    context '#restart' do

      context 'when #failed?' do

        it 'raises an error if the new value is not valid' do
          subject = Agent.new(0, error_mode: :fail, validator: ->(new_value) { false })
          subject.send_via(immediate) { raise StandardError }

          expect {
            subject.restart(0)
          }.to raise_error(Agent::Error)
        end

        it 'sets the new value' do
          subject = Agent.new(0, error_mode: :fail)
          subject.send_via(immediate) { raise StandardError }

          subject.restart(42)
          expect(subject.value).to eq 42
        end

        it 'clears the error' do
          subject = Agent.new(0, error_mode: :fail)
          subject.send_via(immediate) { raise StandardError }

          subject.restart(42)
          expect(subject.error).to be nil
        end

        it 'sets #failed? to true' do
          subject = Agent.new(0, error_mode: :fail)
          subject.send_via(immediate) { raise StandardError }

          subject.restart(42)
          expect(subject).to_not be_failed
        end

        it 'removes all actions from the queue when :clear_actions is true' do
          latch     = Concurrent::CountDownLatch.new
          end_latch = Concurrent::CountDownLatch.new
          subject   = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { latch.wait; raise StandardError }
          subject.send_via(executor) { end_latch.count_down }

          latch.count_down
          10.times { break if subject.failed?; sleep(0.1) }

          subject.restart(42, clear_actions: true)
          result = end_latch.wait(0.1)
          expect(result).to be false
          expect(subject.await_for(1)).to eq true
        end

        it 'does not clear the action queue when :clear_actions is false' do
          latch     = Concurrent::CountDownLatch.new
          end_latch = Concurrent::CountDownLatch.new
          subject   = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { latch.wait; raise StandardError }
          subject.send_via(executor) { end_latch.count_down }

          latch.count_down
          10.times { break if subject.failed?; sleep(0.1) }

          subject.restart(42, clear_actions: false)
          result = end_latch.wait(3)
          expect(result).to be true
          expect(subject.await_for(1)).to eq true
        end

        it 'does not clear the action queue when :clear_actions is not given' do
          latch     = Concurrent::CountDownLatch.new
          end_latch = Concurrent::CountDownLatch.new
          subject   = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { latch.wait; raise StandardError }
          subject.send_via(executor) { end_latch.count_down }

          latch.count_down
          10.times { break if subject.failed?; sleep(0.1) }

          subject.restart(42)
          result = end_latch.wait(3)
          expect(result).to be true
          expect(subject.await_for(1)).to eq true
        end

        it 'resumes action processing if actions are enqueued' do
          count        = 5
          latch        = Concurrent::CountDownLatch.new
          finish_latch = Concurrent::CountDownLatch.new(5)
          subject      = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { latch.wait; raise StandardError }
          count.times { subject.send_via(executor) { finish_latch.count_down } }

          queue = subject.instance_variable_get(:@queue)
          size  = queue.size
          expect(size).to be > 0

          latch.count_down
          10.times { break if subject.failed?; sleep(0.1) }

          subject.restart(42, clear_actions: false)
          expect(finish_latch.wait(5)).to be true
          expect(subject.await_for(1)).to eq true
        end

        it 'does not trigger observation' do
          observer_class = Class.new do
            attr_reader :count

            def initialize
              @count = 0
            end

            def update(time, old_value, new_value)
              @count += 1
            end
          end

          observer = observer_class.new
          subject  = Agent.new(0, error_mode: :fail)
          subject.add_observer(observer)
          subject.send_via(immediate) { raise StandardError }
          subject.restart(42)

          expect(observer.count).to eq 0
        end
      end

      context 'when not #failed?' do

        it 'raises an error' do
          subject = Agent.new(0)
          expect {
            subject.restart(0)
          }.to raise_error(Agent::Error)
        end
      end
    end

    context 'waiting' do

      context 'the await job' do

        it 'does not change the value' do
          expected = 42
          subject  = Agent.new(0)
          subject.send_via(executor) { sleep(0.1); expected }
          subject.await_for(1)
          expect(subject.value).to eq expected
        end

        it 'does not trigger the error mode' do
          subject = Agent.new(10)
          subject.send { |x| sleep(0.1); x + 1 }
          subject.await_for(1)

          expect(subject.value).to eq 11
          expect(subject).to_not be_failed
          expect(subject.error).to be nil
        end

        it 'does not trigger observers' do
          observer_class = Class.new do
            attr_reader :count

            def initialize
              @count = 0
            end

            def update(time, old_value, new_value)
              @count += 1
            end
          end

          observer = observer_class.new
          subject  = Agent.new(0)
          subject.add_observer(observer)
          subject.send_via(executor) { sleep(0.1); 42 }
          subject.await_for(1)

          expect(observer.count).to eq 1
        end

        it 'waits for nested actions' do
          bucket   = []
          latch    = Concurrent::CountDownLatch.new
          executor = Concurrent::FixedThreadPool.new(3)
          subject  = Agent.new(0)

          subject.send_via(executor) do
            subject.send_via(executor) do
              subject.send_via(executor) do
                bucket << 3
              end
              latch.count_down
              sleep(0.2)
              bucket << 2
            end
            bucket << 1
          end
          latch.wait

          subject.await_for(5)
          expect(bucket).to eq [1, 2, 3]

          executor.kill
          expect(executor.wait_for_termination(pool_termination_timeout)).to eq true
        end
      end

      context 'with #await' do

        it 'returns self when there are no pending actions' do
          subject = Agent.new(0)
          expect(subject.await).to eq subject
          expect(subject.await.value).to eq 0
        end

        it 'does not block on actions from other threads' do
          latch   = Concurrent::CountDownLatch.new
          finish   = Concurrent::CountDownLatch.new
          subject = Agent.new(0)
          in_thread do
            subject.send_via(executor) { finish.wait }
            latch.count_down
          end

          latch.wait(0.1)
          expect(subject.await_for(1)).to eq true
          finish.count_down
        end

        it 'blocks indefinitely' do
          start   = Concurrent.monotonic_time
          subject = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          expect(subject.await).to be_truthy
          expect(Concurrent.monotonic_time - start).to be > 0.5
        end

        it 'returns true when all prior actions have processed' do
          count    = 0
          expected = 5
          subject  = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          expected.times { subject.send_via(executor) { count += 1 } }
          subject.await
          expect(count).to eq expected
        end

        it 'blocks forever if restarted with :clear_actions true' do
          pending('the timing is nearly impossible'); fail
          subject = Agent.new(0, error_mode: :fail)

          t = in_thread do
            subject.send_via(executor) { sleep(0.1) }
            subject.send_via(executor) { raise StandardError }
            subject.send_via(executor) { nil }
            in_thread { subject.restart(42, clear_actions: true) }
            subject.await
          end

          thread_status = t.join(0.3)
          expect(thread_status).to be nil
        end
      end

      context 'with #await_for' do

        it 'returns true when there are no pending actions' do
          subject = Agent.new(0)
          expect(subject.await_for(1)).to be true
        end

        it 'does not block on actions from other threads' do
          latch   = Concurrent::CountDownLatch.new
          finish   = Concurrent::CountDownLatch.new
          subject = Agent.new(0)
          in_thread do
            subject.send_via(executor) { finish.wait }
            latch.count_down
          end

          latch.wait(0.1)
          expect(subject.await_for(0.1)).to be true
          finish.count_down
        end

        it 'returns true when all prior actions have processed' do
          subject = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          5.times { subject.send_via(executor) { nil } }
          expect(subject.await_for(10)).to be true
        end

        it 'returns false on timeout' do
          subject = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          5.times { subject.send_via(executor) { nil } }
          expect(subject.await_for(0.1)).to be false
          expect(subject.await_for(5)).to eq true
        end

        it 'returns false if restarted with :clear_actions true' do
          pending('the timing is nearly impossible'); fail
          subject = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { sleep(0.1) }
          subject.send_via(executor) { raise StandardError }
          subject.send_via(executor) { nil }

          in_thread { subject.restart(42, clear_actions: true) }
          ok = subject.await_for(0.2)

          expect(ok).to be false
        end
      end

      context 'with #await_for!' do

        it 'returns true when there are no pending actions' do
          subject = Agent.new(0)
          expect(subject.await_for!(1)).to be true
        end

        it 'does not block on actions from other threads' do
          latch   = Concurrent::CountDownLatch.new
          finish   = Concurrent::CountDownLatch.new
          subject = Agent.new(0)
          in_thread do
            subject.send_via(executor) { finish.wait }
            latch.count_down
          end

          latch.wait(0.1)
          expect(subject.await_for!(0.1)).to be true
          finish.count_down
        end

        it 'returns true when all prior actions have processed' do
          subject = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          5.times { subject.send_via(executor) { nil } }
          expect(subject.await_for!(10)).to be true
        end

        it 'raises an error on timeout' do
          subject = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          5.times { subject.send_via(executor) { nil } }
          expect {
            subject.await_for!(0.1)
          }.to raise_error(Concurrent::TimeoutError)
          expect(subject.await_for(5)).to eq true
        end

        it 'raises an error if restarted with :clear_actions true' do
          pending('the timing is nearly impossible'); fail
          subject = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { sleep(0.1) }
          subject.send_via(executor) { raise StandardError }
          subject.send_via(executor) { nil }

          in_thread { subject.restart(42, clear_actions: true) }

          expect {
            subject.await_for!(0.2)
          }.to raise_error(Concurrent::TimeoutError)
        end
      end

      context 'with #wait' do

        it 'returns true when there are no pending actions and timeout is nil' do
          subject = Agent.new(0)
          expect(subject.wait(nil)).to be true
        end

        it 'returns true when there are no pending actions and a timeout is given' do
          subject = Agent.new(0)
          expect(subject.wait(1)).to be true
        end

        it 'does not block on actions from other threads' do
          latch   = Concurrent::CountDownLatch.new
          finish   = Concurrent::CountDownLatch.new
          subject = Agent.new(0)
          in_thread do
            subject.send_via(executor) { finish.wait }
            latch.count_down
          end

          latch.wait(0.1)
          expect(subject.wait(0.1)).to be true
          finish.count_down
        end

        it 'blocks indefinitely when timeout is nil' do
          start   = Concurrent.monotonic_time
          subject = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          expect(subject.wait(nil)).to be true
          expect(Concurrent.monotonic_time - start).to be > 0.5
        end

        it 'blocks forever when timeout is nil and restarted with :clear_actions true' do
          pending('the timing is nearly impossible'); fail
          subject = Agent.new(0, error_mode: :fail)

          t = in_thread do
            subject.send_via(executor) { sleep(0.1) }
            subject.send_via(executor) { raise StandardError }
            subject.send_via(executor) { nil }
            in_thread { subject.restart(42, clear_actions: true) }
            subject.wait(nil)
          end

          expect(t.join(0.3)).to be nil
        end

        it 'returns true when all prior actions have processed' do
          count    = 0
          expected = 5
          subject  = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          expected.times { subject.send_via(executor) { count += 1 } }
          subject.wait(nil)
          expect(count).to eq expected
        end

        it 'returns false on timeout' do
          subject = Agent.new(0)
          subject.send_via(executor) { sleep(1) }
          5.times { subject.send_via(executor) { nil } }
          expect(subject.wait(0.1)).to be false
          expect(subject.wait(5)).to eq true
        end

        it 'returns false when timeout is given and restarted with :clear_actions true' do
          pending('the timing is nearly impossible'); fail
          subject = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { sleep(0.1) }
          subject.send_via(executor) { raise StandardError }
          subject.send_via(executor) { nil }

          in_thread { subject.restart(42, clear_actions: true) }
          ok = subject.wait(0.2)

          expect(ok).to be false
        end
      end

      context 'with .await' do

        it 'returns true when all prior actions on all agents have processed' do
          latch  = Concurrent::CountDownLatch.new
          agents = 3.times.collect { Agent.new(0) }
          agents.each { |agent| agent.send_via(executor, latch) { |_, l| l.wait(1) } }
          in_thread { latch.count_down }
          ok = Agent.await(*agents)
          expect(ok).to be true
        end
      end

      context 'with .await_for' do

        it 'returns true when there are no pending actions' do
          agents = 3.times.collect { Agent.new(0) }
          ok     = Agent.await_for(1, *agents)
          expect(ok).to be true
        end

        it 'returns true when all prior actions for all agents have processed' do
          latch  = Concurrent::CountDownLatch.new
          agents = 3.times.collect { Agent.new(0) }
          agents.each { |agent| agent.send_via(executor, latch) { |_, l| l.wait(1) } }
          in_thread { latch.count_down }
          ok = Agent.await_for(5, *agents)
          expect(ok).to be true
        end

        it 'returns false on timeout' do
          agents = 3.times.collect { Agent.new(0) }
          agents.each { |agent| agent.send_via(executor) { sleep(0.3) } }
          ok = Agent.await_for(0.1, *agents)
          expect(ok).to be false
          expect(Agent.await_for!(1, *agents)).to eq true
        end
      end

      context 'with await_for!' do

        it 'returns true when there are no pending actions' do
          agents = 3.times.collect { Agent.new(0) }
          ok     = Agent.await_for!(1, *agents)
          expect(ok).to be true
        end

        it 'returns true when all prior actions for all agents have processed' do
          latch  = Concurrent::CountDownLatch.new
          agents = 3.times.collect { Agent.new(0) }
          agents.each { |agent| agent.send_via(executor, latch) { |_, l| l.wait(1) } }
          in_thread { latch.count_down }
          ok = Agent.await_for!(5, *agents)
          expect(ok).to be true
        end

        it 'raises an exception on timeout' do
          agents = 3.times.collect { Agent.new(0) }
          agents.each { |agent| agent.send_via(executor) { sleep(0.3) } }
          expect {
            Agent.await_for!(0.1, *agents)
          }.to raise_error(Concurrent::TimeoutError)
          expect(Agent.await_for!(1, *agents)).to eq true
        end
      end
    end

    context :observable do

      subject { Agent.new(0) }

      def trigger_observable(observable)
        observable.send_via(immediate) { 42 }
      end

      it_behaves_like :observable
    end

    context 'keyed latest dispatch (#send_latest)' do

      specify 'returns a pending Receipt for every dispatch' do
        subject = Agent.new(0)
        receipt = subject.send_latest(:a) { 1 }
        expect(receipt).to be_a Agent::Receipt
        subject.await_for(1)
        expect(receipt).to be_complete
        expect(receipt).to be_fulfilled
      end

      specify 'the receipt is fulfilled with the resulting agent value' do
        subject = Agent.new(0)
        receipt = subject.send_latest(:a) { |v| v + 10 }
        expect(receipt.wait(1)).to be true
        expect(receipt.value).to eq 10
        expect(subject.value).to eq 10
      end

      specify 'each dispatch returns its own distinct receipt' do
        subject = Agent.new(0)
        receipts = 3.times.map { |i| subject.send_latest(:a, i) { |v, i| v + i } }
        subject.await_for(1)
        expect(receipts.uniq.length).to eq 3
      end

      specify 'replaces only not-yet-started messages with the same key' do
        subject  = Agent.new([])
        started  = Concurrent::Event.new
        releaser = Concurrent::CountDownLatch.new(1)

        first = subject.send_latest_via(executor, :a, :first) do |list, p|
          started.set
          releaser.wait(5)
          list + [p]
        end
        expect(started.wait(2)).to be true
        second = subject.send_latest_via(executor, :a, :second) { |list, p| list + [p] }
        third  = subject.send_latest_via(executor, :a, :third)  { |list, p| list + [p] }

        expect(second).to be_superseded
        expect(third.state).to eq :pending

        releaser.count_down
        subject.await_for(2)

        expect(subject.value).to eq [:first, :third]
        expect(first).to be_fulfilled
        expect(second).to be_superseded
        expect(second.reason).to be_a Agent::SupersededError
        expect(third).to be_fulfilled
        expect(third.value).to eq [:first, :third]
      end

      specify 'the replacement keeps the original position relative to other messages' do
        order    = []
        subject  = Agent.new(0)

        # queue while a first action blocks: [a0(blocking), a1, b1, a2, b2]
        started = Concurrent::Event.new
        gate    = Concurrent::CountDownLatch.new(1)
        a0 = subject.send_latest_via(executor, :a) { started.set; gate.wait(5); order << :a0; :a0 }
        expect(started.wait(2)).to be true
        a1 = subject.send_latest_via(executor, :a) { order << :a1; :a1 }
        b1 = subject.send_latest_via(executor, :b) { order << :b1; :b1 }
        a2 = subject.send_latest_via(executor, :a) { order << :a2; :a2 }
        b2 = subject.send_latest_via(executor, :b) { order << :b2; :b2 }

        # latest-write-wins per key, but the surviving message stays at the
        # original queue position of its key (positions 1 and 2)
        expect(a1).to be_superseded
        expect(b1).to be_superseded
        expect(a2.state).to eq :pending
        expect(b2.state).to eq :pending

        gate.count_down
        expect(subject.await_for(5)).to be true

        expect(order).to eq [:a0, :a2, :b2]
        expect(a0.value).to eq :a0
        expect(a2.value).to eq :a2
        expect(b2.value).to eq :b2
      end

      specify 'different keys and plain messages do not interfere' do
        executed = []
        subject  = Agent.new(0)
        started  = Concurrent::Event.new
        gate     = Concurrent::CountDownLatch.new(1)
        record   = ->(tag) { ->(value) { executed << tag; value } }

        # hold the head so the rest queue up and can be coalesced
        head = subject.send_via(executor) { started.set; gate.wait(5); executed << :head }
        expect(started.wait(2)).to be true

        # interleave two merge keys with ordinary (keyless) messages
        x1    = subject.send_latest_via(executor, :x, &record.call(:x1))
        y1    = subject.send_latest_via(executor, :y, &record.call(:y1))
        plain = subject.send_via(executor, &record.call(:plain))
        x2    = subject.send_latest_via(executor, :x, &record.call(:x2))
        y2    = subject.send_latest_via(executor, :y, &record.call(:y2))
        x3    = subject.send_latest_via(executor, :x, &record.call(:x3))

        expect(x1).to be_superseded
        expect(x2).to be_superseded
        expect(y1).to be_superseded
        expect(y2.state).to eq :pending
        expect(x3.state).to eq :pending

        gate.count_down
        expect(subject.await_for(5)).to be true

        # exactly the surviving payloads run, in queue order
        expect(executed).to eq [:head, :x3, :y2, :plain]
        expect(plain).to be true
        expect(head).to be true
        [y2, x3].each { |r| expect(r).to be_fulfilled }
        expect(x1).to be_superseded
        expect(x2).to be_superseded
        expect(y1).to be_superseded
      end

      specify 'an action already executing is never modified' do
        subject  = Agent.new(0)
        started  = Concurrent::Event.new
        releaser = Concurrent::CountDownLatch.new(1)

        running = subject.send_latest(:a, 1) do |_, tag|
          started.set
          releaser.wait(5)
          tag
        end
        expect(started.wait(2)).to be true
        refresh = subject.send_latest(:a, 2) { |_, tag| tag }

        releaser.count_down
        subject.await_for(2)

        expect(running).to be_fulfilled
        expect(running.value).to eq 1
        expect(refresh).to be_fulfilled
        expect(refresh.value).to eq 2
        expect(subject.value).to eq 2
      end

      specify 'passes extra arguments to the action' do
        subject = Agent.new(0)
        receipt = subject.send_latest_off(:a, 5, 7) { |v, x, y| v + x + y }
        subject.await_for(1)
        expect(receipt.value).to eq 12
      end

      specify 'raises ArgumentError when the merge key is nil' do
        subject = Agent.new(0)
        expect { subject.send_latest(nil) { 1 } }.to raise_error(ArgumentError)
        expect { subject.send_latest_off(nil) { 1 } }.to raise_error(ArgumentError)
        expect { subject.send_latest_via(immediate, nil) { 1 } }.to raise_error(ArgumentError)
      end

      specify 'raises ArgumentError when no action is given' do
        subject = Agent.new(0)
        expect { subject.send_latest(:a) }.to raise_error(ArgumentError)
      end

      specify '#value! raises the SupersededError of a superseded dispatch' do
        subject  = Agent.new(0)
        started  = Concurrent::Event.new
        releaser = Concurrent::CountDownLatch.new(1)

        subject.send_latest_via(executor, :a, :head) do
          started.set
          releaser.wait(5)
          :head
        end
        expect(started.wait(2)).to be true
        second = subject.send_latest_via(executor, :a, :second) { |_, p| p }
        third  = subject.send_latest_via(executor, :a, :third)  { |_, p| p }

        expect(second).to be_superseded

        releaser.count_down
        expect(third.value!(2)).to eq :third
        expect { second.value! }.to raise_error(Agent::SupersededError)
      end

      context 'when the action raises in :fail mode' do

        it 'rejects the receipt of the failing action with the error' do
          boom    = StandardError.new('boom')
          subject = Agent.new(0, error_mode: :fail)
          receipt = subject.send_latest(:a) { raise boom }
          expect(receipt.wait(1)).to be true
          expect(receipt).to be_rejected
          expect(receipt.reason).to eq boom
          expect(subject).to be_failed
        end

        it 'rejects still-queued receipts and does not run their actions' do
          ran     = Concurrent::AtomicBoolean.new(false)
          subject = Agent.new(0, error_mode: :fail)

          started  = Concurrent::Event.new
          releaser = Concurrent::CountDownLatch.new(1)
          failing  = subject.send_latest(:a) { started.set; releaser.wait(5); raise 'boom' }
          pending  = subject.send_latest(:b) { ran.value = true; 42 }

          expect(started.wait(2)).to be true
          releaser.count_down

          expect(pending.wait(1)).to be true
          expect(failing).to be_rejected
          expect(pending).to be_rejected
          expect(pending.reason).to be_a StandardError
          expect(ran.true?).to be false
          expect(subject.value).to eq 0
        end

        it 'rejects new dispatches made while failed without blocking the caller' do
          subject = Agent.new(0, error_mode: :fail)
          subject.send_via(immediate) { raise 'boom' }
          receipt = subject.send_latest(:a) { 42 }
          expect(receipt).to be_complete
          expect(receipt).to be_rejected
          expect(receipt.reason).to be_a StandardError
        end
      end

      context 'when the action raises in :continue mode' do

        it 'rejects only the failing receipt and keeps processing' do
          subject = Agent.new(0, error_mode: :continue)
          bad  = subject.send_latest_via(immediate, :a) { raise 'boom' }
          good = subject.send_latest_via(immediate, :a) { 42 }
          expect(bad).to be_rejected
          expect(good).to be_fulfilled
          expect(good.value).to eq 42
          expect(subject).to_not be_failed
        end
      end

      context 'on restart' do

        it 'settles held receipts as rejected when the queue is cleared' do
          ran      = Concurrent::AtomicBoolean.new(false)
          started  = Concurrent::Event.new
          releaser = Concurrent::CountDownLatch.new(1)
          subject  = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { started.set; releaser.wait(5); raise 'boom' }
          held = subject.send_latest(:a) { ran.value = true; 42 }
          expect(started.wait(2)).to be true
          releaser.count_down
          expect(held.wait(1)).to be true
          expect(held).to be_rejected
          expect(subject).to be_failed

          subject.restart(7, clear_actions: true)
          subject.await_for(1)

          expect(held).to be_rejected
          expect(ran.true?).to be false
          expect(subject.value).to eq 7
        end

        it 'keeps held receipts rejected when actions are retained and re-run' do
          started  = Concurrent::Event.new
          releaser = Concurrent::CountDownLatch.new(1)
          subject  = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { started.set; releaser.wait(5); raise 'boom' }
          held = subject.send_latest(:a) { 42 }
          expect(started.wait(2)).to be true
          releaser.count_down
          expect(held.wait(1)).to be true
          expect(held).to be_rejected

          subject.restart(7, clear_actions: false)
          subject.await_for(1)

          # the retained action still runs and updates the value ...
          expect(subject.value).to eq 42
          # ... but its receipt ended when the agent failed and is not reborn
          expect(held).to be_rejected
          expect(held.reason).to be_a StandardError
        end

        it 'a superseded receipt stays superseded across a retained restart' do
          started  = Concurrent::Event.new
          releaser = Concurrent::CountDownLatch.new(1)
          subject  = Agent.new(0, error_mode: :fail)

          subject.send_via(executor) { started.set; releaser.wait(5); raise 'boom' }
          expect(started.wait(2)).to be true
          held_old = subject.send_latest(:a, :old) { |_, p| p }
          held_new = subject.send_latest(:a, :new) { |_, p| p }
          expect(held_old).to be_superseded

          releaser.count_down
          expect(held_new.wait(1)).to be true
          expect(held_new).to be_rejected

          subject.restart(0, clear_actions: false)
          subject.await_for(1)

          expect(held_old).to be_superseded
          expect(held_old.reason).to be_a Agent::SupersededError
          expect(held_new).to be_rejected
        end
      end
    end
  end
end
