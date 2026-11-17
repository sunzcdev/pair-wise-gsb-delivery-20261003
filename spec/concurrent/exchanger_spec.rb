require 'concurrent/exchanger'
require 'concurrent/atomic/atomic_fixnum'

RSpec.shared_examples 'exchanger method with indefinite timeout' do

  before(:each) do
    subject # ensure proper initialization
  end

  it 'blocks indefinitely' do
    latch_1 = Concurrent::CountDownLatch.new
    latch_2 = Concurrent::CountDownLatch.new

    t = in_thread do
      latch_1.count_down
      subject.send(method_name, 0.1)
      latch_2.count_down
    end

    is_sleeping t
    expect(latch_1.count).to eq 0
    expect(latch_2.count).to eq 1
  end

  it 'receives the other value' do
    first_value  = nil
    second_value = nil
    latch        = Concurrent::CountDownLatch.new(2)

    in_thread { first_value = subject.send(method_name, 2); latch.count_down }
    in_thread { second_value = subject.send(method_name, 4); latch.count_down }

    latch.wait(1)

    expect(get_value(first_value)).to eq 4
    expect(get_value(second_value)).to eq 2
  end

  it 'can be reused' do
    first_value  = nil
    second_value = nil
    latch_1      = Concurrent::CountDownLatch.new(2)
    latch_2      = Concurrent::CountDownLatch.new(2)

    in_thread { first_value = subject.send(method_name, 1); latch_1.count_down }
    in_thread { second_value = subject.send(method_name, 0); latch_1.count_down }

    latch_1.wait(1)

    in_thread { first_value = subject.send(method_name, 10); latch_2.count_down }
    in_thread { second_value = subject.send(method_name, 12); latch_2.count_down }

    latch_2.wait(1)
    expect(get_value(first_value)).to eq 12
    expect(get_value(second_value)).to eq 10
  end
end

RSpec.shared_examples 'exchanger method with finite timeout' do

  it 'blocks until timeout' do
    duration = Concurrent::TestHelpers.monotonic_interval do
      begin
        subject.send(method_name, 2, 0.1)
      rescue Concurrent::TimeoutError
        # do nothing
      end
    end
    expect(duration).to be_within(0.05).of(0.1)
  end

  it 'receives the other value' do
    first_value  = nil
    second_value = nil
    latch        = Concurrent::CountDownLatch.new(2)

    in_thread { first_value = subject.send(method_name, 2, 1); latch.count_down }
    in_thread { second_value = subject.send(method_name, 4, 1); latch.count_down }

    latch.wait(1)

    expect(get_value(first_value)).to eq 4
    expect(get_value(second_value)).to eq 2
  end

  it 'can be reused' do
    first_value  = nil
    second_value = nil
    latch_1      = Concurrent::CountDownLatch.new(2)
    latch_2      = Concurrent::CountDownLatch.new(2)

    in_thread { first_value = subject.send(method_name, 1, 1); latch_1.count_down }
    in_thread { second_value = subject.send(method_name, 0, 1); latch_1.count_down }

    latch_1.wait(1)

    in_thread { first_value = subject.send(method_name, 10, 1); latch_2.count_down }
    in_thread { second_value = subject.send(method_name, 12, 1); latch_2.count_down }


    latch_2.wait(1)
    expect(get_value(first_value)).to eq 12
    expect(get_value(second_value)).to eq 10
  end
end

RSpec.shared_examples 'exchanger method cross-thread interactions' do

  it 'when first, waits for a second' do
    first_value  = nil
    second_value = nil
    latch        = Concurrent::CountDownLatch.new(1)

    t1 = in_thread do
      first_value = subject.send(method_name, :foo, 1)
      latch.count_down
    end
    t1.join(0.1)

    second_value = subject.send(method_name, :bar, 0)
    latch.wait(1)

    expect(get_value(first_value)).to eq :bar
    expect(get_value(second_value)).to eq :foo
  end

  it 'allows multiple firsts to cancel if necessary' do
    first_value   = nil
    second_value  = nil
    cancels       = 3
    cancel_latch  = Concurrent::CountDownLatch.new(cancels)
    success_latch = Concurrent::CountDownLatch.new(1)

    threads = cancels.times.collect do
      in_thread do
        begin
          first_value = subject.send(method_name, :foo, 0.1)
        rescue Concurrent::TimeoutError
          # suppress
        ensure
          cancel_latch.count_down
        end
      end
    end

    threads.each { |t| t.join(1) }
    cancel_latch.wait(1)

    t1 = in_thread do
      first_value = subject.send(method_name, :bar, 1)
      success_latch.count_down
    end
    t1.join(0.1)

    second_value = subject.send(method_name, :baz, 0)
    success_latch.wait(1)

    expect(get_value(first_value)).to eq :baz
    expect(get_value(second_value)).to eq :bar
  end
end

RSpec.shared_examples :exchanger do

  context '#exchange' do
    let!(:method_name) { :exchange }

    def get_value(result)
      result
    end

    it_behaves_like 'exchanger method with indefinite timeout'
    it_behaves_like 'exchanger method with finite timeout'
    it_behaves_like 'exchanger method cross-thread interactions'
  end

  context '#exchange!' do
    let!(:method_name) { :exchange! }

    def get_value(result)
      result
    end

    it_behaves_like 'exchanger method with indefinite timeout'
    it_behaves_like 'exchanger method with finite timeout'
    it_behaves_like 'exchanger method cross-thread interactions'
  end

  context '#try_exchange' do
    let!(:method_name) { :try_exchange }

    def get_value(result)
      result.value
    end

    it_behaves_like 'exchanger method with indefinite timeout'
    it_behaves_like 'exchanger method with finite timeout'
    it_behaves_like 'exchanger method cross-thread interactions'
  end

  context '#exchange_result' do
    let!(:method_name) { :exchange_result }

    def get_value(result)
      result.value
    end

    it_behaves_like 'exchanger method with indefinite timeout'
    it_behaves_like 'exchanger method with finite timeout'
    it_behaves_like 'exchanger method cross-thread interactions'

    it 'reports a fulfilled result carrying the partner object by reference' do
      first         = Object.new
      second        = Object.new
      barrier       = Concurrent::CountDownLatch.new(1)
      done          = Concurrent::CountDownLatch.new(1)
      first_result = nil
      second_result = nil

      in_thread do
        barrier.wait
        second_result = subject.exchange_result(second, 5)
        done.count_down
      end

      barrier.count_down
      first_result = subject.exchange_result(first, 5)
      done.wait(5)

      expect(first_result).to be_fulfilled
      expect(second_result).to be_fulfilled
      expect(first_result.state).to eq :fulfilled
      expect(first_result.value).to equal second
      expect(second_result.value).to equal first
    end

    it 'reports an unmatched timeout and keeps ownership of the offered object' do
      mine   = Object.new
      result = subject.exchange_result(mine, 0.05)

      expect(result).to be_timeout
      expect(result.state).to eq :timeout
      expect(result.value).to be_nil

      # the object is still mine: it can be offered again and is handed
      # over by reference, exactly once
      barrier     = Concurrent::CountDownLatch.new(1)
      done        = Concurrent::CountDownLatch.new(1)
      peer_result = nil

      in_thread do
        barrier.wait
        peer_result = subject.exchange_result(:peer, 5)
        done.count_down
      end

      barrier.count_down
      retry_result = subject.exchange_result(mine, 5)
      done.wait(5)

      expect(retry_result).to be_fulfilled
      expect(retry_result.value).to eq :peer
      expect(peer_result).to be_fulfilled
      expect(peer_result.value).to equal mine
    end

    it 'resolves a simultaneous timeout and match at a single commit point' do
      20.times do
        exchanger     = described_class.new
        first         = Object.new
        second        = Object.new
        gate          = Concurrent::CountDownLatch.new(1)
        first_result  = nil
        second_result = nil

        t1 = in_thread { gate.wait; first_result = exchanger.exchange_result(first, 0.01) }
        t2 = in_thread { gate.wait; sleep(rand(0.02)); second_result = exchanger.exchange_result(second, 0.05) }
        gate.count_down
        [t1, t2].each { |t| t.join(5) }

        if first_result.fulfilled?
          # the match won the commit race: both sides observe the exchange
          expect(second_result).to be_fulfilled
          expect(first_result.value).to equal second
          expect(second_result.value).to equal first
        else
          # the timeout won the commit race: nobody may hold the other's
          # object; each side still owns the buffer it offered
          expect(second_result).not_to be_fulfilled
        end
      end
    end
  end
end

module Concurrent

  RSpec.describe RubyExchanger do

    it_behaves_like :exchanger

    context 'abnormal exit' do

      it 'does not leave a stale slot behind for the next exchange' do
        t = in_thread do
          begin
            subject.exchange_result(:doomed, 30)
          rescue RuntimeError
            # expected: interrupted by the test
          end
        end
        is_sleeping t
        t.raise RuntimeError
        t.join(5)

        # the next pair must match with each other, not with the stale node
        first_result  = nil
        second_result = nil
        latch         = Concurrent::CountDownLatch.new(2)

        in_thread { first_result = subject.exchange_result(:first, 5); latch.count_down }
        in_thread { second_result = subject.exchange_result(:second, 5); latch.count_down }
        latch.wait(5)

        expect(first_result).to be_fulfilled
        expect(second_result).to be_fulfilled
        expect(first_result.value).to eq :second
        expect(second_result.value).to eq :first
      end

      it 'wakes the matched waiter with a cancelled result when the partner aborts mid-handoff' do
        aborting = Class.new(RubyExchanger) do
          # aborts after removing the occupier from the slot but before
          # committing the handoff
          def fulfill(*)
            raise 'simulated abort'
          end
        end.new

        matched = Concurrent::CountDownLatch.new(1)
        result  = nil

        t = in_thread do
          result = aborting.exchange_result(:mine, 5)
          matched.count_down
        end
        is_sleeping t

        expect { aborting.exchange_result(:theirs, 5) }.to raise_error('simulated abort')
        matched.wait(5)

        expect(result).to be_cancelled
        expect(result.state).to eq :cancelled
        expect(result.value).to be_nil
      end
    end

    if Concurrent.on_cruby?

      specify 'stress test' do
        thread_count   = 100
        exchange_count = 100
        latch          = Concurrent::CountDownLatch.new(thread_count)

        good = Concurrent::AtomicFixnum.new(0)
        bad  = Concurrent::AtomicFixnum.new(0)
        ugly = Concurrent::AtomicFixnum.new(0)

        thread_count.times.collect do |i|
          in_thread do
            exchange_count.times do |j|
              begin
                result = subject.exchange!(i, 1)
                result == i ? ugly.up : good.up
              rescue Concurrent::TimeoutError
                bad.up
              end
            end
            latch.count_down
          end
        end

        latch.wait

        puts "Good: #{good.value}, Bad (timeout): #{bad.value}, Ugly: #{ugly.value}"
        expect(good.value + bad.value + ugly.value).to eq thread_count * exchange_count
        expect(ugly.value).to eq 0
      end

      specify 'ownership stress test: each offered object ends up owned by exactly one side' do
        thread_count   = 50
        exchange_count = 50
        latch          = Concurrent::CountDownLatch.new(thread_count)
        outcomes       = Queue.new

        threads = thread_count.times.collect do
          in_thread do
            exchange_count.times do
              offered = Object.new
              result  = subject.exchange_result(offered, 1)
              outcomes << [offered, result]
            end
            latch.count_down
          end
        end

        threads.each { |t| t.join(30) }
        latch.wait(30)

        pairs = []
        outcomes.size.times { pairs << outcomes.pop }

        fulfilled          = pairs.select { |_, result| result.fulfilled? }
        offered_by_winners = fulfilled.map(&:first)
        received           = fulfilled.map { |_, result| result.value }

        # exchanges commit in pairs
        expect(fulfilled.size.even?).to be true
        # every received object is an object some thread offered (passed by
        # reference, never copied, never fabricated)
        expect(received - pairs.map(&:first)).to be_empty
        # no object is received twice: exactly one new owner per object
        expect(received.uniq.size).to eq received.size
        # the set of objects taken by partners is exactly the set of objects
        # offered by the threads whose exchange committed
        expect(received).to match_array(offered_by_winners)
      end
    end
  end

  if Concurrent.on_jruby?
    RSpec.describe JavaExchanger do
      it_behaves_like :exchanger
    end
  end

  RSpec.describe Exchanger do

    context 'class hierarchy' do

      if Concurrent.on_jruby?
        it 'inherits from JavaExchanger' do
          expect(Exchanger.ancestors).to include(JavaExchanger)
        end
      else
        it 'inherits from RubyExchanger' do
          expect(Exchanger.ancestors).to include(RubyExchanger)
        end
      end
    end
  end
end
