# frozen_string_literal: true

module HTTPX
  module Plugins
    #
    # This plugin implements convenience methods for Server Sent Events streams.
    #
    # It keeps track of the recovery position of the stream, i.e. the id of the last event
    # which was confirmed by the peer (the last event whose terminating blank line was
    # received, and which was delivered to the caller). That is the position from which a
    # dropped stream can be resumed: the caller can read it via
    # StreamResponseMethods#last_event_id, and pass it as the :last_event_id option of the
    # request which reopens the stream (which sets the "last-event-id" request header).
    #
    # https://gitlab.com/os85/httpx/wikis/Server-Sent-Events
    #
    module ServerSentEvents
      Message = if RUBY_VERSION >= "3.2.0" # rubocop:disable Naming/ConstantName
        Data.define(:data, :event, :id, :retry_after) do
          def initialize(event: nil, id: nil, retry_after: nil, **kwargs)
            super
          end
        end
      else
        Struct.new(:data, :event, :id, :retry_after, keyword_init: true)
      end

      class << self
        def subplugins
          {
            retries: ServerSentEventsRetries,
          }
        end

        def load_dependencies(klass)
          klass.plugin(:stream)
        end
      end

      # adds support for the following options:
      #
      # :event_stream :: whether the request is a server-sent events text event stream (defaults to <tt>false</tt>).
      # :last_event_id :: id of the last event confirmed by the peer, which a request reopening a dropped
      #                   stream should resume from; when set (and not empty), it is sent in the
      #                   "last-event-id" request header (defaults to <tt>nil</tt>).
      module OptionsMethods
        def option_event_stream(val)
          val
        end

        def option_last_event_id(val)
          val.nil? ? val : String(val)
        end
      end

      module InstanceMethods
        # sends a stream request.
        def request(*, **options)
          options[:stream] = true if options[:event_stream]

          super
        end

        def build_request(...)
          super.tap do |request|
            if request.options.event_stream
              request.headers["accept"] = "text/event-stream"
              request.headers["cache-control"] = "no-cache"

              # an explicit resume position, set by the caller reopening a dropped stream.
              event_id = request.options.last_event_id

              request.headers["last-event-id"] = event_id if event_id && !event_id.empty?
            end
          end
        end
      end

      module RequestMethods
        # last event message which was fully received (and yielded to the caller).
        attr_accessor :last_server_sent_message

        # last event id confirmed by the peer, i.e. the id in effect after the last fully
        # received event; it's the position from which the stream can be resumed.
        #
        # It's nil until an event is received; it becomes the empty string when an event
        # with an empty "id" field is received (which resets the resumable position), and
        # is otherwise the value of the "id" field of the last event carrying one.
        attr_accessor :last_server_sent_event_id

        # last reconnection interval (in milliseconds) sent by the peer in a "retry" field
        # of a fully received event.
        attr_accessor :last_server_sent_retry

        def initialize(*)
          super

          @last_server_sent_message = nil
          @last_server_sent_event_id = nil
          @last_server_sent_retry = nil
        end
      end

      module StreamResponseMethods
        # yields each event Message as the server emits them.
        #
        # only events whose terminating blank line was received are yielded. Bytes belonging
        # to a still incomplete line (including a multi-byte character split across reads)
        # are buffered; if the connection drops before the event is terminated, they're
        # discarded, and the resumable position is not moved past the last confirmed event.
        def each_message(&block)
          return enum_for(__method__) unless block

          @message_payload = {}
          @message_buffer = +"".b

          each do |chunk|
            # chunks are buffered as bytes, so that a multi-byte character split across
            # two reads is only interpreted after it is complete.
            @message_buffer << chunk.b

            while (line = shift_message_line)
              if line.empty?
                message = dispatch(@message_payload)

                next unless message

                yield message
              else
                parse_line(line, @message_payload)
              end
            end
          end
        end

        # id of the last event confirmed by the peer, i.e. the value which should be sent in
        # the "last-event-id" header of a request reopening the stream.
        #
        # returns nil if no event was confirmed yet, the empty string if the last confirmed
        # event carried an empty "id" (which resets the position), or the last "id" value.
        def last_event_id
          @request.last_server_sent_event_id
        end

        # drops the bytes of the event which was being received when the stream dropped.
        # This is called when the request is resent (see ServerSentEventsRetries), as the
        # peer is expected to replay that event from the last confirmed position.
        def reset_message_parsing
          @message_payload&.clear
          @message_buffer&.clear
        end

        private

        # returns the next complete line from the read buffer (without its line terminator),
        # or nil when a full line hasn't been read yet.
        def shift_message_line
          buffer = @message_buffer
          idx = buffer.index("\n")

          return unless idx

          line = buffer.byteslice(0, idx)
          @message_buffer = buffer.byteslice((idx + 1)..-1)

          # the event stream format allows both "\r\n" and "\n" as line terminators.
          line = line.byteslice(0, line.bytesize - 1) if line.end_with?("\r")

          line
        end

        # processes an event which has just been terminated by a blank line.
        #
        # returns the Message to deliver to the caller, or nil if the event is not to be
        # delivered (i.e. it was comment-only, or carried no "data" field).
        def dispatch(payload)
          return if payload.empty?

          message = nil

          # comment-carrying blocks are not events, and are not delivered.
          # an event carrying no data has nothing to deliver either.
          if payload.key?(:data) && !payload.key?(:comment)
            message = Message.new(
              data: to_text(payload[:data]),
              event: to_text(payload[:event]),
              id: to_text(payload[:id]),
              retry_after: payload[:retry_after]
            )

            @request.last_server_sent_message = message

            # the recovery position corresponds to the events actually delivered to the
            # caller: an event carrying an "id" moves the position (an empty "id" resets it
            # to the empty string), an event without an "id" leaves it untouched.
            @request.last_server_sent_event_id = message.id if payload.key?(:id)
          end

          # a "retry" field reconfigures the reconnection interval of the stream, whether or
          # not the event carrying it has payload to deliver.
          @request.last_server_sent_retry = payload[:retry_after] if payload.key?(:retry_after)

          message
        ensure
          payload.clear
        end

        # event stream text is UTF-8; values are parsed as bytes (so that a character split
        # across reads isn't mangled), and decoded here, once the line is complete.
        def to_text(value)
          return if value.nil?

          text = value.dup.force_encoding(Encoding::UTF_8)

          text.valid_encoding? ? text : value
        end

        def parse_line(line, payload)
          type, value = line.split(":", 2)

          if value.nil?
            value = ""
          elsif value.start_with?(" ")
            value = value.byteslice(1..-1)
          end

          case type
          when "data"
            if payload.key?(:data)
              payload[:data] << "\n" << value
            else
              payload[:data] = value
            end
          when "id", "event"
            payload[type.to_sym] = value
          when "retry"
            # integer milliseconds; invalid values are ignored, per spec.
            payload[:retry_after] = value if value.match?(/\A\d+\z/)
          when ""
            # lines starting with ":" are comments.
            payload[:comment] = true if line.start_with?(":")
          end
          # unknown fields are ignored, per spec.
        end
      end

      module ServerSentEventsRetries
        module InstanceMethods
          private

          def prepare_to_retry(request, *)
            super

            # the event which was in-flight when the stream dropped is going to be replayed
            # by the peer from the last confirmed position, so its partial bytes must go.
            request.stream&.reset_message_parsing

            # resume from the last event actually confirmed by the peer. an empty id resets
            # the position, in which case the header must not be sent.
            event_id = request.last_server_sent_event_id

            case event_id
            when nil
              # no event was confirmed yet: keep whatever the caller explicitly set.
            when ""
              request.headers.delete("last-event-id")
            else
              request.headers["last-event-id"] = event_id
            end
          end

          def when_to_retry(request, *)
            # an explicitly configured reconnection policy takes precedence.
            retry_after = super

            return retry_after if retry_after

            # otherwise, honor the interval last advertised by the peer in a "retry" field.
            server_retry = request.last_server_sent_retry

            return unless server_retry

            # the "retry" field is expressed in milliseconds.
            server_retry.to_i / 1_000.0
          end
        end
      end
    end
    register_plugin(:server_sent_events, ServerSentEvents)
  end
end
