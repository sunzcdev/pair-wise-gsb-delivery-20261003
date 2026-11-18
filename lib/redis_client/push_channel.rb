# frozen_string_literal: true

require "thread"

class RedisClient
  # RESP3 push frames (the `>` type) are not replies to any command. A
  # PushChannel receives them in arrival order, independently of the command
  # response stream, so that pipelines and regular calls keep returning results
  # in command order.
  #
  # A single PushChannel is shared by every connection of a client. It is
  # thread-safe and is driven entirely in-memory: pushes are handed to it while
  # a connection parses the wire, but user callbacks are always invoked outside
  # of the protocol read lock.
  class PushChannel
    class Error < RedisClient::Error
    end

    # Raised when a push cannot be buffered because the bounded buffer is full
    # and the channel is configured with the +:reject+ policy.
    class BufferOverflowError < Error
    end

    # Raised when an operation is attempted on a closed channel.
    class ChannelClosedError < Error
    end

    # Raised when a push callback fails and the channel uses the +:raise+
    # policy.
    class CallbackError < Error
    end

    # Raised when code running from a push callback tries to read from the very
    # connection that delivered the push, which would deadlock against the
    # protocol read lock.
    class ReentrantDispatchError < CallbackError
    end

    # Default number of pushes buffered while no subscriber is attached.
    DEFAULT_CAPACITY = 128

    # How to handle a push that cannot be delivered immediately:
    #
    # * +:reject+      - raise {BufferOverflowError}, terminating the connection
    #   read rather than silently dropping a push or a normal reply.
    # * +:drop_oldest+ - discard the oldest buffered push to make room for the
    #   newest one.
    OVERFLOW_POLICIES = %i[reject drop_oldest].freeze

    # How to handle an exception raised by a subscriber callback:
    #
    # * +:isolate+ - the exception is contained (and forwarded to +on_error+) so
    #   that the connection keeps reading further replies.
    # * +:raise+   - the exception is wrapped in {CallbackError} and re-raised
    #   from the connection read, terminating the command that was in flight.
    ERROR_POLICIES = %i[isolate raise].freeze

    attr_reader :capacity, :overflow_policy, :error_policy

    # @param capacity [Integer] maximum number of pushes kept while no
    #   subscriber is attached. Use 0 to reject/drop every push that arrives
    #   without a subscriber.
    # @param overflow_policy [Symbol] one of {OVERFLOW_POLICIES}.
    # @param error_policy [Symbol] one of {ERROR_POLICIES}.
    # @param on_error [Proc, nil] optional callable invoked with each exception
    #   isolated from a subscriber callback, as +on_error.call(error)+.
    def initialize(
      capacity: DEFAULT_CAPACITY,
      overflow_policy: :reject,
      error_policy: :isolate,
      on_error: nil
    )
      raise ArgumentError, "capacity must be an Integer >= 0" unless capacity.is_a?(Integer) && capacity >= 0
      unless OVERFLOW_POLICIES.include?(overflow_policy)
        raise ArgumentError, "unknown overflow_policy: #{overflow_policy.inspect}"
      end
      unless ERROR_POLICIES.include?(error_policy)
        raise ArgumentError, "unknown error_policy: #{error_policy.inspect}"
      end
      if on_error && !on_error.respond_to?(:call)
        raise ArgumentError, "on_error must respond to #call"
      end

      @capacity = capacity
      @overflow_policy = overflow_policy
      @error_policy = error_policy
      @on_error = on_error

      @mutex = Mutex.new
      @subscribers = []
      @buffer = []
      @waiting = []
      @closed = false
    end

    # Register a block called for every push, in arrival order. Returns the
    # channel itself so the call can be chained.
    def subscribe(&block)
      raise ArgumentError, "a block is required" unless block

      @mutex.synchronize do
        raise ChannelClosedError, "PushChannel is closed" if @closed
        @subscribers << block
      end
      self
    end

    # Remove a previously registered callback (or every callback when no block
    # is given).
    def unsubscribe(block = nil)
      @mutex.synchronize do
        if block
          @subscribers.delete(block)
        else
          @subscribers.clear
        end
      end
      self
    end

    # Pull the next push that was not consumed by a callback, blocking at most
    # +timeout+ seconds. Returns +nil+ on timeout. Raises {ChannelClosedError}
    # once the channel is closed and fully drained.
    def pop(timeout = nil)
      deadline = timeout ? RedisClient.now + timeout : nil

      @mutex.synchronize do
        loop do
          return @buffer.shift unless @buffer.empty?
          raise ChannelClosedError, "PushChannel is closed" if @closed

          remaining = deadline ? deadline - RedisClient.now : nil
          return nil if remaining && remaining <= 0

          @waiting << Thread.current
          begin
            @mutex.sleep(remaining)
          ensure
            @waiting.delete(Thread.current)
          end
        end
      end
    end

    def closed?
      @mutex.synchronize { @closed }
    end

    # Whether any thread is currently blocked in {#pop}. Mostly useful for
    # tests that need to deterministically trigger a wake-on-close.
    def waiting?
      @mutex.synchronize { !@waiting.empty? }
    end

    # Deliver a push frame. Called by a connection right after the push has been
    # parsed and the protocol read lock released, so callbacks may safely issue
    # their own commands on other connections. Returns true when the push was
    # delivered or buffered, false when it was dropped by the overflow policy.
    #
    # Raises {BufferOverflowError} under the +:reject+ overflow policy and
    # {CallbackError} under the +:raise+ error policy.
    def dispatch(push)
      callbacks = nil
      buffered = true
      @mutex.synchronize do
        raise ChannelClosedError, "PushChannel is closed" if @closed

        subscribers = @subscribers
        if subscribers.empty?
          buffered = buffer_locked(push)
        else
          callbacks = subscribers.dup
        end
      end

      if callbacks
        dispatch_to_subscribers(callbacks, push)
        true
      else
        buffered
      end
    end

    # Close the channel. Buffered pushes can still be drained with {#pop}, but
    # no further push is accepted and every thread waiting on {#pop} is woken
    # up.
    def close
      waiters = nil
      @mutex.synchronize do
        @closed = true
        @subscribers.clear
        waiters = @waiting.dup
        @waiting.clear
      end
      waiters&.each(&:wakeup)
      self
    end

    private

    # Must be called while holding @mutex. Returns true when the push was
    # buffered, false when it was dropped by the overflow policy, or raises
    # BufferOverflowError under the +:reject+ policy.
    def buffer_locked(push)
      if @capacity == 0 || @buffer.size >= @capacity
        case @overflow_policy
        when :reject
          raise BufferOverflowError,
            "PushChannel buffer is full (#{@capacity} pushes); no subscriber is consuming pushes"
        when :drop_oldest
          return false if @capacity == 0

          @buffer.shift
        end
      end

      @buffer << push
      @waiting.shift&.wakeup if @waiting.any?
      true
    end

    def dispatch_to_subscribers(callbacks, push)
      callbacks.each do |callback|
        begin
          callback.call(push)
        rescue Exception => error # rubocop:disable Lint/RescueException
          handle_callback_error(error)
        end
      end
    end

    def handle_callback_error(error)
      case @error_policy
      when :isolate
        @on_error&.call(error)
      when :raise
        if error.is_a?(CallbackError)
          raise error
        else
          # Preserve the original callback exception as the cause instead of
          # whatever exception happens to be active in the rescue chain.
          raise CallbackError, error.message, cause: error
        end
      end
    end
  end

  # A RESP3 push frame (`>` type). It behaves like the Array it wraps but keeps
  # its identity so it can never be mistaken for the reply of a pending command.
  class Push < Array
  end
end