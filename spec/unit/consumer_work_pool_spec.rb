# frozen_string_literal: true

require "bunny/consumer_work_pool"

describe Bunny::ConsumerWorkPool do

  # Waits up to `timeout` seconds for `block` to become truthy. Keeps these
  # specs deterministic without depending on a running RabbitMQ broker.
  def wait_for(timeout = 5)
    deadline = Time.now + timeout
    loop do
      result = yield
      return result if result
      raise "wait_for timed out after #{timeout}s" if Time.now > deadline
      sleep 0.01
    end
  end

  after(:each) do
    @pool.shutdown(true) if @pool && @pool.running?
  end

  describe "consumer affinity and ordering" do
    it "runs callbacks of one consumer strictly in submission order with several workers" do
      @pool = described_class.new(3)
      @pool.start
      order = []
      mutex = Mutex.new

      10.times do |i|
        @pool.submit(consumer_tag: "a") { mutex.synchronize { order << i } }
      end
      wait_for { order.length == 10 }

      expect(order).to eq((0..9).to_a)
    end

    it "runs different consumers concurrently while serialising each consumer" do
      @pool = described_class.new(3)
      @pool.start
      order_b = []
      mutex   = Mutex.new
      gate    = Queue.new
      saw_a   = Queue.new

      # b0 blocks while holding consumer b's single in-flight slot.
      @pool.submit(consumer_tag: "b") do
        gate.pop
        mutex.synchronize { order_b << 0 }
      end
      4.times do |i|
        @pool.submit(consumer_tag: "b") { mutex.synchronize { order_b << i + 1 } }
      end
      sleep 0.1

      # consumer a must not be blocked by the stuck consumer b
      @pool.submit(consumer_tag: "a") { saw_a << :done }
      expect(saw_a.pop).to eq(:done)

      gate << :release
      wait_for { order_b.length == 5 }
      expect(order_b).to eq((0..4).to_a)
    end

    it "never runs two blocks of the same consumer at once" do
      @pool = described_class.new(4)
      @pool.start
      concurrent = 0
      max_concurrent = 0
      mutex = Mutex.new

      20.times do
        @pool.submit(consumer_tag: "x") do
          mutex.synchronize do
            concurrent += 1
            max_concurrent = [max_concurrent, concurrent].max
          end
          sleep 0.005
          mutex.synchronize { concurrent -= 1 }
        end
      end
      @pool.shutdown(true)

      expect(max_concurrent).to eq(1)
    end

    it "spreads untagged work across workers" do
      @pool = described_class.new(2)
      @pool.start
      worker_ids = Queue.new

      8.times do
        @pool.submit do
          sleep 0.02
          worker_ids << Thread.current.object_id
        end
      end

      ids = Array.new(8) { worker_ids.pop }.uniq
      expect(ids.length).to be >= 2
    end
  end

  describe "#resize" do
    it "grows the pool while running and new workers take work" do
      @pool = described_class.new(1)
      @pool.start
      expect(@pool.threads.count(&:alive?)).to eq(1)

      @pool.resize(4)
      wait_for { @pool.threads.count(&:alive?) == 4 }
      expect(@pool.size).to eq(4)

      done = Queue.new
      4.times { @pool.submit { sleep 0.02; done << :ok } }
      expect(Array.new(4) { done.pop }.length).to eq(4)
    end

    it "updates the target size when resized before start" do
      @pool = described_class.new(1)
      @pool.resize(3)
      expect(@pool.size).to eq(3)

      @pool.start
      wait_for { @pool.threads.count(&:alive?) == 3 }
    end

    it "rejects sizes smaller than one" do
      @pool = described_class.new(1)
      expect { @pool.resize(0) }.to raise_error(ArgumentError)
    end

    it "lets draining workers finish their in-flight block on the original worker" do
      @pool = described_class.new(2)
      @pool.start
      owner_id = nil
      owner_mutex = Mutex.new
      release = Queue.new
      finished_on = Queue.new

      @pool.submit(consumer_tag: "c") do
        owner_mutex.synchronize { owner_id = Thread.current.object_id }
        release.pop
        finished_on << Thread.current.object_id
      end
      wait_for { !owner_id.nil? }

      # shrink while the block is stuck; the stuck worker must keep ownership
      @pool.resize(1)
      wait_for { @pool.threads.count(&:alive?) == 1 }

      release << :go
      expect(finished_on.pop).to eq(owner_id)
    end

    it "migrates queued blocks on shrink without breaking per-consumer order" do
      @pool = described_class.new(2)
      @pool.start
      sequence = []
      mutex = Mutex.new
      hold = Queue.new

      @pool.submit(consumer_tag: "c") do
        mutex.synchronize { sequence << :c0 }
        hold.pop
      end
      wait_for { mutex.synchronize { sequence.include?(:c0) } }

      4.times do |i|
        @pool.submit(consumer_tag: "c") { mutex.synchronize { sequence << :"c#{i + 1}" } }
      end
      @pool.submit(consumer_tag: "d") { mutex.synchronize { sequence << :d0 } }

      @pool.resize(1)
      wait_for { @pool.threads.count(&:alive?) == 1 }
      hold << :release
      @pool.shutdown(true)

      c_events = sequence.select { |event| event.to_s.start_with?("c") }
      expect(c_events).to eq([:c0, :c1, :c2, :c3, :c4])
      expect(sequence).to include(:d0)
    end

    it "preserves ordering and exactly-once processing across grow-then-shrink with a stuck first callback" do
      @pool = described_class.new(1)
      @pool.start
      processed = Hash.new(0)
      started   = Queue.new
      release   = Queue.new

      @pool.submit(consumer_tag: "stuck") do
        processed["stuck-0"] += 1
        started << :started
        release.pop
        processed["stuck-0-done"] += 1
      end
      started.pop

      3.times do |i|
        @pool.submit(consumer_tag: "stuck") { processed["stuck-#{i + 1}"] += 1 }
      end

      # grow while the first callback is still in flight
      @pool.resize(3)
      wait_for { @pool.threads.count(&:alive?) == 3 }

      # another consumer arriving during the resize window is scheduled on its own rules
      other_done = Queue.new
      @pool.submit(consumer_tag: "other") do
        processed["other-0"] += 1
        other_done << :ok
      end
      other_done.pop
      expect(processed["other-0"]).to eq(1)

      # shrink below the original size while the first callback remains stuck
      @pool.resize(1)
      wait_for { @pool.threads.count(&:alive?) == 1 }

      # the stuck block is still owned by its original worker and counted once
      expect(processed["stuck-0"]).to eq(1)

      release << :go
      @pool.shutdown(true)

      (1..3).each { |i| expect(processed["stuck-#{i}"]).to eq(1) }
      expect(processed["stuck-0-done"]).to eq(1)
      processed.each_value { |count| expect(count).to eq(1) }
    end

    it "keeps per-consumer order and exactly-once processing under random resizes" do
      @pool = described_class.new(2)
      @pool.start
      tags = (1..8).map { |i| "t#{i}" }
      total = 400
      counters = Hash.new(0)
      next_expected = Hash.new(0)
      mutex = Mutex.new
      errors = []

      producer = Thread.new do
        total.times do |i|
          tag = tags[i % tags.length]
          index = i / tags.length
          @pool.submit(consumer_tag: tag) do
            mutex.synchronize do
              unless next_expected[tag] == index
                errors << "#{tag}: expected #{next_expected[tag]} got #{index}"
              end
              next_expected[tag] += 1
              counters[tag] += 1
            end
          end
        end
      end

      resizer = Thread.new do
        25.times do
          @pool.resize([1, 2, 3, 4].sample)
          sleep 0.005
        end
      end

      producer.join
      resizer.join
      @pool.resize(2)
      @pool.shutdown(true)

      expect(errors).to be_empty
      expected = Hash.new(0)
      total.times { |i| expected[tags[i % tags.length]] += 1 }
      expected.each { |tag, count| expect(counters[tag]).to eq(count) }
    end
  end

  describe "#shutdown" do
    it "waits for in-flight and queued blocks when asked to wait for workers" do
      @pool = described_class.new(1)
      @pool.start
      done = []

      @pool.submit { sleep 0.1; done << :a }
      @pool.submit { done << :b }
      @pool.shutdown(true)

      expect(done).to eq([:a, :b])
      expect(@pool.threads.any?(&:alive?)).to eq(false)
    end

    it "is safe to call more than once" do
      @pool = described_class.new(1)
      @pool.start
      @pool.submit { :ok }

      expect { @pool.shutdown(true); @pool.shutdown(true) }.not_to raise_error
    end

    it "honours the shutdown timeout instead of hanging on a stuck block" do
      @pool = described_class.new(1, false, 0.1)
      @pool.start
      release = Queue.new
      @pool.submit { release.pop }

      started = Time.now
      @pool.shutdown(true)
      expect(Time.now - started).to be < 1.0
      release << :go
    end

    it "drops work submitted after shutdown" do
      @pool = described_class.new(1)
      @pool.start
      @pool.shutdown(true)

      ran = Queue.new
      @pool.submit { ran << :never }
      sleep 0.1
      expect(ran.empty?).to eq(true)
    end
  end

  describe "#pause and #resume" do
    it "does not dispatch while paused and continues after resume" do
      @pool = described_class.new(1)
      @pool.start
      @pool.pause

      ran = Queue.new
      @pool.submit { ran << :x }
      sleep 0.2
      expect(ran.empty?).to eq(true)

      @pool.resume
      expect(ran.pop).to eq(:x)
    end
  end

  describe "#backlog and #busy?" do
    it "count queued and in-flight work" do
      @pool = described_class.new(1)
      @pool.start
      release = Queue.new
      entered = Queue.new

      @pool.submit do
        entered << :in
        release.pop
      end
      entered.pop
      @pool.submit { :queued }

      expect(@pool.backlog).to eq(2)
      expect(@pool.busy?).to eq(true)

      release << :go
      wait_for { @pool.backlog.zero? }
      expect(@pool.busy?).to eq(false)
    end
  end
end
