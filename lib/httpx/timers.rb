# frozen_string_literal: true

module HTTPX
  class Timers
    def initialize
      # timers are kept ordered by their absolute monotonic deadline. Grouping
      # callbacks sharing the same delay (see #after) is preserved by reusing an
      # already scheduled interval, which is also how a timer "belongs" to an
      # interval: cancelling a timer only removes its own callback, and an empty
      # interval is always dropped, so a stale request timer can never block or
      # interrupt timers scheduled by another request reusing the same selector.
      @intervals = []
    end

    def empty?
      prune!
      @intervals.empty?
    end

    def after(interval_in_secs, cb = nil, &blk)
      callback = cb || blk

      raise Error, "timer must have a callback" unless callback

      # I'm assuming here that most requests will have the same
      # request timeout, as in most cases they share common set of
      # options. A user setting different request timeouts for 100s of
      # requests will already have a hard time dealing with that.
      unless (interval = @intervals.bsearch { |t| t.interval == interval_in_secs })
        interval = Interval.new(interval_in_secs)
        @intervals << interval
        @intervals.sort!
      end

      interval << callback

      Timer.new(interval, callback)
    end

    # returns the delay until the next timer deadline, or +nil+ when there is
    # nothing scheduled. Intervals whose callbacks were all cancelled are pruned
    # here, so they never contribute a wait interval.
    def wait_interval
      prune!

      return if @intervals.empty?

      @intervals.first.time_left(Utils.now)
    end

    # runs every timer whose deadline has been reached. A cancelled timer is
    # never invoked, even when its interval fires while the cancellation happens
    # during the dispatch of another callback sharing the same interval.
    def fire(error = nil)
      prune!

      return if @intervals.empty?

      now = Utils.now

      raise error if error && error.timeout != @intervals.first.time_left(now)

      due, @intervals = @intervals.partition { |interval| interval.due?(now) }

      due.each(&:elapse)
    end

    private

    # only intervals without any live callback are removed; due intervals with
    # pending callbacks are kept so that #fire can dispatch them.
    def prune!
      @intervals = @intervals.reject(&:no_callbacks?)
    end

    class Timer
      # simpler helper which allows classification
      attr_accessor :label

      def initialize(interval, callback)
        @interval = interval
        @callback = callback
        @cancelled = false
      end

      # cancelling is idempotent and only affects this timer. The owning interval
      # is left untouched otherwise; it gets pruned once it has no callbacks left.
      def cancel
        return if @cancelled

        @cancelled = true
        @interval.delete(@callback)
      end

      def cancelled?
        @cancelled
      end
    end

    class Interval
      include Comparable

      attr_reader :interval

      def initialize(interval)
        @interval = interval
        @callbacks = []
        @deadline = nil
      end

      def <=>(other)
        @interval <=> other.interval
      end

      def ==(other)
        return @interval == other if other.is_a?(Numeric)

        @interval == other.to_f # rubocop:disable Lint/FloatComparison
      end

      def to_f
        Float(@interval)
      end

      def <<(callback)
        was_empty = @callbacks.empty?

        @callbacks << callback

        # arm (or re-arm after all previous timers were cancelled) the absolute
        # monotonic deadline when the interval gains its first live callback;
        # callbacks joining an already-armed interval share that deadline.
        @deadline = Utils.now + @interval if was_empty
      end

      def delete(callback)
        @callbacks.delete(callback)
      end

      def no_callbacks?
        @callbacks.empty?
      end

      def time_left(now = Utils.now)
        return @interval if @deadline.nil?

        @deadline - now
      end

      def due?(now = Utils.now)
        return false if @callbacks.empty? || @deadline.nil?

        time_left(now) <= 0
      end

      # dispatches the callbacks still registered at fire time. The list is
      # iterated over a snapshot, but each callback is re-checked against the
      # live registration list, so a timer cancelled (by a sibling callback of
      # the same interval, or externally) while the interval fires does not run.
      def elapse
        return if @callbacks.empty?

        @callbacks.dup.each do |callback|
          next unless @callbacks.include?(callback)

          callback.call
        end
      end
    end
    private_constant :Interval
  end
end
