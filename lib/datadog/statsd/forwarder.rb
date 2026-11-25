# frozen_string_literal: true

module Datadog
  class Statsd
    class Forwarder
      SCOPE_STACK_KEY = :datadog_statsd_scope_stack

      attr_reader :telemetry
      attr_reader :transport_type
      attr_reader :message_buffer

      # Collects the original messages accepted inside an isolated scope. The
      # embedded MessageBuffer enforces payload/pool limits; packets it flushes
      # into the capture connection do not need to be retained because the
      # original messages are republished on successful scope completion.
      class IsolatedScope
        attr_reader :messages

        def initialize(buffer, capture_connection)
          @buffer = buffer
          @capture_connection = capture_connection
          @messages = []
        end

        def add(message)
          accepted = @buffer.add(message)
          return accepted unless accepted

          @messages << message
          @capture_connection.packets.clear
          accepted
        end
      end

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

        @buffer_max_payload_size = buffer_max_payload_size
        @buffer_max_pool_size = buffer_max_pool_size || DEFAULT_BUFFER_POOL_SIZE
        @buffer_overflowing_stategy = buffer_overflowing_stategy
        @serializer = serializer

        @message_buffer = MessageBuffer.new(@connection,
          max_payload_size: @buffer_max_payload_size,
          max_pool_size: @buffer_max_pool_size,
          overflowing_stategy: @buffer_overflowing_stategy,
          serializer: @serializer
        )

        sender_queue_size ||= 1 if single_thread
        sender_queue_size ||= (@transport_type == :udp ?
                               UDP_DEFAULT_SENDER_QUEUE_SIZE : UDS_DEFAULT_SENDER_QUEUE_SIZE)

        @sender = single_thread ?
          SingleThreadSender.new(
            @message_buffer,
            logger: logger,
            flush_interval: buffer_flush_interval,
            queue_size: sender_queue_size) :
          Sender.new(
            @message_buffer,
            logger: logger,
            flush_interval: buffer_flush_interval,
            telemetry: @telemetry,
            queue_size: sender_queue_size)
        @sender.start
      end

      def send_message(message)
        if (scope = current_scope)
          scope.add(message)
          return
        end

        sender.add(message)

        tick_telemetry
      end

      # Runs a block with a thread-local, bounded capture buffer. Messages added
      # through this forwarder stay invisible to other threads and to the real
      # connection until the block completes successfully. If the block raises,
      # all captured messages are discarded and the exception is re-raised.
      def isolated_scope
        scope = new_isolated_scope
        scope_stack << scope

        messages = nil
        begin
          yield
          messages = scope.messages
        ensure
          stack = Thread.current[SCOPE_STACK_KEY]
          if stack
            stack.delete(scope)
            Thread.current[SCOPE_STACK_KEY] = nil if stack.empty?
          end
        end

        publish_captured_messages(messages)
      end

      def in_isolated_scope?
        !current_scope.nil?
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

      def close
        sender.stop
        connection.close
      end

      private
      attr_reader :sender
      attr_reader :connection

      def scope_stack
        Thread.current[SCOPE_STACK_KEY] ||= []
      end

      def current_scope
        stack = Thread.current[SCOPE_STACK_KEY]
        stack.last if stack
      end

      def new_isolated_scope
        capture_connection = MessageBuffer::CaptureConnection.new
        buffer = MessageBuffer.new(
          capture_connection,
          max_payload_size: @buffer_max_payload_size,
          max_pool_size: @buffer_max_pool_size,
          overflowing_stategy: @buffer_overflowing_stategy,
          serializer: @serializer
        )

        IsolatedScope.new(buffer, capture_connection)
      end

      def publish_captured_messages(messages)
        return if messages.nil? || messages.empty?

        if (parent_scope = current_scope)
          messages.each { |message| parent_scope.add(message) }
        elsif sender.respond_to?(:add_messages)
          sender.add_messages(messages)
          tick_telemetry
        else
          messages.each { |message| sender.add(message) }
          tick_telemetry
        end
      end

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
