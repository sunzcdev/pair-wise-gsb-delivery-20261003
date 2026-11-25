# frozen_string_literal: true

module Datadog
  class Statsd
    class Forwarder
      attr_reader :telemetry
      attr_reader :transport_type

      def initialize(
        connection_cfg: nil,

        buffer_max_payload_size: nil,
        buffer_max_pool_size: nil,
        buffer_overflowing_stategy: :drop,
        buffer_flush_interval: nil,

        sender_queue_size: nil,

        telemetry_flush_interval: nil,
        global_tags: [],

        single_thread: false,

        logger: nil,
        container_id: nil,
        external_data: nil,
        cardinality: nil,

        serializer:
      )
        @transport_type = connection_cfg.transport_type

        @global_tags = global_tags

        @telemetry = if telemetry_flush_interval
          Telemetry.new(telemetry_flush_interval,
            container_id,
            external_data,
            cardinality,
            global_tags: global_tags,
            transport_type: @transport_type
          )
        else
          nil
        end

        @connection = connection_cfg.make_connection(logger: logger, telemetry: telemetry)

        # Initialize buffer
        buffer_max_payload_size ||= (@transport_type == :udp ?
                                     UDP_DEFAULT_BUFFER_SIZE : UDS_DEFAULT_BUFFER_SIZE)

        if buffer_max_payload_size <= 0
          raise ArgumentError, 'buffer_max_payload_size cannot be <= 0'
        end

        unless telemetry.nil? || telemetry.would_fit_in?(buffer_max_payload_size)
          raise ArgumentError, "buffer_max_payload_size is not high enough to use telemetry (tags=(#{global_tags.inspect}))"
        end

        buffer = MessageBuffer.new(@connection,
          max_payload_size: buffer_max_payload_size,
          max_pool_size: buffer_max_pool_size || DEFAULT_BUFFER_POOL_SIZE,
          overflowing_stategy: buffer_overflowing_stategy,
          serializer: serializer
        )

        sender_queue_size ||= 1 if single_thread
        sender_queue_size ||= (@transport_type == :udp ?
                               UDP_DEFAULT_SENDER_QUEUE_SIZE : UDS_DEFAULT_SENDER_QUEUE_SIZE)

        @sender = single_thread ?
          SingleThreadSender.new(
            buffer,
            logger: logger,
            flush_interval: buffer_flush_interval,
            queue_size: sender_queue_size) :
          Sender.new(
            buffer,
            logger: logger,
            flush_interval: buffer_flush_interval,
            telemetry: @telemetry,
            queue_size: sender_queue_size)
        @sender.start
      end

      def send_message(message)
        sender.add(message)

        tick_telemetry
      end

      def sync_with_outbound_io
        sender.rendez_vous
      end

      def flush(flush_telemetry: false, sync: false)
        do_flush_telemetry if telemetry && flush_telemetry

        sender.flush(sync: sync)
      end

      def host
        return nil unless transport_type == :udp

        connection.host
      end

      def port
        return nil unless transport_type == :udp

        connection.port
      end

      def socket_path
        return nil unless transport_type == :uds

        connection.socket_path
      end

      # Switches the live stats outlet to another UDP endpoint that may use a
      # smaller max packet size.
      #
      # Already sent packets stay on the old endpoint's account while lines
      # still queued or buffered are re-grouped according to the new payload
      # limit and delivered through the new endpoint.
      def switch_connection(host, port, max_payload_size: nil)
        unless @transport_type == :udp
          raise Error, 'switch_connection is only supported for UDP connections'
        end

        if max_payload_size
          raise ArgumentError, 'buffer_max_payload_size cannot be <= 0' if max_payload_size <= 0

          unless telemetry.nil? || telemetry.would_fit_in?(max_payload_size)
            raise ArgumentError, "buffer_max_payload_size is not high enough to use telemetry (tags=(#{global_tags.inspect}))"
          end
        end

        new_connection = ConnectionCfg.new(host: host, port: port)
          .make_connection(logger: connection.logger, telemetry: telemetry)

        sender.switch_connection(new_connection, max_payload_size: max_payload_size)

        # the old socket is now unreachable, release it. The sender has fully
        # drained its queue before switching, so no packet can be written to
        # the old connection anymore.
        connection.close

        @connection = new_connection
      end

      def close
        sender.stop
        connection.close
      end

      private
      attr_reader :sender
      attr_reader :connection
      attr_reader :global_tags

      def do_flush_telemetry
        telemetry_snapshot = telemetry.flush
        telemetry.reset

        telemetry_snapshot.each do |message|
          sender.add(message)
        end
      end

      def tick_telemetry
        return nil unless telemetry

        do_flush_telemetry if telemetry.should_flush?
      end
    end
  end
end
