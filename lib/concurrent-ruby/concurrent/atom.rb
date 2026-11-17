require 'concurrent/atomic/atomic_reference'
require 'concurrent/collection/copy_on_notify_observer_set'
require 'concurrent/concern/logging'
require 'concurrent/concern/observable'
require 'concurrent/synchronization/object'

# @!macro thread_safe_variable_comparison
#
#   ## Thread-safe Variable Classes
#
#   Each of the thread-safe variable classes is designed to solve a different
#   problem. In general:
#
#   * *{Concurrent::Agent}:* Shared, mutable variable providing independent,
#     uncoordinated, *asynchronous* change of individual values. Best used when
#     the value will undergo frequent, complex updates. Suitable when the result
#     of an update does not need to be known immediately.
#   * *{Concurrent::Atom}:* Shared, mutable variable providing independent,
#     uncoordinated, *synchronous* change of individual values. Best used when
#     the value will undergo frequent reads but only occasional, though complex,
#     updates. Suitable when the result of an update must be known immediately.
#   * *{Concurrent::AtomicReference}:* A simple object reference that can be updated
#     atomically. Updates are synchronous but fast. Best used when updates a
#     simple set operations. Not suitable when updates are complex.
#     {Concurrent::AtomicBoolean} and {Concurrent::AtomicFixnum} are similar
#     but optimized for the given data type.
#   * *{Concurrent::Exchanger}:* Shared, stateless synchronization point. Used
#     when two or more threads need to exchange data. The threads will pair then
#     block on each other until the exchange is complete.
#   * *{Concurrent::MVar}:* Shared synchronization point. Used when one thread
#     must give a value to another, which must take the value. The threads will
#     block on each other until the exchange is complete.
#   * *{Concurrent::ThreadLocalVar}:* Shared, mutable, isolated variable which
#     holds a different value for each thread which has access. Often used as
#     an instance variable in objects which must maintain different state
#     for different threads.
#   * *{Concurrent::TVar}:* Shared, mutable variables which provide
#     *coordinated*, *synchronous*, change of *many* stated. Used when multiple
#     value must change together, in an all-or-nothing transaction.


module Concurrent

  # Atoms provide a way to manage shared, synchronous, independent state.
  #
  # An atom is initialized with an initial value and an optional validation
  # proc. At any time the value of the atom can be synchronously and safely
  # changed. If a validator is given at construction then any new value
  # will be checked against the validator and will be rejected if the
  # validator returns false or raises an exception.
  #
  # There are two ways to change the value of an atom: {#compare_and_set} and
  # {#swap}. The former will set the new value if and only if it validates and
  # the current value matches the new value. The latter will atomically set the
  # new value to the result of running the given block if and only if that
  # value validates.
  #
  # ## Example
  #
  # ```
  # def next_fibonacci(set = nil)
  #   return [0, 1] if set.nil?
  #   set + [set[-2..-1].reduce{|sum,x| sum + x }]
  # end
  #
  # # create an atom with an initial value
  # atom = Concurrent::Atom.new(next_fibonacci)
  #
  # # send a few update requests
  # 5.times do
  #   atom.swap{|set| next_fibonacci(set) }
  # end
  #
  # # get the current value
  # atom.value #=> [0, 1, 1, 2, 3, 5, 8]
  # ```
  #
  # ## Update Block Constraints
  #
  # The block given to {#swap} is an *optimistic* update: the Atom reads the
  # current value, applies the block, and then tries to commit the result with
  # a compare-and-set. When another thread commits in the meantime the block is
  # invoked again with the newer value. It may also be invoked again when a
  # commit attempt loses a race. There is no bound on the number of attempts.
  #
  # The block is therefore required to be *idempotent with respect to all
  # effects other than its return value*: it may run any number of times, but
  # only the value returned by the attempt that actually commits is observed by
  # anyone. In particular the block must not mutate state the caller cares
  # about, append to shared collections, send messages, write to a database,
  # increment counters, or perform any other externally visible side effect.
  # Accumulate the intended effect in the returned value and let an observer
  # perform it once, when the change is committed. Side effects that cannot be
  # deferred belong outside the Atom.
  #
  # The block must also be free of shared mutable state that it does not
  # synchronize itself; it may run concurrently with other update blocks on the
  # same Atom.
  #
  # ## Observation
  #
  # Atoms support observers through the {Concurrent::Observable} mixin module.
  # Notification of observers occurs only when a change is *committed*, i.e.
  # when the new value passed validation and the compare-and-set that publishes
  # it succeeded. A failed validation, an exception raised by the validator, or
  # a lost compare-and-set race does not notify anyone: observers never see a
  # value the Atom did not actually hold.
  #
  # When notified the observer receives four arguments: `time`, `old_value`,
  # `new_value`, and `version`. The `time` argument is the time at which the
  # value change was committed. `old_value` is the value the Atom held
  # immediately before that commit and `new_value` is the value it holds
  # immediately after it. `version` is a monotonically increasing integer that
  # is assigned to each committed change, starting at 1 for the first change
  # after construction (the initial value reported by {#version} has version 0).
  # Note that `old_value` and `new_value` may be the same; this is not an error,
  # it simply means the committed operation returned the same value.
  #
  # Observers written against the older three argument form keep working: an
  # observer whose callback accepts fewer than four arguments receives only as
  # many as it declares (an observer accepting `*args` receives all four). This
  # makes the change backward compatible while letting new observers opt into
  # the version.
  #
  # A call which commits when no other thread is announcing notifies the
  # observers before it returns, preserving Atom's synchronous behaviour. When
  # another thread is already announcing, the committing call returns as soon as
  # its change is queued and the announcing thread delivers it. The commit itself
  # is never delayed by observers, and observers are never called while holding
  # the Atom's internal lock.
  #
  # Notifications for a single Atom are delivered in commit order: if a change
  # with version `n` is announced to an observer, then every change with a
  # version lower than `n` has already been announced to that observer. This
  # holds even when an observer reacts by triggering a further update of the
  # same Atom: the nested update is committed immediately, but its notification
  # is queued behind the notification which caused it and is delivered only
  # after that notification has reached every observer. Each observer therefore
  # sees a strictly increasing sequence of versions, with `old_value` of a
  # notification equal to the `new_value` of the one before it. An update made
  # from within an update block (rather than from an observer) is not ordered
  # this way and may be interleaved; trigger follow-up updates from observers.
  #
  # If an observer raises an exception the exception is logged and swallowed.
  # The Atom keeps the committed value (it is not rolled back) and the
  # remaining observers are still notified exactly once, in registration order.
  # An observer is not called again for a change it has already been notified
  # of. An observer which unconditionally triggers another update would keep
  # the Atom announcing changes forever; that is a bug in the observer, not a
  # supported usage.
  #
  # The `old_value`/`new_value` pair handed to an observer is only referenced
  # while the notification is being delivered; the Atom does not retain it
  # after that, so observers that need to keep it beyond the callback must copy
  # or retain it themselves.
  #
  # Unlike in Clojure, `Atom` cannot participate in {Concurrent::TVar} transactions.
  #
  # @!macro thread_safe_variable_comparison
  #
  # @see http://clojure.org/atoms Clojure Atoms
  # @see http://clojure.org/state Values and Change - Clojure's approach to Identity and State
  class Atom < Synchronization::Object
    include Concern::Logging
    include Concern::Observable

    safe_initialization!

    # A committed value together with the number of commits which produced it.
    # Instances are frozen and never mutated, a new one is published atomically
    # by every successful commit.
    #
    # @!visibility private
    State = Struct.new(:value, :version)

    # A committed change which is waiting to be announced to the observers.
    # Instances live only until they have been delivered; the atom does not keep
    # them as part of its state, so a change (and the values it carries) becomes
    # garbage as soon as the queue has moved past it.
    #
    # @!visibility private
    Change = Struct.new(:time, :old_value, :new_value, :version)

    private_constant :State, :Change

    attr_atomic(:state)
    private :state, :state=, :swap_state, :compare_and_set_state, :update_state

    # Create a new atom with the given initial value.
    #
    # @param [Object] value The initial value
    # @param [Hash] opts The options used to configure the atom
    # @option opts [Proc] :validator (nil) Optional proc used to validate new
    #   values. It must accept one and only one argument which will be the
    #   intended new value. The validator will return true if the new value
    #   is acceptable else return false (preferably) or raise an exception.
    #
    # @!macro deref_options
    #
    # @raise [ArgumentError] if the validator is not a `Proc` (when given)
    def initialize(value, opts = {})
      super()
      @Validator     = opts.fetch(:validator, -> v { true })
      @CommitLock    = ::Mutex.new
      @Changes       = []
      @Announcer     = nil
      self.observers = Collection::CopyOnNotifyObserverSet.new
      self.state     = State.new(value, 0).freeze
    end

    # The current value of the atom.
    #
    # @return [Object] The current value.
    def value
      state.value
    end

    alias_method :deref, :value

    # The version of the current value. A version is assigned to every commit,
    # starting at `0` for the value given at construction and increasing by one
    # with every committed change. It is the same number observers receive with
    # their notification.
    #
    # @return [Integer] The version of the current value.
    def version
      state.version
    end

    # Atomically swaps the value of atom using the given block. The current
    # value will be passed to the block, as will any arguments passed as
    # arguments to the function. The new value will be validated against the
    # (optional) validator proc given at construction. If validation fails the
    # value will not be changed.
    #
    # Internally, {#swap} reads the current value, applies the block to it, and
    # attempts to compare-and-set it in. Since another thread may have changed
    # the value in the intervening time, it may have to retry, and does so in a
    # spin loop. The net effect is that the value will always be the result of
    # the application of the supplied block to a current value, atomically.
    # However, because the block might be called multiple times, it must be free
    # of side effects. See the class documentation for details.
    #
    # Observers are notified only when the resulting value is actually
    # committed. A rejected value (validator returned false or raised) and an
    # attempt which lost a race against another writer produce no notification.
    # When an observer raises, the exception is logged and swallowed: the
    # committed value is kept and the remaining observers are still notified.
    #
    # @note The given block may be called multiple times, and thus should be free
    #   of side effects.
    #
    # @param [Object] args Zero or more arguments passed to the block.
    #
    # @yield [value, args] Calculates a new value for the atom based on the
    #   current value and any supplied arguments.
    # @yieldparam value [Object] The current value of the atom.
    # @yieldparam args [Object] All arguments passed to the function, in order.
    # @yieldreturn [Object] The intended new value of the atom.
    #
    # @return [Object] The final value of the atom after all operations and
    #   validations are complete.
    #
    # @raise [ArgumentError] When no block is given.
    def swap(*args)
      raise ArgumentError.new('no block given') unless block_given?

      loop do
        current   = state
        old_value = current.value
        new_value = yield(old_value, *args)

        return old_value unless valid?(new_value)
        return new_value if ns_commit(new_value, current)
      end
    end

    # Atomically sets the value of atom to the new value if and only if the
    # current value of the atom is identical to the old value and the new
    # value successfully validates against the (optional) validator given
    # at construction.
    #
    # Observers are notified only when the value is actually changed. A failed
    # validation or a lost compare-and-set race produces no notification. When
    # an observer raises, the exception is logged and swallowed: the committed
    # value is kept and the remaining observers are still notified.
    #
    # @param [Object] old_value The expected current value.
    # @param [Object] new_value The intended new value.
    #
    # @return [Boolean] True if the value is changed else false.
    def compare_and_set(old_value, new_value)
      return false unless valid?(new_value)

      if old_value.kind_of?(Numeric)
        # Match `AtomicReference#compare_and_set` for numbers: compare with
        # `#==` and retry if another writer slipped in between the comparison
        # and the commit, so a still-matching value is not rejected needlessly.
        loop do
          current = state
          return false unless ns_numeric_matches?(old_value, current.value)
          return true if ns_commit(new_value, current)
        end
      else
        current = state
        return false unless current.value.equal?(old_value)
        ns_commit(new_value, current)
      end
    end

    # Atomically sets the value of atom to the new value without regard for the
    # current value so long as the new value successfully validates against the
    # (optional) validator given at construction.
    #
    # Observers are notified only when the value is actually committed. A failed
    # validation produces no notification. When an observer raises, the
    # exception is logged and swallowed: the committed value is kept and the
    # remaining observers are still notified.
    #
    # @param [Object] new_value The intended new value.
    #
    # @return [Object] The final value of the atom after all operations and
    #   validations are complete.
    def reset(new_value)
      return state.value unless valid?(new_value)

      ns_commit(new_value)
      new_value
    end

    private

    # Is the new value valid?
    #
    # @param [Object] new_value The intended new value.
    # @return [Boolean] false if the validator function returns false or raises
    #   an exception else true
    def valid?(new_value)
      @Validator.call(new_value)
    rescue
      false
    end

    # Does the current value match a numeric expectation? Mirrors the equality
    # used by `AtomicReference#compare_and_set` for numbers: `#==`, with `NaN`
    # matching only `NaN` (since `NaN != NaN`).
    #
    # @param [Numeric] expected the value given by the caller
    # @param [Object] actual the value currently held by the atom
    # @return [Boolean]
    def ns_numeric_matches?(expected, actual)
      return false unless actual.kind_of?(Numeric)

      if expected.respond_to?(:nan?) && expected.nan?
        actual.respond_to?(:nan?) && actual.nan?
      else
        actual == expected
      end
    end

    # Commits `new_value` and announces the change to the observers.
    #
    # Passing `expected` commits only if the atom still holds exactly that
    # state, which is how a compare-and-set (and a retried {#swap} attempt)
    # detects that it was overtaken; passing `nil` commits unconditionally, last
    # writer wins. The state swap, the version assignment, and the enqueueing of
    # the notification all happen while holding the commit lock, so changes are
    # queued in commit order. Observers are never called while the lock is held.
    #
    # @param [Object] new_value the value to commit
    # @param [State, nil] expected the state the caller based its computation on
    # @return [Boolean] true when the value was committed, false when the atom
    #   had already moved on and nothing was announced
    def ns_commit(new_value, expected = nil)
      change = nil

      @CommitLock.synchronize do
        current = state
        return false if expected && !current.equal?(expected)

        committed = State.new(new_value, current.version + 1).freeze
        return false unless compare_and_set_state(current, committed)

        change = Change.new(Time.now, current.value, new_value, committed.version)
        @Changes << change
      end

      ns_announce
      true
    end

    # Announces the queued changes, in commit order, then returns.
    #
    # A single thread announces at a time. The thread which commits a change
    # simply enqueues it and then tries to become the announcer; if it succeeds
    # it drains the queue, so in the common single-threaded case the caller has
    # notified every observer by the time it returns (the synchronous behaviour
    # Atom always had). If another thread is already announcing, the caller
    # enqueued its change and returns - the active announcer will deliver it.
    # This deliberately never waits for the other thread: an observer is free to
    # block on work done by a thread which is itself updating the atom, which
    # would deadlock if committing waited for the announcer.
    #
    # Because ownership is released only in the same critical section that finds
    # the queue empty, a change enqueued at any moment is either taken by the
    # current announcer or causes the producer to become the next announcer;
    # no change can be left behind.
    #
    # A change committed by an observer (a nested update) is enqueued and, since
    # the observer runs on the announcing thread, the nested commit returns
    # immediately. The active announcer delivers it after the current change has
    # reached every observer, so each observer sees strictly increasing
    # versions and never a change out of commit order.
    #
    # @return [void]
    def ns_announce
      token = ::Object.new

      @CommitLock.synchronize do
        return if @Announcer
        @Announcer = token
      end

      begin
        loop do
          change = nil
          @CommitLock.synchronize do
            change = @Changes.shift
            # Release ownership in the same critical section as the empty
            # check, so a producer either enqueues before this point (and is
            # picked up above) or observes an unowned queue and takes over.
            @Announcer = nil if change.nil?
          end
          break if change.nil?

          begin
            ns_notify_change(change)
          rescue Exception => error
            # `ns_notify_change` isolates observer failures itself; this is a
            # last resort so an unexpected error cannot strand the changes
            # still queued behind this one.
            ns_log_observer_error(change, error, change.version)
          end
        end
      ensure
        # The loop already released ownership if it drained the queue; only
        # clear it here if this call still owns it (e.g. an unexpected error),
        # never taking ownership away from a successor.
        @CommitLock.synchronize do
          @Announcer = nil if @Announcer.equal?(token)
        end
      end
    end

    # Announces a single committed change to all observers.
    #
    # Each observer is called exactly once for the change, in registration
    # order. An observer which raises does not prevent the remaining observers
    # from being notified and does not affect the already committed value; the
    # exception is swallowed and logged. Nothing is propagated to the committing
    # thread, because the change is already committed and cannot be undone.
    #
    # @param [Change] change the committed change to announce
    # @return [void]
    def ns_notify_change(change)
      errors = observers.notify_observers_safely do |observer, function|
        ns_observation_arguments(observer, function, change)
      end

      errors.each do |observer, error|
        ns_log_observer_error(observer, error, change.version)
      end
    end

    # Records an observer failure without letting it affect the announcement.
    #
    # @param [Object] observer the observer which raised
    # @param [Exception] error the raised exception
    # @param [Integer, nil] version the version of the change being announced
    # @return [void]
    def ns_log_observer_error(observer, error, version = nil)
      log(Concern::Logging::ERROR, 'atom',
          "observer #{observer.inspect} raised #{error.class} while handling " \
          "version #{version.inspect}: #{error.message}")
    end

    # Builds the argument list for one observer, honouring the number of
    # arguments it accepts so observers written before the version was added
    # keep receiving the three arguments they expect. An observer accepting the
    # full list (or a splat) receives `time`, `old_value`, `new_value`, and
    # `version`.
    #
    # @param [Object] observer the observer being notified
    # @param [Symbol] function the method to call on the observer
    # @param [Change] change the committed change being announced
    # @return [Array] the arguments to pass
    def ns_observation_arguments(observer, function, change)
      arguments = [change.time, change.old_value, change.new_value, change.version]

      accepted = ns_accepted_argument_count(observer, function)
      accepted.nil? ? arguments : arguments.first(accepted)
    end

    # How many positional arguments can the observer's callback accept? `nil`
    # means "no limit" (a splat or an unknown callable).
    #
    # @param [Object] observer the observer being notified
    # @param [Symbol] function the method to call on the observer
    # @return [Integer, nil]
    def ns_accepted_argument_count(observer, function)
      callable = observer.is_a?(::Proc) || observer.is_a?(::Method) ?
                   observer : observer.method(function)

      parameters = callable.parameters
      return nil if parameters.any? { |kind, _| kind == :rest }

      parameters.count { |kind, _| kind == :req || kind == :opt }
    rescue NameError
      nil
    end
  end
end
