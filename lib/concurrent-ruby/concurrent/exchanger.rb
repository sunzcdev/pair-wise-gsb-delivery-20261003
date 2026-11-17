require 'concurrent/constants'
require 'concurrent/errors'
require 'concurrent/maybe'
require 'concurrent/atomic/atomic_reference'
require 'concurrent/atomic/count_down_latch'
require 'concurrent/utility/engine'
require 'concurrent/utility/monotonic_time'

module Concurrent

  # @!macro exchanger
  #
  #   A synchronization point at which threads can pair and swap elements within
  #   pairs. Each thread presents some object on entry to the exchange method,
  #   matches with a partner thread, and receives its partner's object on return.
  #
  #   @!macro thread_safe_variable_comparison
  #
  #   This implementation is very simple, using only a single slot for each
  #   exchanger (unlike more advanced implementations which use an "arena").
  #   This approach will work perfectly fine when there are only a few threads
  #   accessing a single `Exchanger`. Beyond a handful of threads the performance
  #   will degrade rapidly due to contention on the single slot, but the algorithm
  #   will remain correct.
  #
  #   @see http://docs.oracle.com/javase/7/docs/api/java/util/concurrent/Exchanger.html java.util.concurrent.Exchanger
  #   @example
  #
  #     exchanger = Concurrent::Exchanger.new
  #
  #     threads = [
  #       Thread.new { puts "first: " << exchanger.exchange('foo', 1) }, #=> "first: bar"
  #       Thread.new { puts "second: " << exchanger.exchange('bar', 1) } #=> "second: foo"
  #     ]
  #     threads.each {|t| t.join(2) }

  # @!visibility private
  class AbstractExchanger < Synchronization::Object

    # The explicit, identity-carrying outcome of an exchange attempt,
    # returned by {AbstractExchanger#exchange_result}.
    #
    # When reusable buffers are handed over through an {Exchanger} it must
    # always be unambiguous who owns which object afterwards. A `Result`
    # makes the commit point of the exchange observable:
    #
    # * `:fulfilled` - the exchange matched with a partner and committed.
    #   {#value} is the partner's object (the identical object, passed by
    #   reference, never copied). Ownership of the offered object has
    #   transferred to the partner.
    # * `:timeout` - no partner matched within the timeout. Nothing changed
    #   hands; the caller still owns the object it offered.
    # * `:cancelled` - a partner matched but aborted (e.g. abnormal exit)
    #   before the handoff committed. Nothing changed hands; the caller
    #   still owns the object it offered.
    #
    # A `:timeout` or `:cancelled` result therefore guarantees that no other
    # thread received the offered object, and a `:fulfilled` result
    # guarantees that the offered object was taken by exactly one partner.
    class Result

      # The exchange matched with a partner and committed.
      FULFILLED = :fulfilled

      # The exchange expired without matching a partner.
      TIMEOUT = :timeout

      # The exchange matched but the partner aborted before committing.
      CANCELLED = :cancelled

      # @!visibility private
      def self.fulfilled(value)
        new(FULFILLED, value)
      end

      # @!visibility private
      def self.timeout
        new(TIMEOUT, nil)
      end

      # @!visibility private
      def self.cancelled
        new(CANCELLED, nil)
      end

      # @return [Symbol] the outcome identity: `:fulfilled`, `:timeout`
      #   or `:cancelled`
      attr_reader :state

      # @return [Object, nil] the object handed over by the partner (the
      #   identical object, passed by reference) when {#fulfilled?};
      #   `nil` otherwise
      attr_reader :value

      # @!visibility private
      def initialize(state, value)
        @state = state
        @value = value
        freeze
      end
      private_class_method :new

      # @return [Boolean] true when the exchange committed and {#value}
      #   holds the partner's object
      def fulfilled?
        @state == FULFILLED
      end

      # @return [Boolean] true when the exchange expired unmatched; the
      #   offered object is still owned by the caller
      def timeout?
        @state == TIMEOUT
      end

      # @return [Boolean] true when a matched partner aborted before the
      #   handoff committed; the offered object is still owned by the caller
      def cancelled?
        @state == CANCELLED
      end
    end

    # @!visibility private
    CANCEL = ::Object.new
    private_constant :CANCEL

    def initialize
      super
    end

    # @!macro exchanger_method_do_exchange
    #
    #   Waits for another thread to arrive at this exchange point (unless the
    #   current thread is interrupted), and then transfers the given object to
    #   it, receiving its object in return. The timeout value indicates the
    #   approximate number of seconds the method should block while waiting
    #   for the exchange. When the timeout value is `nil` the method will
    #   block indefinitely.
    #
    #   @param [Object] value the value to exchange with another thread
    #   @param [Numeric, nil] timeout in seconds, `nil` blocks indefinitely
    #
    # @!macro exchanger_method_exchange
    #
    #   In some edge cases when a `timeout` is given a return value of `nil` may be
    #   ambiguous. Specifically, if `nil` is a valid value in the exchange it will
    #   be impossible to tell whether `nil` is the actual return value or if it
    #   signifies timeout. When `nil` is a valid value in the exchange consider
    #   using {#exchange!} or {#try_exchange} instead.
    #
    #   @return [Object] the value exchanged by the other thread or `nil` on timeout
    def exchange(value, timeout = nil)
      (value = do_exchange(value, timeout)) == CANCEL ? nil : value
    end

    # @!macro exchanger_method_do_exchange
    # @!macro exchanger_method_exchange_bang
    #
    #   On timeout a {Concurrent::TimeoutError} exception will be raised.
    #
    #   @return [Object] the value exchanged by the other thread
    #   @raise [Concurrent::TimeoutError] on timeout
    def exchange!(value, timeout = nil)
      if (value = do_exchange(value, timeout)) == CANCEL
        raise Concurrent::TimeoutError
      else
        value
      end
    end

    # @!macro exchanger_method_do_exchange
    # @!macro exchanger_method_try_exchange
    #
    #   The return value will be a {Concurrent::Maybe} set to `Just` on success or
    #   `Nothing` on timeout.
    #
    #   @return [Concurrent::Maybe] on success a `Just` maybe will be returned with
    #     the item exchanged by the other thread as `#value`; on timeout a
    #     `Nothing` maybe will be returned with {Concurrent::TimeoutError} as `#reason`
    #
    #   @example
    #
    #     exchanger = Concurrent::Exchanger.new
    #
    #     result = exchanger.exchange(:foo, 0.5)
    #
    #     if result.just?
    #       puts result.value #=> :bar
    #     else
    #       puts 'timeout'
    #     end
    def try_exchange(value, timeout = nil)
      if (value = do_exchange(value, timeout)) == CANCEL
        Concurrent::Maybe.nothing(Concurrent::TimeoutError)
      else
        Concurrent::Maybe.just(value)
      end
    end

    # @!macro exchanger_method_do_exchange
    # @!macro exchanger_method_exchange_result
    #
    #   Unlike {#exchange} the outcome carries an explicit identity, so a
    #   timeout (or a cancellation) racing with a match can never be
    #   mistaken for a completed exchange: the caller always knows whether
    #   it still owns the object it offered or whether that object was
    #   committed to a partner. Objects are passed by reference; large
    #   buffers are never copied.
    #
    #   @return [Result] the outcome of the attempt; `result.fulfilled?`
    #     means the exchange committed and `result.value` is the partner's
    #     object, `result.timeout?` means no partner matched in time, and
    #     `result.cancelled?` means a matched partner aborted before the
    #     handoff committed. For the two latter states the caller retains
    #     ownership of the object it offered.
    def exchange_result(value, timeout = nil)
      do_exchange_result(value, timeout)
    end

    private

    # @!macro exchanger_method_do_exchange
    #
    # @return [Object, CANCEL] the value exchanged by the other thread; {CANCEL} on timeout
    def do_exchange(value, timeout)
      raise NotImplementedError
    end

    # @!macro exchanger_method_do_exchange
    #
    # @return [Result] the explicit outcome of the exchange attempt
    def do_exchange_result(value, timeout)
      # Default implementation for engines which cannot observe
      # cancellation: an unmatched exchange is reported as a timeout.
      (value = do_exchange(value, timeout)) == CANCEL ? Result.timeout : Result.fulfilled(value)
    end
  end

  # @!macro internal_implementation_note
  # @!visibility private
  class RubyExchanger < AbstractExchanger
    # A simplified version of java.util.concurrent.Exchanger written by
    # Doug Lea, Bill Scherer, and Michael Scott with assistance from members
    # of JCP JSR-166 Expert Group and released to the public domain. It does
    # not include the arena or the multi-processor spin loops.
    # http://grepcode.com/file/repository.grepcode.com/java/root/jdk/openjdk/6-b14/java/util/concurrent/Exchanger.java

    safe_initialization!

    class Node < Concurrent::Synchronization::Object
      attr_atomic :value
      safe_initialization!

      def initialize(item)
        super()
        @Item      = item
        @Latch     = Concurrent::CountDownLatch.new
        self.value = nil
      end

      def latch
        @Latch
      end

      def item
        @Item
      end
    end
    private_constant :Node

    def initialize
      super
    end

    private

    attr_atomic(:slot)

    # @!macro exchanger_method_do_exchange
    #
    # @return [Object, CANCEL] the value exchanged by the other thread; {CANCEL} on timeout
    def do_exchange(value, timeout)
      result = do_exchange_result(value, timeout)
      result.fulfilled? ? result.value : CANCEL
    end

    # @!macro exchanger_method_do_exchange
    #
    # @return [Result] the explicit outcome of the exchange attempt
    def do_exchange_result(value, timeout)

      # ALGORITHM
      #
      # From the original Java version:
      #
      # > The basic idea is to maintain a "slot", which is a reference to
      # > a Node containing both an Item to offer and a "hole" waiting to
      # > get filled in.  If an incoming "occupying" thread sees that the
      # > slot is null, it CAS'es (compareAndSets) a Node there and waits
      # > for another to invoke exchange.  That second "fulfilling" thread
      # > sees that the slot is non-null, and so CASes it back to null,
      # > also exchanging items by CASing the hole, plus waking up the
      # > occupying thread if it is blocked.  In each case CAS'es may
      # > fail because a slot at first appears non-null but is null upon
      # > CAS, or vice-versa.  So threads may need to retry these
      # > actions.
      #
      # This version:
      #
      # An exchange occurs between an "occupier" thread and a "fulfiller" thread.
      # The "slot" is used to setup this interaction. The first thread in the
      # exchange puts itself into the slot (occupies) and waits for a fulfiller.
      # The second thread removes the occupier from the slot and attempts to
      # perform the exchange. Removing the occupier also frees the slot for
      # another occupier/fulfiller pair.
      #
      # Because the occupier and the fulfiller are operating independently and
      # because there may be contention with other threads, any failed operation
      # indicates contention. Both the occupier and the fulfiller operate within
      # spin loops. Any failed actions along the happy path will cause the thread
      # to repeat the loop and try again.
      #
      # When a timeout value is given the thread must be cognizant of time spent
      # in the spin loop. The remaining time is checked every loop. When the time
      # runs out the thread will exit.
      #
      # A "node" is the data structure used to perform the exchange. Only the
      # occupier's node is necessary. It's the node used for the exchange.
      # Each node has an "item," a "hole" (self), and a "latch." The item is the
      # node's initial value. It never changes. It's what the fulfiller returns on
      # success. The occupier's hole is where the fulfiller put its item. It's the
      # item that the occupier returns on success. The latch is used for synchronization.
      # Because a thread may act as either an occupier or fulfiller (or possibly
      # both in periods of high contention) every thread creates a node when
      # the exchange method is first called.
      #
      # COMMIT POINT
      #
      # The CAS on the occupier's hole (nil -> partner's value, performed by
      # the fulfiller, or nil -> CANCEL, performed by the occupier itself or
      # by an aborting partner) is the single commit point of an exchange.
      # A timeout and a match may race, but exactly one of them wins that
      # CAS, and both sides derive their outcome from it. It is therefore
      # impossible for both parties to walk away each believing it kept its
      # own object while also holding the other's.
      #
      # ABNORMAL EXIT
      #
      # Both roles clean up on abnormal exit (exception or kill):
      #
      #   * An occupier removes its node from the slot and cancels its hole,
      #     so the next exchange never inherits a stale slot and a racing
      #     fulfiller sees a cancelled hole instead of handing its value to
      #     a dead partner.
      #   * A fulfiller which already removed the occupier from the slot
      #     cancels the occupier's hole and wakes it, so the matched waiter
      #     is never left sleeping on a dead partner; it finishes with a
      #     cancelled result.
      #
      # The following steps occur within the spin loop. If any actions fail
      # the thread will loop and try again, so long as there is time remaining.
      # If time runs out the thread will exit with a timeout result.
      #
      # Check the slot for an occupier:
      #
      #   * If the slot is empty try to occupy
      #   * If the slot is full try to fulfill
      #
      # Attempt to occupy:
      #
      #   * Attempt to CAS myself into the slot
      #   * Go to sleep and wait to be woken by a fulfiller
      #   * If the sleep is successful my hole holds the outcome
      #     - The fulfiller's value: the exchange committed, return it
      #     - CANCEL: the partner aborted after matching, return cancelled
      #   * When the sleep fails (time ran out) attempt to cancel the operation
      #     - Attempt to CAS myself out of the slot
      #     - If successful there is no contention
      #       - Return timeout
      #     - On failure, I am competing with a fulfiller
      #       - Attempt to CAS my hole to CANCEL
      #       - On success
      #         - The fulfiller will see the cancelled hole and retry
      #         - Return timeout
      #       - On failure the fulfiller has committed the exchange
      #         - Return the value from my hole (the fulfiller's value)
      #
      # Attempt to fulfill:
      #
      # * Attempt to CAS the occupier out of the slot
      #   - On failure loop again
      # * Attempt to CAS my item into the occupier's hole (the commit point)
      #   - On failure the occupier cancelled or is cancelling
      #     - Loop again
      #   - On success we are on the happy path
      #     - Wake the sleeping occupier
      #     - Return the occupier's item

      value  = NULL if value.nil? # The sentinel allows nil to be a valid value
      end_at = Concurrent.monotonic_time + timeout.to_f # The time to give up
      me     = nil

      begin
        me = Node.new(value) # create my node in case I need to occupy

        loop do
          other = slot
          if other && compare_and_set_slot(other, nil)
            # try to fulfill
            begin
              if fulfill(other, value)
                # happy path: the CAS on the other's hole was the commit point
                break Result.fulfilled(null_to_nil(other.item))
              end
              # the occupier cancelled its hole before we could commit: retry
            ensure
              # We already removed the occupier from the slot. If the handoff
              # did not commit (its hole was cancelled first, or we are
              # unwinding because of an abnormal exit) the matched occupier
              # must not be left sleeping on a dead partner: cancel its hole
              # and wake it so it can finish with a cancelled result.
              abandon(other)
            end
          elsif other.nil? && compare_and_set_slot(nil, me)
            # try to occupy
            remaining = timeout && (end_at - Concurrent.monotonic_time)
            if me.latch.wait(remaining)
              # woken: my hole holds the outcome committed by the partner
              hole = me.value
              break hole == CANCEL ? Result.cancelled : Result.fulfilled(null_to_nil(hole))
            else
              # the wait timed out: race my cancellation against a racing
              # fulfiller. The CAS on my hole is the commit point which
              # decides whether the exchange happened or not.
              if compare_and_set_slot(me, nil)
                # no fulfiller matched with me: unmatched timeout
                break Result.timeout
              elsif me.compare_and_set_value(nil, CANCEL)
                # a fulfiller matched but lost the commit race; it will see
                # the cancelled hole and retry. Nothing changed hands.
                break Result.timeout
              else
                # the fulfiller won the commit race: the exchange committed
                break Result.fulfilled(null_to_nil(me.value))
              end
            end
          end
          break Result.timeout if timeout && Concurrent.monotonic_time >= end_at
        end
      ensure
        if me
          # Never leave my node behind: an abnormal exit (exception or kill)
          # must not strand my node in the slot for the next exchange, and a
          # fulfiller racing to match with me must see a cancelled hole
          # rather than hand its value to a dead partner. Both operations
          # are no-ops once the exchange has resolved.
          compare_and_set_slot(me, nil)
          me.compare_and_set_value(nil, CANCEL)
        end
      end
    end

    # Attempt to commit the handoff to a matched occupier. The CAS on the
    # occupier's hole is the single commit point of the exchange: once it
    # succeeds both sides are obliged to observe the exchange as fulfilled.
    #
    # @!visibility private
    # @return [Boolean] true when the handoff committed
    def fulfill(other, value)
      if other.compare_and_set_value(nil, value)
        other.latch.count_down
        true
      else
        false
      end
    end

    # Cancel a matched occupier's hole and wake it. Used when the handoff
    # could not (or will not) commit, so the matched waiter is never left
    # sleeping on a partner that already walked away. A no-op when the
    # occupier's hole has already resolved.
    #
    # @!visibility private
    def abandon(other)
      other.compare_and_set_value(nil, CANCEL)
      other.latch.count_down
    end

    # @!visibility private
    def null_to_nil(value)
      value == NULL ? nil : value
    end
  end

  if Concurrent.on_jruby?
    require 'concurrent/utility/native_extension_loader'

    # @!macro internal_implementation_note
    # @!visibility private
    class JavaExchanger < AbstractExchanger

      def initialize
        @exchanger = java.util.concurrent.Exchanger.new
      end

      private

      # @!macro exchanger_method_do_exchange
      #
      # @return [Object, CANCEL] the value exchanged by the other thread; {CANCEL} on timeout
      def do_exchange(value, timeout)
        result = nil
        if timeout.nil?
          Synchronization::JRuby.sleep_interruptibly do
            result = @exchanger.exchange(value)
          end
        else
          Synchronization::JRuby.sleep_interruptibly do
            result = @exchanger.exchange(value, 1000 * timeout, java.util.concurrent.TimeUnit::MILLISECONDS)
          end
        end
        result
      rescue java.util.concurrent.TimeoutException
        CANCEL
      end
    end
  end

  # @!visibility private
  # @!macro internal_implementation_note
  ExchangerImplementation = case
                            when Concurrent.on_jruby?
                              JavaExchanger
                            else
                              RubyExchanger
                            end
  private_constant :ExchangerImplementation

  # @!macro exchanger
  class Exchanger < ExchangerImplementation

    # @!method initialize
    #   Creates exchanger instance

    # @!method exchange(value, timeout = nil)
    #   @!macro exchanger_method_do_exchange
    #   @!macro exchanger_method_exchange

    # @!method exchange!(value, timeout = nil)
    #   @!macro exchanger_method_do_exchange
    #   @!macro exchanger_method_exchange_bang

    # @!method try_exchange(value, timeout = nil)
    #   @!macro exchanger_method_do_exchange
    #   @!macro exchanger_method_try_exchange

    # @!method exchange_result(value, timeout = nil)
    #   @!macro exchanger_method_do_exchange
    #   @!macro exchanger_method_exchange_result
  end
end
