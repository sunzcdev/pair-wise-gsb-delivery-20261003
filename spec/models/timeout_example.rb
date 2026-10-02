class TimeoutExample
  include AASM

  attr_reader :clock

  def initialize(clock = nil)
    @clock = clock
  end

  # Manual monotonic clock: when present it fully determines both how deadline
  # anchors are read and the "now" passed to advances.
  def aasm_monotonic_now
    clock ? clock.now : super
  end

  aasm do
    state :waiting, initial: true, timeout: 5, timeout_event: :expire
    state :running, timeout: 0
    state :completed
    state :stuck

    event :start do
      transitions from: :waiting, to: :running
    end

    event :expire do
      transitions from: :waiting, to: :completed
      transitions from: :running, to: :stuck
    end
  end
end

class TimeoutWithoutEventExample
  include AASM

  attr_reader :clock

  def initialize(clock = nil)
    @clock = clock
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  aasm do
    # Default timeout event name is :expire.
    state :waiting, initial: true, timeout: 3
    state :completed

    event :expire do
      transitions from: :waiting, to: :completed
    end
  end
end

class TimeoutGuardedExample
  include AASM

  attr_accessor :allow_expire, :clock

  def initialize(clock = nil)
    @clock = clock
    @allow_expire = false
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  aasm do
    state :waiting, initial: true, timeout: 10
    state :completed

    event :expire, guard: :allow_expire do
      transitions from: :waiting, to: :completed
    end
  end
end

class TimeoutQuietGuardedExample
  include AASM

  attr_accessor :allow_expire, :clock

  def initialize(clock = nil)
    @clock = clock
    @allow_expire = false
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  aasm whiny_transitions: false do
    state :waiting, initial: true, timeout: 10
    state :completed

    event :expire, guard: :allow_expire do
      transitions from: :waiting, to: :completed
    end
  end
end

class TimeoutRaisingExample
  include AASM

  attr_reader :clock

  def initialize(clock = nil)
    @clock = clock
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  def boom
    raise StandardError, 'boom in callback'
  end

  aasm do
    state :waiting, initial: true, timeout: 10
    state :completed

    event :expire, guard: :boom do
      transitions from: :waiting, to: :completed
    end
  end
end

class TimeoutMissingEventExample
  include AASM

  attr_reader :clock

  def initialize(clock = nil)
    @clock = clock
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  aasm do
    state :waiting, initial: true, timeout: 10
    state :completed
    # no :expire event defined
  end
end

class TimeoutAfterRaisesExample
  include AASM

  attr_reader :clock

  def initialize(clock = nil)
    @clock = clock
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  def boom
    raise StandardError, 'boom in after callback'
  end

  aasm do
    state :waiting, initial: true, timeout: 10
    state :completed, timeout: 10

    event :expire, after: :boom do
      transitions from: :waiting, to: :completed
    end
  end
end

class TimeoutSelfTransitionExample
  include AASM

  attr_reader :clock

  def initialize(clock = nil)
    @clock = clock
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  aasm do
    state :waiting, initial: true, timeout: 5

    event :poke do
      transitions from: :waiting, to: :waiting
    end

    event :expire do
      transitions from: :waiting, to: :waiting
    end
  end
end

class TimeoutNamedMachineExample
  include AASM

  attr_reader :clock

  def initialize(clock = nil)
    @clock = clock
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  aasm(:work) do
    state :waiting, initial: true, timeout: 2
    state :completed

    event :expire do
      transitions from: :waiting, to: :completed
    end
  end
end

class TimeoutZeroWaitChainExample
  include AASM

  attr_reader :clock

  def initialize(clock = nil)
    @clock = clock
  end

  def aasm_monotonic_now
    clock ? clock.now : super
  end

  aasm do
    state :waiting, initial: true, timeout: 5
    state :running, timeout: 0
    state :done

    event :expire do
      transitions from: :waiting, to: :running
      transitions from: :running, to: :done
    end
  end
end

class TimeoutReentryExample
  include AASM

  attr_accessor :t

  def initialize
    @t = 0.0
  end

  def aasm_monotonic_now
    @t
  end

  aasm do
    state :waiting, initial: true, timeout: 5
    state :other

    event :leave do
      transitions from: :waiting, to: :other
    end

    event :come_back do
      transitions from: :other, to: :waiting
    end

    event :expire do
      transitions from: :waiting, to: :other
    end
  end
end

class TimeoutInlineSelfTransitionExample
  include AASM

  attr_accessor :t

  def initialize
    @t = 0.0
  end

  def aasm_monotonic_now
    @t
  end

  aasm do
    state :waiting, initial: true, timeout: 5

    event :poke do
      transitions from: :waiting, to: :waiting
    end

    event :expire do
      transitions from: :waiting, to: :waiting
    end
  end
end

class TimeoutExtraArgsExample
  include AASM

  attr_accessor :seen

  def aasm_monotonic_now
    0.0
  end

  aasm do
    state :waiting, initial: true, timeout: 1
    state :done

    event :expire do
      transitions from: :waiting, to: :done
    end
  end
end

class TimeoutKeywordArgumentsExample
  include AASM

  attr_reader :received_key, :received_args

  def aasm_monotonic_now
    0.0
  end

  aasm do
    state :waiting, initial: true, timeout: 1
    state :done

    event :expire, guard: :capture do
      transitions from: :waiting, to: :done
    end
  end

  def capture(*args, key:, **rest)
    @received_args = args
    @received_key = key
    true
  end
end

# Minimal manual monotonic clock for deterministic tests.
class ManualMonotonicClock
  attr_accessor :now

  def initialize(start = 100.0)
    @now = start.to_f
  end

  def advance(seconds)
    @now += seconds
  end
end
