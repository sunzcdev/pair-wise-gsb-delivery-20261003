# frozen_string_literal: true

require "amq/protocol/client"
require "amq/protocol/frame"

module Bunny
  # Periodically sends heartbeats.
  #
  # The decision whether a heartbeat has to be sent is based on the transport's
  # {Bunny::NetworkActivityMonitor}, not on an optimistic timestamp updated when
  # a frame is merely enqueued:
  #
  #   * a publishing thread may have queued data that the transport has not
  #     finished writing yet,
  #   * the reader thread may be receiving frames from the peer,
  #
  # and only progress in the write direction of the connection this sender is
  # bound to can defer a heartbeat. A monitor belongs to exactly one transport,
  # so when the connection is replaced during recovery, write callbacks that
  # eventually complete on the old transport cannot refresh the timers of the
  # new one.
  #
  # @private
  class HeartbeatSender

    #
    # API
    #

    def initialize(transport, logger)
      @transport = transport
      @logger    = logger
      @mutex     = Monitor.new
    end

    def start(period = 30)
      @mutex.synchronize do
        # calculate interval as half the given period plus
        # some compensation for Ruby's implementation inaccuracy
        # (we cannot get at the nanos level the Java client uses, and
        # our approach is simplistic). MK.
        @interval = [(period / 2) - 1, 0.4].max

        @thread = Thread.new(&method(:run))
        @thread.report_on_exception = false
      end
    end

    def stop
      @mutex.synchronize { @thread.exit }
    end

    # Kept for backwards compatibility.
    #
    # Historically this optimistically refreshed a shared "last activity"
    # timestamp as soon as a frame was handed to the transport. Timing is now
    # driven by frames that were *actually* written (and read), recorded on the
    # transport's monitor, so this method no longer changes any timer.
    def signal_activity!
      # no-op: see NetworkActivityMonitor and Transport#write
    end

    protected

    def run
      begin
        loop do
          self.beat

          sleep @interval
        end
      rescue IOError => ioe
        @logger.error "I/O error in the heartbeat sender: #{ioe.message}"
        stop
      rescue ::Exception => e
        @logger.error "Error in the heartbeat sender: #{e.message}"
        stop
      end
    end

    def beat
      monitor = @transport.network_activity

      # Preserve the original idle-connection contract: a heartbeat is due
      # only when this connection's write side has made no progress for a
      # full interval. Queued-but-unwritten data does not count; traffic
      # observed in the read direction does not suppress a heartbeat either,
      # matching the previous behavior where only outgoing activity did.
      if monitor.write_idle_for?(@interval)
        activity = monitor.to_h
        @logger.debug do
          "Sending a heartbeat, connection: #{activity[:connection_id]}, " \
          "last write at: #{activity[:last_write_at]}, " \
          "last read at: #{activity[:last_read_at]}, " \
          "frames written: #{activity[:frames_written]}, " \
          "frames read: #{activity[:frames_read]}, interval (s): #{@interval}"
        end
        @transport.write_without_timeout(AMQ::Protocol::HeartbeatFrame.encode, true)
      end
    end
  end
end
