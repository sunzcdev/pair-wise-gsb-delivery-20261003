# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)
require "rake/resource_manager"

class TestRakeResourceManager < Rake::TestCase # :nodoc:
  include Rake

  def setup
    super
    @mgr = ResourceManager.new
    @mgr.declare(:compiler, 2)
    @mgr.declare(:linker, 1)
  end

  def test_declare_requires_positive_integer_capacity
    assert_raises(ArgumentError) { @mgr.declare(:res, 0) }
    assert_raises(ArgumentError) { @mgr.declare(:res, -1) }
    assert_raises(ArgumentError) { @mgr.declare(:res, 1.5) }
    assert_raises(ArgumentError) { @mgr.declare(:res, "two") }
  end

  def test_declare_accepts_string_names
    @mgr.declare("asm", 3)
    assert_equal 3, @mgr.limit(:asm)
    assert_equal 3, @mgr.available("asm")
  end

  def test_normalize
    assert_equal({}, ResourceManager.normalize(nil))
    assert_equal({ compiler: 1 }, ResourceManager.normalize("compiler" => 1))
    assert_raises(InvalidResourceRequirement) do
      ResourceManager.normalize([:compiler])
    end
  end

  def test_validate_rejects_unknown_resource
    ex = assert_raises(InvalidResourceRequirement) do
      @mgr.validate!(unknown: 1)
    end
    assert_match(/unknown resource/i, ex.message)
  end

  def test_validate_rejects_non_positive_amounts
    assert_raises(InvalidResourceRequirement) { @mgr.validate!(compiler: 0) }
    assert_raises(InvalidResourceRequirement) { @mgr.validate!(compiler: -2) }
    assert_raises(InvalidResourceRequirement) { @mgr.validate!(compiler: 1.5) }
    assert_raises(InvalidResourceRequirement) { @mgr.validate!(compiler: nil) }
  end

  def test_validate_rejects_amount_exceeding_capacity
    ex = assert_raises(InvalidResourceRequirement) do
      @mgr.validate!(compiler: 3)
    end
    assert_match(/exceeds/, ex.message)
  end

  def test_validate_accepts_legal_requirements
    assert_equal(
      { compiler: 2, linker: 1 },
      @mgr.validate!(compiler: 2, linker: 1))
    assert_equal({}, @mgr.validate!({}))
    assert_equal({}, @mgr.validate!(nil))
  end

  def test_acquire_and_release_roundtrip
    @mgr.acquire(compiler: 2, linker: 1)
    assert_equal 0, @mgr.available(:compiler)
    assert_equal 0, @mgr.available(:linker)
    @mgr.release(compiler: 2, linker: 1)
    assert_equal 2, @mgr.available(:compiler)
    assert_equal 1, @mgr.available(:linker)
  end

  def test_acquisition_is_atomic_no_partial_hold_while_waiting
    @mgr.acquire(linker: 1) # main thread holds the only linker quota

    entered = Queue.new
    waiter = Thread.new do
      entered << true
      @mgr.acquire(compiler: 1, linker: 1)
    end
    entered.pop
    sleep 0.1 # let the waiter block on the missing linker quota

    # The waiter must hold nothing while waiting: both compiler quotas
    # are still available.
    assert_equal 2, @mgr.available(:compiler)
    assert_equal 0, @mgr.available(:linker)

    @mgr.release(linker: 1)
    waiter.join(5)
    refute waiter.alive?, "waiter should have been granted its quotas"
    assert_equal 1, @mgr.available(:compiler)
    assert_equal 0, @mgr.available(:linker)
  end

  def test_admission_is_strictly_fifo
    @mgr.acquire(compiler: 2) # main thread holds everything

    admitted = []
    mu = Mutex.new
    threads = 3.times.map do |i|
      Thread.new do
        sleep 0.05 * i # stagger the arrival order: 0, 1, 2
        @mgr.with_resources(compiler: 1) do
          mu.synchronize { admitted << i }
        end
      end
    end
    sleep 0.3 # give every waiter a chance to queue up

    @mgr.release(compiler: 2)
    threads.each { |t| t.join(5) }

    assert_equal [0, 1, 2], admitted
  end

  def test_later_requests_never_overtake_an_earlier_one
    # Starvation rule: a large request at the head of the queue blocks
    # later (smaller) requests until it can be fully satisfied, so it
    # is guaranteed to be admitted once the quota is released.
    @mgr.acquire(compiler: 2) # main thread holds everything

    admitted = []
    mu = Mutex.new
    big = Thread.new do
      @mgr.with_resources(compiler: 2) do
        mu.synchronize { admitted << :big }
      end
    end
    sleep 0.1 # big queues first
    small = Thread.new do
      @mgr.with_resources(compiler: 1) do
        mu.synchronize { admitted << :small }
      end
    end
    sleep 0.1 # small queues behind big

    @mgr.release(compiler: 1) # only half of what big needs
    sleep 0.2
    assert_equal [], admitted,
      "small request must not overtake the older big request"

    @mgr.release(compiler: 1)
    big.join(5)
    small.join(5)
    assert_equal [:big, :small], admitted
  end

  def test_with_resources_releases_on_error
    assert_raises(RuntimeError) do
      @mgr.with_resources(compiler: 2, linker: 1) { raise "boom" }
    end
    assert_equal 2, @mgr.available(:compiler)
    assert_equal 1, @mgr.available(:linker)
  end

  def test_with_resources_releases_when_thread_is_killed
    entered = Queue.new
    victim = Thread.new do
      @mgr.with_resources(compiler: 1, linker: 1) do
        entered << true
        sleep 30
      end
    end
    entered.pop # wait until the quotas are held
    assert_equal 0, @mgr.available(:linker)

    victim.kill
    victim.join
    assert_equal 2, @mgr.available(:compiler)
    assert_equal 1, @mgr.available(:linker)
  end

  def test_cancelled_waiter_leaves_the_queue
    @mgr.acquire(linker: 1)

    entered = Queue.new
    waiter = Thread.new do
      entered << true
      @mgr.acquire(linker: 1)
    end
    entered.pop
    sleep 0.1
    waiter.kill
    waiter.join

    # The cancelled waiter must not block later requests.
    @mgr.release(linker: 1)
    @mgr.acquire(linker: 1)
    assert_equal 0, @mgr.available(:linker)
  end

  def test_illegal_requirement_is_rejected_without_waiting
    @mgr.acquire(compiler: 2) # everything is busy
    assert_raises(InvalidResourceRequirement) do
      @mgr.acquire(compiler: 3) # would wait forever if not rejected
    end
    assert_raises(InvalidResourceRequirement) do
      @mgr.acquire(unknown: 1)
    end
  end

  def test_empty_requirements_never_block
    @mgr.acquire(compiler: 2, linker: 1) # everything is busy
    assert_equal({}, @mgr.acquire({}))
    assert_equal({}, @mgr.acquire(nil))
  end
end
