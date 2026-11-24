# frozen_string_literal: true

require "timeout"
require "bunny/exceptions"
require "bunny/timestamp"

module Bunny
  # Cooperative cancellation token for a publish waiting on a {PublishGate}.
  #
  # Pass one as the +:cancellation+ option of {Bunny::Channel#basic_publish}
  # and call {#cancel} from any other thread to abandon the wait. A token can
  # only be cancelled once.
  #
  # @api public
  class CancellationToken
    def initialize
      @mutex     = Mutex.new
      @cancelled = false
      @reason    = nil
      @callbacks = []
    end

    # @return [Boolean] true if {#cancel} was called
    def cancelled?
      @mutex.synchronize { @cancelled }
    end
    alias canceled? cancelled?

    # @return [Object] reason passed to {#cancel}
    def reason
      @mutex.synchronize { @reason }
    end

    # Cancels the token and wakes every wait registered with it.
    # @return [Boolean] true for the call that actually cancelled it
    def cancel(reason = :cancelled)
      should_notify = @mutex.synchronize do
        return false if @cancelled

        @cancelled = true
        @reason    = reason
        true
      end
      return false unless should_notify

      callbacks = @mutex.synchronize { @callbacks.dup }
      callbacks.each do |callback|
        callback.call(self)
      rescue StandardError
        # a cancellation observer must never break the cancelling thread
      end
      true
    end
    alias cancel! cancel

    # Registers a callback invoked on cancellation, or immediately when the
    # token is already cancelled.
    # @api private
    def on_cancel(&block)
      raise ArgumentError, "a block is required" unless block

      invoke_now = @mutex.synchronize do
        if @cancelled
          true
        else
          @callbacks << block
          false
        end
      end
      block.call(self) if invoke_now
      self
    end

    # Removes a callback previously registered with {#on_cancel}.
    # @api private
    def off_cancel(callback)
      @mutex.synchronize { @callbacks.delete(callback) }
      self
    end
  end

  # Connection-wide admission gate for publishes while the broker has flagged
  # the connection as blocked (connection.blocked).
  #
  # The gate only parks publisher threads before their frames are handed to the
  # transport. It never holds the transport write lock or a session-wide mutex
  # while waiting, so consumer acknowledgements, heartbeats and the reader loop
  # (which delivers connection.unblocked) keep making progress.
  #
  # A wait can be bounded with a deadline and/or aborted with a
  # {CancellationToken}. On unblock only tickets that are still waiting are
  # admitted; a ticket whose deadline expired or that was cancelled is removed
  # first and can therefore never be transmitted afterwards.
  #
  # @api public
  class PublishGate
    # A single publisher admission attempt.
    class Ticket
      # @api private
      def initialize(gate, state, deadline, cancellation)
        @gate         = gate
        @state        = state
        @deadline     = deadline
        @cancellation = cancellation
      end

      attr_reader :deadline, :cancellation
      attr_accessor :state, :error, :cancellation_callback

      def waiting?
        @state == :waiting
      end

      def admitted?
        @state == :admitted
      end

      def cancelled?
        @state == :cancelled
      end
      alias canceled? cancelled?

      def timed_out?
        @state == :timed_out
      end

      def failed?
        @state == :failed
      end

      def terminal?
        @state != :waiting
      end
    end

    # Poll slice used for cancellation objects that do not expose {#on_cancel}.
    CANCELLATION_POLL_INTERVAL = 0.02

    def initialize(logger = nil)
      @logger   = logger
      @mutex    = Mutex.new
      @condition = ConditionVariable.new
      @blocked   = false
      @reason    = nil
      @closed    = false
      @failure   = nil
      @waiters   = []
    end

    # @return [Boolean] whether the gate currently refuses new admissions
    def blocked?
      @mutex.synchronize { @blocked }
    end

    # @return [Boolean] whether the gate was permanently failed ({#fail_all!})
    def closed?
      @mutex.synchronize { @closed }
    end

    # @return [Integer] number of publishes currently waiting for admission
    def waiting_count
      @mutex.synchronize { @waiters.size }
    end

    # @return [Array<Ticket>] live waiting tickets, in registration order
    def waiting_tickets
      @mutex.synchronize { @waiters.dup }
    end

    # Drives the gate into the blocked state. Called from Session frame handling.
    # @api private
    def block!(reason = nil)
      @mutex.synchronize do
        @blocked = true
        @reason  = reason
      end
    end

    # Admits every publish that is still waiting. Cancelled or timed out tickets
    # are no longer in the waiting set and are therefore never admitted.
    # @api private
    def unblock!
      tickets = @mutex.synchronize do
        @blocked = false
        @reason  = nil
        current  = @waiters
        @waiters = []
        current.each { |ticket| ticket.state = :admitted }
        @condition.broadcast
        current
      end
      tickets
    end

    # Permanently fails all waiting and future admissions, e.g. when the
    # connection is closed or lost while publishers are parked.
    # @api private
    def fail_all!(error = nil)
      error ||= Bunny::ConnectionAlreadyClosed.new
      tickets = @mutex.synchronize do
        @closed  = true
        @failure = error
        current  = @waiters
        @waiters = []
        current.each do |ticket|
          ticket.state = :failed
          ticket.error = error
        end
        @condition.broadcast
        current
      end
      tickets
    end
    alias shutdown! fail_all!

    # Clears blocked/closed state for a fresh connection generation. Any waiter
    # still registered is failed rather than left parked forever.
    # @api private
    def reset!(error = nil)
      error ||= Bunny::ConnectionAlreadyClosed.new
      @mutex.synchronize do
        @waiters.each do |ticket|
          next unless ticket.waiting?

          ticket.state = :failed
          ticket.error = error
        end
        @waiters.clear
        @blocked = false
        @reason  = nil
        @closed  = false
        @failure = nil
        @condition.broadcast
      end
    end

    # Blocks the caller until admitted, cancelled, the deadline expires or the
    # gate fails.
    #
    # @param timeout [Numeric, nil] maximum seconds to wait; nil waits forever
    # @param cancellation [CancellationToken, #cancelled?, nil]
    # @return [true] when admitted
    # @raise [PublishWaitTimeout] when the deadline expires
    # @raise [PublishCancelled] when cancelled
    # @raise [Bunny::Exception] the failure given to {#fail_all!}
    def wait_for_admission(timeout: nil, cancellation: nil)
      ticket = register(timeout, cancellation)
      begin
        await(ticket)
      ensure
        unsubscribe(ticket)
      end
    end
    alias admit wait_for_admission
    alias wait wait_for_admission
    alias wait_if_blocked wait_for_admission
    alias await_unblock wait_for_admission

    # Registers a ticket without blocking. Pair with {#await}.
    # @api private
    def register(timeout, cancellation)
      deadline = timeout ? Bunny::Timestamp.monotonic + timeout.to_f : nil

      @mutex.synchronize do
        raise @failure if @closed && @failure

        if cancellation&.cancelled?
          return Ticket.new(self, :cancelled, deadline, cancellation)
        end

        state = @blocked ? :waiting : :admitted
        ticket = Ticket.new(self, state, deadline, cancellation)
        @waiters << ticket if state == :waiting
        ticket
      end.tap do |ticket|
        next unless ticket.waiting? && cancellation

        if cancellation.respond_to?(:on_cancel)
          callback = ->(_token) { cancel(ticket) }
          ticket.cancellation_callback = callback
          cancellation.on_cancel(&callback)
        end
      end
    end

    # Waits for a ticket to reach a terminal state.
    # @api private
    def await(ticket)
      @mutex.synchronize do
        until ticket.terminal?
          raise @failure if @closed && @failure

          if ticket.cancellation&.cancelled?
            cancel_locked(ticket, :cancelled)
            next
          end

          now       = Bunny::Timestamp.monotonic
          remaining = ticket.deadline ? ticket.deadline - now : nil
          if remaining && remaining <= 0
            expire_locked(ticket)
            next
          end

          slice = wait_slice(ticket.cancellation, remaining)
          @condition.wait(@mutex, slice)
        end

        case ticket.state
        when :admitted then true
        when :cancelled
          raise PublishCancelled, "publish was cancelled before it was sent"
        when :timed_out
          raise PublishWaitTimeout, "timed out waiting for connection to unblock"
        when :failed
          raise ticket.error || Bunny::ConnectionAlreadyClosed.new
        end
      end
    end

    # Cancels a registered ticket if it is still waiting.
    # @return [Boolean] true when this call won the race and cancelled it
    # @api private
    def cancel(ticket)
      @mutex.synchronize { cancel_locked(ticket, :cancelled) }
    end

    private

    def unsubscribe(ticket)
      callback = ticket.cancellation_callback
      token    = ticket.cancellation
      return unless callback && token.respond_to?(:off_cancel)

      token.off_cancel(callback)
    end

    def wait_slice(cancellation, remaining)
      if cancellation
        [remaining, CANCELLATION_POLL_INTERVAL].compact.min
      else
        remaining
      end
    end

    def cancel_locked(ticket, _reason)
      return false unless ticket.waiting?

      ticket.state = :cancelled
      @waiters.delete(ticket)
      @condition.broadcast
      true
    end

    def expire_locked(ticket)
      ticket.state = :timed_out
      @waiters.delete(ticket)
      @condition.broadcast
    end
  end

  # Alternative name some clients use for {PublishGate}.
  BlockedPublishGate = PublishGate
end
