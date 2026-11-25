module SemanticLogger
  class QueueProcessor
    # Internal: how long the worker thread of a queue processor waits, after it could not write to
    # the destination of its appender, before it writes again, and when it gives up instead.
    #
    # It knows how many times in a row writing has failed, how many times the worker thread may retry
    # before it gives up (async_max_retries), how many requests, such as a flush or close, are
    # waiting for the worker thread to run them, and whether the process is exiting. Each back-off
    # waits a second longer than the one before, except while the worker thread is waited on, by a
    # request or the exiting process, which must not wait out back-offs: then the worker thread does
    # not wait at all, submitting a request or exiting ends the back-off under way, and once the
    # destination fails, the worker thread gives up on the messages queued ahead of the next request
    # (see #give_up?).
    #
    # It also knows the failure it is backing off from, as a token it hands to the appender, so that
    # the appender, or the client it writes with, can report once that the destination has recovered,
    # and end that back-off rather than wait it out (see #recovered). A report is honoured only for
    # the failure under way, and only once: one that arrives after that back-off has ended, such as a
    # late one delivered during the back-off from a later failure, ends nothing and starts no write.
    #
    # Requests are submitted from other threads, and from a signal trap, where Ruby does not allow a
    # Mutex to be locked, so it counts them, and waits, on Queues. So are recoveries reported, which
    # take no lock either, which is why the failure under way is read and written as a plain
    # instance variable, and each failure takes a single token, as a Request does.
    class Backoff
      # Internal: a failure of the destination of the appender, after which the worker thread backs
      # off. It is handed to the appender before the back-off (see QueueProcessor#notify_backing_off),
      # so that the appender, or the client it writes with, can report once that the destination has
      # recovered from that failure, and end that back-off (see #recovered).
      #
      # It knows its own generation, the failures in a row it belongs to, which is how many seconds
      # the back-off waits, and the exception with which the destination failed. So only the failure
      # that the worker thread is still backing off from can be reported, once: a report for a
      # failure whose back-off has ended, such as one delivered late after another failure, ends
      # nothing and starts no write.
      class Failure
        # Returns [Integer] this failure's generation: it grows by one for each failure that the
        # worker thread backs off from, since the process started or the last fork, so that every
        # back-off, and every recovery reported for it, can be told apart.
        attr_reader :id

        # Returns [Integer] the failures in a row that this one belongs to, counting it, which is
        # also how many seconds the back-off waits.
        attr_reader :retry_count

        # Returns [Exception] the exception with which the destination failed.
        attr_reader :exception

        def initialize(backoff, generation, retry_count, exception)
          @backoff     = backoff
          @id          = generation
          @retry_count = retry_count
          @exception   = exception
          @honoured    = false
          # A single token, which the first report of a recovery for this failure takes, so that no
          # other report for it can end a back-off.
          @token       = Queue.new
          @token << true
        end

        # Report that the destination has recovered from this failure, once, ending the back-off
        # under way instead of waiting it out. Returns [true|false] whether it did so, and false for
        # a failure that is no longer the one being backed off from, such as one delivered late after
        # that back-off ended: it then ends nothing and so starts no write. Takes no lock, so that it
        # can be called from any thread, and from a signal trap.
        def recovered = @backoff.recovered(self)
        alias recovered! recovered
        alias recover recovered

        # Returns [true|false] whether a report of a recovery from this failure ended its back-off.
        def honoured? = @honoured

        # Returns [true|false] whether the worker thread is still backing off from this failure.
        def pending? = @backoff.failure.equal?(self)

        # Returns [true|false] whether the back-off from this failure has ended, whether it ran out,
        # was ended by a report of a recovery, or was cancelled by a request or the process exiting.
        def ended? = !pending?

        def inspect
          "#<#{self.class.name} id=#{id} retry_count=#{retry_count} #{pending? ? 'pending' : 'ended'}>"
        end

        # Takes the single token, so that only the first report of a recovery for this failure can be
        # honoured, whichever thread makes it. Returns [true|false] whether it took it. Internal use
        # by Backoff only.
        def claim = @token.pop(timeout: 0)

        # Records that a report of a recovery ended the back-off from this failure. Internal use by
        # Backoff only.
        def honour = @honoured = true
      end

      # Internal: what the back-off of a queue processor did, one entry for each event, so that which
      # failure a report of a recovery ended, which arrived too late to end one, and which write the
      # worker thread tried again can be told apart, by the generation of the failure (see
      # Failure#id).
      #
      # It knows the most entries it keeps, so that a worker thread that fails and backs off for a
      # long time, such as through an outage, cannot grow it without bound: once full, the oldest
      # entry makes way for a new one. The counts of failures, recoveries, and late recoveries, are
      # kept on the back-off itself, so they are exact however many events have scrolled off.
      class EventLog
        # One event: its kind (:failure, :recovery, :late_recovery, or :write), the generation of the
        # failure it belongs to, and any detail the failure carried, such as the exception.
        Event = Struct.new(:kind, :failure_id, :detail) do
          def to_s = [kind, "failure=#{failure_id}", detail].compact.join(" ")
        end

        # Returns [Integer] the most events kept.
        attr_reader :limit

        def initialize(limit = 100)
          @limit  = limit
          @events = []
        end

        # Record an event, dropping the oldest once the limit is reached. The caller supplies one of
        # the kinds above, the failure's generation, and any detail. Takes no lock, since it is
        # recorded on the worker thread, or on a thread that reports a recovery, one event at a time,
        # and a lost entry only ever costs a line of the record.
        def record(kind, failure_id, detail = nil)
          @events << Event.new(kind, failure_id, detail)
          @events.shift while @events.size > limit
          self
        end

        # Returns [Array<Event>] the events recorded, oldest first.
        def events = @events.dup

        # Returns [Array<Event>] the events of the given kind.
        def of_kind(kind) = @events.select { |event| event.kind == kind }

        # Returns [Integer] how many events of the given kind the log holds (see #limit).
        def count(kind) = @events.count { |event| event.kind == kind }

        def clear = @events.clear

        def size = @events.size

        def inspect = "#<#{self.class.name} limit=#{limit} events=#{@events.inspect}>"
      end

      # Internal: a snapshot of what the back-off of a queue processor did, for a report or a test to
      # read without the back-off changing under it: how many failures the worker thread backed off
      # from (each with a generation of its own), how many reports that the destination had recovered
      # ended one, and how many arrived after it had ended, starting no write. It holds the failure
      # under way, if any, whose recovery the appender may report once, and the most recent events
      # (see EventLog), each by its failure's generation.
      Summary = Data.define(:failures, :recoveries, :late_recoveries, :failure, :events) do
        # Returns [Array<Event>] the most recent back-off events, oldest first.
        def backoff_events = events.events
      end

      # Returns [Integer] the number of times the worker thread retries after a failure, before it
      # gives up, or -1 to retry for ever.
      attr_reader :max_retries

      # Returns [Integer] the number of failures in a row, which is also how many seconds the next
      # back-off waits.
      attr_reader :retry_count

      # Returns [Failure|nil] the failure the worker thread is backing off from, and whose recovery
      # the appender may report once (see #recovered), or nil once that back-off has ended, whether
      # it ran out, was ended by a report of a recovery, or was cancelled by a request or the process
      # exiting.
      attr_reader :failure

      # Returns [Integer] how many failures the worker thread backed off from since the process
      # started, or since the last fork. Each one has a generation of its own (see Failure#id).
      attr_reader :failures

      # Returns [Integer] how many reports that the destination had recovered ended the back-off
      # under way, since the process started, or since the last fork.
      attr_reader :recovery_count

      # Returns [Integer] how many reports that the destination had recovered arrived for a failure
      # whose back-off had already ended, and so were ignored, starting no write, since the process
      # started, or since the last fork.
      attr_reader :late_recovery_count

      # Returns [EventLog] the recent events: each failure the worker thread backed off from, each
      # report of a recovery that ended one, each that arrived too late, and each write of a message
      # the destination failed again after the back-off, by the failure's generation. It keeps the
      # most recent EventLog#limit, so that its counts can differ from the totals above.
      attr_reader :events

      # Parameters:
      #   clock: [#wait]
      #     An object that waits for the given number of seconds, or until the given Queue holds an
      #     entry, as Queue#pop(timeout:) does, for a test to supply a fake clock:
      #       clock.wait(queue, seconds)   waits on the queue, at most that long.
      #     nil: wait on the real clock, as Queue#pop(timeout:) does.
      #   reporter: [#log, nil]
      #     The reporter of the queue processor, told when a report of a recovery ended a back-off,
      #     and when one arrived too late to. nil: report nothing.
      #   appender: [Subscriber, nil]
      #     The appender to tell of each failure before the back-off from it (see
      #     Subscriber#backing_off). nil, or one that does not answer it: none.
      def initialize(max_retries, clock: nil, reporter: nil, appender: nil)
        @max_retries         = max_retries
        @clock               = clock
        @reporter            = reporter
        @appender            = appender
        @retry_count         = 0
        @failure             = nil
        @failures            = 0
        @recovery_count      = 0
        @late_recovery_count = 0
        @events              = EventLog.new
        # The failure the next #wait is for (see #retry?), and the one under way, which a report of a
        # recovery clears.
        @awaiting            = nil
        # One entry for each request waiting.
        @requests            = Queue.new
        # Holds an entry once a request has been submitted, the process has begun exiting, or a
        # recovery has been reported, since the back-off under way began. One is enough to end it, so
        # that submitting many requests without a failure does not add up.
        @interrupts          = SizedQueue.new(1)
        @exiting             = false
        @gave_up             = nil
      end

      # Start counting afresh, once writing succeeds.
      def reset
        @retry_count = 0
        @failure     = nil
      end

      # Start afresh for a worker thread new to the process, after a fork: it has not failed yet, the
      # requests counted as waiting were on the queue of the parent process, which the child
      # replaces, and the child is not exiting, even when the parent was, such as when another thread
      # forked while the parent flushed at exit. The failures, and the recoveries reported, start
      # counting afresh too, since the generations of the parent are not the child's.
      def restart
        reset
        @failures            = 0
        @recovery_count      = 0
        @late_recovery_count = 0
        @events.clear
        @requests.clear
        @exiting = false
        @gave_up = nil
      end

      # Count a failure, and return the Failure to hand to the appender (see Subscriber#backing_off),
      # or false once the worker thread has retried max_retries times in a row.
      #
      # The exception is the one with which the destination failed, if known, which the appender may
      # use to tell whether a recovery it hears of is for this failure.
      def retry?(exception = nil)
        return false unless max_retries == -1 || retry_count < max_retries

        @retry_count += 1
        @failures    += 1
        @failure      = Failure.new(self, @failures, @retry_count, exception)
        # The failure the next wait is for, kept apart from the one under way, which a report of a
        # recovery clears before the worker thread gets to wait (see #wait).
        @awaiting     = @failure
        @events.record(:failure, @failure.id, exception)
        notify(@failure)
        @failure
      end

      # Returns [Integer|String] the number of retries the worker thread makes at most, for a report.
      def limit = max_retries == -1 ? "unlimited" : max_retries

      # Returns [Summary] what the back-off did, as a snapshot: the failures, the recoveries, the late
      # ones, the failure under way, and the most recent events (see Summary).
      def summary
        Summary.new(
          failures:        @failures,
          recoveries:      @recovery_count,
          late_recoveries: @late_recovery_count,
          failure:         @failure,
          events:          @events
        )
      end

      # Waits retry_count seconds for the failure the worker thread counted last (see #retry?),
      # unless it is waited on, the appender reports that the destination has recovered from that
      # failure, a request is submitted, or the process exits.
      #
      # The interruptions from before are cleared before it checks whether it is waited on, so that a
      # request submitted, the process exiting, or a recovery reported, in between either is seen or
      # interrupts the wait. It waits for the failure kept for this wait, rather than the one under
      # way, so that a recovery reported for it between the failure and this call is still seen: the
      # interruption it pushed is cleared here, but the failure is marked honoured, which ends the
      # wait. Once it returns, the wait has ended however it ended, so a report of a recovery that
      # arrives after that is late, and ends nothing (see #recovered).
      def wait
        failure = @awaiting
        @interrupts.clear
        return end_failure(failure) if waited_on? || failure&.honoured?

        @clock ? @clock.wait(@interrupts, retry_count) : @interrupts.pop(timeout: retry_count)
      ensure
        @awaiting = nil
        end_failure(failure)
        # The worker thread writes the message again now, whether the back-off ran out or a report of
        # a recovery ended it, so the events tell a real retry from the back-off apart.
        @events.record(:write, failure.id) if failure
      end

      # End the back-off from the supplied failure, if it is the one under way, since the appender
      # reports that the destination has recovered from it. Returns [true|false] whether it ended it:
      # false for a failure whose back-off has already ended, whether it ran out, was cancelled by a
      # request or the process exiting, or had a recovery reported for it before, such as a report
      # delivered late after another failure. Such a report then ends nothing, and so starts no
      # write. Takes no lock, so that it can be called from any thread, and from a signal trap.
      #
      # The count of failures in a row is kept, since the destination failed again: a write that
      # fails once the back-off has been ended is the next retry of the same run of failures, not the
      # first of a new one. A write that succeeds resets it (see #reset).
      def recovered(failure)
        # Only the first report for a failure can end a back-off, whatever thread makes it.
        if !failure.claim || !@failure.equal?(failure)
          @late_recovery_count += 1
          @events.record(:late_recovery, failure.id, failure.honoured? ? :already_reported : :back_off_ended)
          @reporter&.late_recovery(failure)
          return false
        end

        failure.honour
        @failure         = nil
        @recovery_count += 1
        @events.record(:recovery, failure.id)
        @reporter&.recovered(failure)
        # Ends the wait under way, if any, and makes the worker thread skip one it has yet to begin.
        @interrupts.push(true, timeout: 0)
        true
      end

      # Called before a request is added to the queue: it is waiting until #request_done, and ends the
      # back-off under way. Never waits, since an interruption already queued ends it as well. The
      # failure it was backing off from is no longer under way, so a recovery reported for it later
      # is late, and changes nothing (see #recovered).
      def request_submitted
        @failure = nil
        @requests << true
        @interrupts.push(true, timeout: 0)
      end

      # Called once for each request submitted, once it has run or will not (see Request#done). The
      # worker thread no longer gives up on the messages queued after it (see #give_up?).
      def request_done
        @requests.pop(timeout: 0)
        @gave_up = nil
      end

      # Tell it whether the process is exiting: true before the flush at exit, and false once that
      # flush has returned, since a test framework can run its tests in a later at_exit hook. While
      # the process exits, the worker thread is waited on, as though a request were waiting, and the
      # back-off under way ends. Once it no longer exits, the worker thread gives up no more, unless a
      # request is still waiting (see #gave_up). Called from the exiting thread, which may be in a
      # signal trap, so it takes no lock either.
      def exiting=(exiting)
        @exiting = exiting
        if exiting
          @failure = nil
          @interrupts.push(true, timeout: 0)
        elsif !waited_on?
          @gave_up = nil
        end
      end

      # Whether the worker thread is waited on: a request, such as a flush or close, is waiting for
      # it to run it, or the process is exiting. It then neither waits out back-offs nor writes again
      # what the destination failed.
      def waited_on? = @exiting || request_waiting?

      # Whether a request, such as a flush or close, is waiting for the worker thread to run it. The
      # thread that submitted it may be the worker thread of the queue processor that writes to this
      # one's appender, which then waits for this worker thread (see QueueProcessor#holding_up?).
      def request_waiting? = !@requests.empty?

      # Called once the destination failed. While the worker thread is waited on, it gives up on the
      # messages queued ahead of the next request, such as a flush, and discards them without writing
      # them (see #gave_up), since each could take as long to fail, such as the open_timeout of a
      # server that does not answer. While the process exits, it does so as well before the flush at
      # exit reaches the queue. Once a request has run (see #request_done), it writes the next
      # message once more, even while the process exits, and gives up again if that fails. So
      # returns true, having recorded the exception, for the worker thread to carry on straight
      # away, without backing off or counting a retry. Otherwise returns false.
      def give_up?(exception)
        return false unless waited_on?

        @gave_up = exception
        true
      end

      # Returns the exception with which the destination failed while the worker thread was waited
      # on (see #give_up?), while the messages it takes off the queue are to be discarded: until a
      # request has run, or the worker thread is no longer waited on. Otherwise nil.
      def gave_up
        return unless @gave_up
        return @gave_up if waited_on?

        @gave_up = nil
      end

      private

      # Tell the appender of the failure the worker thread is about to back off from, so that it, or
      # its client, can report once that the destination has recovered, from the Failure it is given,
      # which ends that back-off (see #recovered). An appender that raises is reported to the internal
      # logger, and the back-off is waited out, since that is not a failure to write.
      def notify(failure)
        return unless @appender.respond_to?(:backing_off)

        @appender.backing_off(failure)
      rescue StandardError => e
        @reporter&.log(:warn, "Async: Failed to tell the appender that the worker thread is backing off", e)
      end

      # Ends the back-off from the supplied failure, if that failure is still under way, once the
      # wait for it has returned. A report of a recovery for it that arrives afterwards is then
      # late: it cannot take the token, which is spent here, and so ends nothing and starts no write.
      # Leaves the count of failures in a row as it is, since the destination failed again.
      def end_failure(failure)
        return unless failure

        failure.claim
        @failure = nil if @failure.equal?(failure)
        nil
      end
    end
  end
end
