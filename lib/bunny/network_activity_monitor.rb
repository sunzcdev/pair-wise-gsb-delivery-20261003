# frozen_string_literal: true

module Bunny
  # Per-connection record of the network I/O actually observed on a transport.
  #
  # A new monitor is created together with every {Bunny::Transport}, so a
  # transport that has been replaced during connection recovery carries its own
  # instance: a write or read that eventually completes on the old transport can
  # only ever touch the old monitor and therefore never resets the timers of the
  # connection that took over.
  #
  # Two directions are tracked independently:
  #
  #   * the write side is updated only once a full frame has been handed to the
  #     socket (queued data that is still waiting for the transport does not
  #     count),
  #   * the read side is updated only once a complete, legal AMQP frame has been
  #     received.
  #
  # @private
  class NetworkActivityMonitor
    # A monotonically increasing identifier, handy when several connections of
    # one session are involved (e.g. during recovery).
    attr_reader :connection_id

    def initialize(connection_id:, clock: nil, initial_time: nil)
      @connection_id = connection_id
      @clock         = clock || -> { Bunny::Timestamp.monotonic }
      @mutex         = Monitor.new

      now = initial_time || current_time

      @last_write_at       = now
      @last_read_at        = now
      @frames_written      = 0
      @frames_read         = 0
      @last_written_frame  = nil
      @last_read_frame     = nil
    end

    # Records that a complete frame (or a chunk of frames encoded into one
    # write) has actually been written to the socket.
    #
    # Only successful, completed writes reach this method: a publish that is
    # merely queued while the transport write is blocked leaves the write-side
    # timer untouched.
    #
    # @param [String] frame the exact bytes handed to the socket
    def record_write!(frame)
      @mutex.synchronize do
        @last_written_frame = frame
        @frames_written    += 1
        @last_write_at      = current_time
      end
    end

    # Records that a complete, legal AMQP frame has been read from the socket.
    #
    # @param [AMQ::Protocol::Frame] frame the decoded frame received from the peer
    def record_read!(frame)
      @mutex.synchronize do
        @last_read_frame = frame
        @frames_read    += 1
        @last_read_at    = current_time
      end
    end

    # @return [Numeric] monotonic timestamp of the last completed write
    def last_write_at
      @mutex.synchronize { @last_write_at }
    end

    # @return [Numeric] monotonic timestamp of the last completed read
    def last_read_at
      @mutex.synchronize { @last_read_at }
    end

    # @return [String, nil] exact bytes of the most recently written frame
    def last_written_frame
      @mutex.synchronize { @last_written_frame }
    end

    # @return [AMQ::Protocol::Frame, nil] most recently received frame
    def last_read_frame
      @mutex.synchronize { @last_read_frame }
    end

    # @return [Integer] number of completed writes recorded on this connection
    def frames_written
      @mutex.synchronize { @frames_written }
    end

    # @return [Integer] number of frames read on this connection
    def frames_read
      @mutex.synchronize { @frames_read }
    end

    # Whether the write side of this connection has been quiet for at least
    # +seconds+. This is what the heartbeat policy consults, preserving the
    # original contract that a heartbeat is only sent when no frame has been
    # written for a full interval.
    #
    # @param [Numeric] seconds
    # @return [Boolean]
    def write_idle_for?(seconds)
      @mutex.synchronize { current_time - @last_write_at >= seconds }
    end

    # Whether the read side of this connection has been quiet for at least
    # +seconds+.
    #
    # @param [Numeric] seconds
    # @return [Boolean]
    def read_idle_for?(seconds)
      @mutex.synchronize { current_time - @last_read_at >= seconds }
    end

    # Snapshot suitable for logging and test assertions. It explains, for one
    # specific connection, what was observed in each direction.
    #
    # @return [Hash]
    def to_h
      @mutex.synchronize do
        {
          connection_id:       @connection_id,
          last_write_at:       @last_write_at,
          last_read_at:        @last_read_at,
          frames_written:      @frames_written,
          frames_read:         @frames_read,
          last_written_frame:  @last_written_frame,
          last_read_frame:     @last_read_frame
        }
      end
    end

    # Human-readable report of the activity (or lack thereof) in each
    # direction on this connection.
    #
    # @return [String]
    def report
      @mutex.synchronize do
        write_frame = @last_written_frame
        read_frame  = @last_read_frame

        "#<#{self.class.name} connection=#{@connection_id} " \
          "writes=#{@frames_written} last_write_at=#{@last_write_at} " \
          "last_written_frame=#{write_frame.inspect} " \
          "reads=#{@frames_read} last_read_at=#{@last_read_at} " \
          "last_read_frame=#{read_frame.inspect}>"
      end
    end

    alias to_s report

    private

    def current_time
      @clock.call
    end
  end
end
