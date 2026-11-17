# frozen_string_literal: true

require_relative 'body_proxy'

module Rack
  # Rack::Lock locks every request inside a mutex, so that every request
  # will effectively be executed synchronously.
  #
  # The +scope+ option controls how long the lock is held:
  #
  # [:call]
  #   The lock covers +app.call+ only.  It is released as soon as the
  #   application returns, before the response body has been sent.
  # [:close]
  #   The lock is held until the response body is closed.  This is the
  #   default, and preserves the historical behaviour of Rack::Lock.
  # [:response]
  #   The lock covers the whole response lifetime.  It is released when an
  #   enumerable body returns from +#each+, when a streaming body returns
  #   from +#call+, or when the body is closed before it has been consumed,
  #   whichever happens first.  This is useful for applications that only
  #   release shared state once the response body has been read.
  #
  #   use Rack::Lock, scope: :response
  #
  # The +:close+ and +:response+ scopes can outlive the call to +app.call+,
  # so the body may be consumed by a different thread than the one that
  # called the application.  Ruby's Mutex can only be unlocked by the thread
  # that locked it, therefore a Permit (which can be released from any
  # thread) is used when no mutex is given.  A Mutex supplied explicitly is
  # used as-is, and is only safe when it is locked and unlocked on the same
  # thread.  A Mutex cannot be used with <tt>scope: :response</tt> at all,
  # because releasing it usually happens on the consuming thread; trying to
  # do so raises an ArgumentError.
  class Lock
    # The scopes accepted by the +scope+ option.
    SCOPES = [:call, :close, :response].freeze

    # The default scope, which holds the lock until the response body is
    # closed.
    DEFAULT_SCOPE = :close

    def initialize(app, mutex = nil, scope: DEFAULT_SCOPE)
      unless SCOPES.include?(scope)
        raise ArgumentError, "scope must be one of #{SCOPES.map(&:inspect).join(', ')}, not #{scope.inspect}"
      end

      @app, @scope = app, scope
      @mutex = mutex || Permit.new
      @lease_based = @mutex.respond_to?(:acquire)

      if scope == :response && !@lease_based
        raise ArgumentError, "scope: :response requires a mutex that can be released from another thread, such as Rack::Lock::Permit, not #{@mutex.class}"
      end
    end

    def call(env)
      lease = acquire

      if @scope == :call
        begin
          @app.call(env)
        ensure
          release(lease)
        end
      else
        returned = false
        begin
          response = @app.call(env)
          body = response.pop
          response << wrap_body(body, lease)
          returned = true
          response
        ensure
          # If the application raised, threw, or returned a response that could
          # not be wrapped, the lock is released here.  Otherwise the body
          # owns the lease and releases it exactly once.
          release(lease) unless returned
        end
      end
    end

    private

    def acquire
      if @lease_based
        @mutex.acquire
      else
        @mutex.lock
        nil
      end
    end

    def release(lease)
      if lease
        lease.release
      else
        @mutex.unlock
      end
    end

    def wrap_body(body, lease)
      case @scope
      when :response
        ResponseBodyProxy.new(body) { release(lease) }
      else
        BodyProxy.new(body) { release(lease) }
      end
    end

    # A mutual exclusion permit that can be released by a thread other than
    # the one that acquired it.
    #
    # Ruby's Mutex must be unlocked by the thread that locked it, which does
    # not work when the response body is generated in one thread and consumed
    # (and therefore released) in another.  A Permit is acquired with
    # #acquire, which returns a Lease; the lease can be released from any
    # thread.
    #
    # Waiters are admitted first-in, first-out.  A thread that starts waiting
    # is admitted before any thread that calls #acquire later, so a stream of
    # new arrivals cannot starve an existing waiter.  An interrupted waiter is
    # removed from the queue and the remaining waiters are woken up.
    #
    # A Permit also implements the same-thread Mutex interface (#lock,
    # #unlock, #try_lock, #synchronize, #locked? and #owned?), so it can be
    # used wherever a Mutex is expected as long as lock and unlock happen on
    # the same thread.
    class Permit
      # The right to hold a Permit, returned by Permit#acquire.
      #
      # A lease is released at most once, from any thread.  Releasing a lease
      # that was already released, or that is no longer the active lease, does
      # nothing and returns +false+, so a stale release can never unlock a
      # later holder.
      class Lease
        # The thread that acquired the lease.
        attr_reader :owner

        def initialize(permit)
          @permit = permit
          @owner = Thread.current
          @released = false
        end

        # Release the permit.  Returns +true+ if this call released the
        # permit, or +false+ if the lease was already released or is stale.
        def release
          @permit.release_lease(self)
        end

        # Whether this lease has been released.
        def released?
          @released
        end

        # :nodoc:
        #
        # Mark the lease as released.  Returns +true+ only for the first call.
        # The permit calls this while holding its internal mutex.
        def claim!
          return false if @released
          @released = true
        end
      end

      def initialize
        @mutex = Mutex.new
        @condition = ConditionVariable.new
        @lease = nil
        @next_ticket = 0
        @waiting = []
      end

      # Acquire the permit, blocking while it is held or while earlier callers
      # are still waiting.  Returns the Lease that must be released.
      def acquire
        @mutex.synchronize do
          if @lease && @lease.owner.equal?(Thread.current)
            raise ThreadError, "deadlock; recursive locking"
          end

          if @lease.nil? && @waiting.empty?
            return @lease = Lease.new(self)
          end

          ticket = (@next_ticket += 1)
          @waiting << ticket
          lease = nil
          begin
            @condition.wait(@mutex) until @waiting.first == ticket && @lease.nil?
            @waiting.shift
            lease = @lease = Lease.new(self)
          ensure
            # Thread#kill cannot be rescued, so the queue is cleaned up in an
            # ensure block.  An interrupted waiter must not keep its place, or
            # every later waiter would be blocked behind it forever.  After a
            # successful acquisition +lease+ is set and this is a no-op.
            unless lease
              @waiting.delete(ticket)
              # Only reachable if the thread was killed after storing the
              # lease but before assigning the local variable.
              @lease = nil if @lease && @lease.owner.equal?(Thread.current)
              @condition.broadcast
            end
          end

          lease
        end
      end

      # Try to acquire the permit without blocking.  Returns a Lease, or +nil+
      # if the permit is held or other callers are already waiting.
      def try_acquire
        @mutex.synchronize do
          @lease = Lease.new(self) if @lease.nil? && @waiting.empty?
        end
      end

      # Acquire the permit for the current thread, blocking until available.
      # Raises ThreadError if the current thread already holds it.
      def lock
        acquire
        self
      end

      # Release the permit held by the current thread.  Raises ThreadError if
      # the current thread does not hold it.
      def unlock
        lease = @mutex.synchronize do
          @lease if @lease && @lease.owner.equal?(Thread.current)
        end

        unless lease && lease.release
          raise ThreadError, "Attempt to unlock a Permit which is not locked by the current thread"
        end

        self
      end

      # Try to acquire the permit for the current thread without blocking.
      def try_lock
        !try_acquire.nil?
      end

      # Acquire the permit for the duration of the block.
      def synchronize
        lease = acquire
        begin
          yield
        ensure
          lease.release
        end
      end

      # Whether the permit is currently held.
      def locked?
        @mutex.synchronize { !@lease.nil? }
      end

      # Whether the given thread (the current thread by default) acquired the
      # active lease.
      def owned?(thread = Thread.current)
        @mutex.synchronize { !@lease.nil? && @lease.owner.equal?(thread) }
      end

      # The number of callers waiting for the permit, not including the
      # current holder.
      def waiting_count
        @mutex.synchronize { @waiting.size }
      end

      # Whether any callers are waiting for the permit.
      def waiting?
        waiting_count > 0
      end

      # :nodoc:
      #
      # Release the given lease if it is still the active one.  Returns
      # +true+ if the permit was released.
      def release_lease(lease)
        @mutex.synchronize do
          return false unless lease.claim!
          return false unless @lease.equal?(lease)

          @lease = nil
          @condition.broadcast
          true
        end
      end
    end

    # Wraps a response body so that the lock is released once the response has
    # been fully consumed, or when the body is closed without having been
    # consumed.  The lock is released exactly once, whether the body is
    # enumerable (+#each+), streaming (+#call+), converted with +#to_ary+, or
    # discarded before iteration starts.
    class ResponseBodyProxy < BodyProxy # :nodoc:
      BODY_METHODS = {each: true, call: true, to_ary: true}.freeze

      def initialize(body, &release)
        @on_release = release
        @release_mutex = Mutex.new
        @released = false
        super(body) { release_lock }
      end

      def each
        unless @body.respond_to?(:each)
          release_lock
          raise NoMethodError, "undefined method 'each' for #{@body.class} (use #call for a streaming body)"
        end

        return enum_for(:each) unless block_given?

        begin
          @body.each { |chunk| yield chunk }
        ensure
          release_lock
        end
      end

      def call(stream)
        unless @body.respond_to?(:call)
          release_lock
          raise NoMethodError, "undefined method 'call' for #{@body.class} (use #each for an enumerable body)"
        end

        begin
          @body.call(stream)
        ensure
          release_lock
        end
      end

      def to_ary
        unless @body.respond_to?(:to_ary)
          release_lock
          raise NoMethodError, "undefined method 'to_ary' for #{@body.class}"
        end

        @body.to_ary
      ensure
        # A body that responds to both #to_ary and #close must close itself
        # when converted, which also releases the lock.
        close
      end

      def close
        super
      ensure
        release_lock
      end

      def respond_to?(method_name, include_all = false)
        if BODY_METHODS.key?(method_name)
          @body.respond_to?(method_name, include_all)
        else
          super
        end
      end

      private

      # Release the lock at most once.  The body may be consumed and closed
      # from different threads, so the check and the assignment are guarded.
      def release_lock
        release = @release_mutex.synchronize do
          next false if @released
          @released = true
        end

        @on_release.call if release
      end
    end
  end
end
