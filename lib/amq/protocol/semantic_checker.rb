# frozen_string_literal: true

require "amq/protocol/client"

module AMQ
  module Protocol
    # Standalone AMQP 0-9-1 semantic checker.
    #
    # Unlike Frame.encode, which only proves that a channel number fits in two
    # bytes, the checker validates frame type, channel scope, content class and
    # the allowed payload structure against the same method descriptions
    # (AMQ::Protocol::METHODS, Frame::TYPES and Basic::DECODE_PROPERTIES*) the
    # real encoder uses. It never calls an encode or decode method, so a packet
    # that merely packs successfully is never assumed to be protocol compliant.
    #
    # Unknown methods or content classes stay in a reviewable unsupported
    # state: they are reported as violations but never handed to any handler.
    module SemanticChecker
      # Rule identifiers. Every violation carries one of them together with the
      # original field position it was detected at.
      RULE_FRAME_NIL                = :frame_nil
      RULE_FRAME_TYPE_UNKNOWN       = :frame_type_unknown
      RULE_PAYLOAD_NIL              = :payload_nil
      RULE_CHANNEL_OUT_OF_RANGE     = :channel_out_of_range
      RULE_HEARTBEAT_CHANNEL        = :heartbeat_channel_must_be_zero
      RULE_HEARTBEAT_PAYLOAD_EMPTY  = :heartbeat_payload_must_be_empty
      RULE_METHOD_PAYLOAD_TOO_SHORT = :method_payload_must_start_with_class_and_method_ids
      RULE_METHOD_UNSUPPORTED       = :method_not_supported
      RULE_CONNECTION_CHANNEL_ZERO  = :connection_methods_must_use_channel_zero
      RULE_CHANNEL_METHOD_NONZERO   = :channel_scoped_methods_must_use_nonzero_channel
      RULE_HEADER_PAYLOAD_TOO_SHORT = :header_payload_must_contain_class_weight_size_and_flags
      RULE_HEADER_WEIGHT            = :header_weight_must_be_zero
      RULE_HEADER_CLASS_UNSUPPORTED = :content_class_not_supported
      RULE_HEADER_PROPERTY_FLAGS    = :header_uses_unknown_basic_property_flags
      RULE_HEADER_PROPERTY_TRUNCATED = :header_property_values_are_truncated
      RULE_BODY_CHANNEL_NONZERO     = :body_frames_must_use_nonzero_channel
      RULE_FRAMESET_EMPTY           = :frameset_must_not_be_empty
      RULE_FRAMESET_CHANNEL_MIXED   = :frameset_frames_must_share_one_channel
      RULE_FRAMESET_FIRST_FRAME     = :frameset_must_start_with_a_method_frame
      RULE_FRAMESET_HEARTBEAT       = :heartbeat_frames_stand_alone
      RULE_FRAMESET_HEADER_EXPECTED = :content_method_must_be_followed_by_a_header_frame
      RULE_FRAMESET_HEADER_FORBIDDEN = :contentless_method_must_not_be_followed_by_a_header_frame
      RULE_FRAMESET_BODY_ORDER      = :body_frames_only_follow_the_header_frame
      RULE_FRAMESET_HEADER_DUPLICATE = :frameset_must_contain_a_single_header_frame
      RULE_FRAMESET_BODY_SIZE       = :header_body_size_must_equal_the_sum_of_body_frame_sizes
      RULE_FRAMESET_CONTENT_CLASS   = :header_content_class_must_match_the_method_class

      # Basic content is the only content class defined in AMQP 0-9-1 and the
      # only one the generated encoder can emit header frames for.
      BASIC_CLASS_ID = Basic.method_id

      # Offset of the content class id, weight, body size and property flags in
      # a header frame payload.
      HEADER_CLASS_ID_OFFSET  = 0
      HEADER_WEIGHT_OFFSET    = 2
      HEADER_BODY_SIZE_OFFSET = 4
      HEADER_FLAGS_OFFSET     = 12
      HEADER_MIN_PAYLOAD_SIZE = 14

      METHOD_IDS_SIZE = 4

      Violation = Struct.new(:rule, :field, :location, :message, :details, keyword_init: true) do
        def to_s
          "#{rule} at #{location}: #{message}"
        end
      end

      class Report
        attr_reader :violations

        def initialize(violations = [])
          @violations = violations
        end

        def ok?
          @violations.empty?
        end

        # Unknown methods and content classes remain unsupported and reviewable
        # instead of being dispatched to a handler.
        def supported?
          @violations.none? { |violation|
            violation.rule == RULE_METHOD_UNSUPPORTED || violation.rule == RULE_HEADER_CLASS_UNSUPPORTED
          }
        end
      end

      class << self
        # Checks a single frame (an AMQ::Protocol::Frame subclass) against the
        # frame-level rules. Frame objects are inspected only; no encode,
        # decode or handler method is invoked.
        def check_frame(frame)
          Report.new(check_frame_into([], frame, "frame"))
        end

        # Checks a complete frameset: one method frame, optionally followed by a
        # header frame and body frames for content-bearing methods.
        def check_frameset(frames)
          violations = []

          unless frames.is_a?(Array) && !frames.empty?
            violations << violation(RULE_FRAMESET_EMPTY, "frameset", "frameset",
                                    "a frameset must contain at least one frame",
                                    {:given => frames.inspect})
            return Report.new(violations)
          end

          frames.each_with_index do |frame, index|
            check_frame_into(violations, frame, "frameset[#{index}]")
          end

          check_frameset_structure(violations, frames)
          Report.new(violations)
        end

        # Resolves a method class from the class and method ids exactly like
        # MethodFrame#method_class does, using the shared METHODS description.
        # Returns nil for unknown methods instead of guessing.
        def method_class_for(class_id, method_id)
          METHODS.fetch(class_id << 16 | method_id, nil)
        end

        # Class id of the Protocol::Class a method belongs to, taken from the
        # shared method description rather than recomputed by the caller.
        def connection_method?(method_class)
          method_class.name.start_with?("connection.")
        end

        private

        def check_frame_into(violations, frame, location)
          if frame.nil?
            violations << violation(RULE_FRAME_NIL, "frame", location,
                                    "frame must not be nil", {})
            return violations
          end

          type_id = frame_type_id(frame)
          unless Frame::TYPES_REVERSE.key?(type_id)
            violations << violation(RULE_FRAME_TYPE_UNKNOWN, "type", "#{location}.type",
                                    "frame type must be one of #{Frame::TYPES_OPTIONS.inspect}",
                                    {:type => frame.respond_to?(:class) ? frame.class.id : nil})
            return violations
          end

          check_channel(violations, frame, location)
          check_payload_presence(violations, frame, location)

          case type_id
          when Frame::TYPES[:heartbeat]
            check_heartbeat(violations, frame, location)
          when Frame::TYPES[:method]
            check_method_frame(violations, frame, location)
          when Frame::TYPES[:headers]
            check_header_frame(violations, frame, location)
          when Frame::TYPES[:body]
            check_body_frame(violations, frame, location)
          end

          violations
        end

        def check_channel(violations, frame, location)
          channel = frame.channel
          return if Frame::CHANNEL_RANGE.include?(channel)

          violations << violation(RULE_CHANNEL_OUT_OF_RANGE, "channel", "#{location}.channel",
                                  "channel must be an integer in 0..65535",
                                  {:channel => channel, :range => Frame::CHANNEL_RANGE.to_a.values_at(0, -1)})
        end

        def check_payload_presence(violations, frame, location)
          return unless frame.payload.nil?

          violations << violation(RULE_PAYLOAD_NIL, "payload", "#{location}.payload",
                                  "payload can't be nil", {})
        end

        def check_heartbeat(violations, frame, location)
          unless frame.channel == 0
            violations << violation(RULE_HEARTBEAT_CHANNEL, "channel", "#{location}.channel",
                                    "heartbeat frames are connection-wide and must use channel 0",
                                    {:channel => frame.channel})
          end

          if frame.payload && !frame.payload.empty?
            violations << violation(RULE_HEARTBEAT_PAYLOAD_EMPTY, "payload", "#{location}.payload",
                                    "heartbeat frames must carry an empty payload",
                                    {:payload_bytesize => frame.payload.bytesize})
          end
        end

        def check_method_frame(violations, frame, location)
          payload = frame.payload
          return if payload.nil?

          if payload.bytesize < METHOD_IDS_SIZE
            violations << violation(RULE_METHOD_PAYLOAD_TOO_SHORT, "payload", "#{location}.payload",
                                    "method frame payload must start with the 2-byte class id and 2-byte method id",
                                    {:payload_bytesize => payload.bytesize, :required => METHOD_IDS_SIZE})
            return
          end

          class_id, method_id = payload.byteslice(0, 4).unpack(PACK_UINT16_X2)
          method_class = method_class_for(class_id, method_id)

          if method_class.nil?
            violations << violation(RULE_METHOD_UNSUPPORTED, "payload", "#{location}.payload[0..3]",
                                    "no AMQP 0-9-1 method is described for class id #{class_id}, method id #{method_id}; it is left unsupported and not processed",
                                    {:class_id => class_id, :method_id => method_id})
            return
          end

          check_channel_scope(violations, frame, location, method_class)
        end

        def check_channel_scope(violations, frame, location, method_class)
          channel = frame.channel

          if connection_method?(method_class)
            unless channel == 0
              violations << violation(RULE_CONNECTION_CHANNEL_ZERO, "channel", "#{location}.channel",
                                      "#{method_class.name} is a connection method and must be sent on channel 0",
                                      {:method => method_class.name, :channel => channel})
            end
          else
            unless channel && channel > 0
              violations << violation(RULE_CHANNEL_METHOD_NONZERO, "channel", "#{location}.channel",
                                      "#{method_class.name} is channel-scoped and must be sent on a nonzero channel",
                                      {:method => method_class.name, :channel => channel})
            end
          end
        end

        def check_header_frame(violations, frame, location)
          payload = frame.payload
          return if payload.nil?

          if payload.bytesize < HEADER_MIN_PAYLOAD_SIZE
            violations << violation(RULE_HEADER_PAYLOAD_TOO_SHORT, "payload", "#{location}.payload",
                                    "header frame payload must contain class id, weight, body size and property flags (#{HEADER_MIN_PAYLOAD_SIZE} bytes)",
                                    {:payload_bytesize => payload.bytesize, :required => HEADER_MIN_PAYLOAD_SIZE})
            return
          end

          class_id = payload.byteslice(HEADER_CLASS_ID_OFFSET, 2).unpack1(PACK_UINT16)
          weight   = payload.byteslice(HEADER_WEIGHT_OFFSET, 2).unpack1(PACK_UINT16)

          unless weight == 0
            violations << violation(RULE_HEADER_WEIGHT, "weight", "#{location}.payload[2..3]",
                                    "header frame weight must be 0",
                                    {:weight => weight})
          end

          unless class_id == BASIC_CLASS_ID
            violations << violation(RULE_HEADER_CLASS_UNSUPPORTED, "class_id", "#{location}.payload[0..1]",
                                    "content class id #{class_id} is not described in AMQP 0-9-1; only basic content (class #{BASIC_CLASS_ID}) is supported",
                                    {:class_id => class_id, :supported_class_id => BASIC_CLASS_ID})
            return
          end

          check_basic_property_flags(violations, payload, location)
        end

        # Validates the property flags and walks the property values using the
        # same flag and type descriptions Basic.decode_properties uses, without
        # instantiating any property values or invoking a decode callback.
        def check_basic_property_flags(violations, payload, location)
          flags = payload.byteslice(HEADER_FLAGS_OFFSET, 2).unpack1(PACK_UINT16)
          property_data = payload.byteslice(HEADER_MIN_PAYLOAD_SIZE..-1)
          offset = 0

          Basic::DECODE_PROPERTIES_KEYS.each do |flag|
            next unless flags >= flag

            flags -= flag
            type = Basic::DECODE_PROPERTIES_TYPE[flag]
            consumed = property_value_size(type, property_data, offset)
            if consumed.nil?
              violations << violation(RULE_HEADER_PROPERTY_TRUNCATED, "properties",
                                      "#{location}.payload[#{HEADER_MIN_PAYLOAD_SIZE + offset}..-1]",
                                      "#{Basic::DECODE_PROPERTIES[flag]} property value is truncated or has a bad length prefix",
                                      {:property => Basic::DECODE_PROPERTIES[flag], :type => type})
              return
            end
            offset += consumed
          end

          if flags != 0
            violations << violation(RULE_HEADER_PROPERTY_FLAGS, "property_flags", "#{location}.payload[12..13]",
                                    "header property flags contain unknown bits 0x#{flags.to_s(16).upcase}",
                                    {:unknown_flag_bits => flags})
          end

          if flags == 0 && offset != property_data.bytesize
            violations << violation(RULE_HEADER_PROPERTY_TRUNCATED, "properties",
                                    "#{location}.payload[#{HEADER_MIN_PAYLOAD_SIZE + offset}..-1]",
                                    "header frame carries #{property_data.bytesize - offset} trailing byte(s) after the declared property values",
                                    {:trailing_bytes => property_data.bytesize - offset})
          end
        end

        def property_value_size(type, data, offset)
          case type
          when :octet
            return 1 if data.bytesize >= offset + 1
          when :timestamp
            return 8 if data.bytesize >= offset + 8
          when :shortstr
            return nil if data.bytesize < offset + 1

            length = data.getbyte(offset)
            return 1 + length if data.bytesize >= offset + 1 + length
          when :table
            return nil if data.bytesize < offset + 4

            length = data.byteslice(offset, 4).unpack1(PACK_UINT32)
            return 4 + length if data.bytesize >= offset + 4 + length
          end
          nil
        end

        def check_body_frame(violations, frame, location)
          return unless frame.channel && frame.channel.zero?

          violations << violation(RULE_BODY_CHANNEL_NONZERO, "channel", "#{location}.channel",
                                  "body frames carry channel content and must use a nonzero channel",
                                  {:channel => frame.channel})
        end

        def check_frameset_structure(violations, frames)
          channels = frames.map { |frame| frame.nil? ? nil : frame.channel }.uniq
          if channels.size > 1
            violations << violation(RULE_FRAMESET_CHANNEL_MIXED, "channel", "frameset[*].channel",
                                    "all frames of a frameset must share one channel",
                                    {:channels => channels})
          end

          first = frames.first
          return if first.nil?

          if heartbeat?(first)
            violations << violation(RULE_FRAMESET_HEARTBEAT, "frameset", "frameset[0]",
                                    "a heartbeat frame stands alone and cannot open a content frameset",
                                    {})
            return
          end

          unless method_frame?(first)
            violations << violation(RULE_FRAMESET_FIRST_FRAME, "frameset", "frameset[0]",
                                    "a frameset must start with a method frame",
                                    {:first_frame_type => frame_type_id(first)})
            return
          end

          method_class = resolve_method(first)
          return if method_class.nil? # already reported as unsupported

          rest = frames[1..-1]
          if method_class.has_content?
            check_content_frameset(violations, first, rest, method_class)
          else
            check_contentless_frameset(violations, rest, method_class)
          end
        end

        def check_content_frameset(violations, method_frame, rest, method_class)
          header_indexes = rest.each_index.select { |i| header_frame?(rest[i]) }
          body_indexes   = rest.each_index.select { |i| body_frame?(rest[i]) }
          heartbeat_indexes = rest.each_index.select { |i| heartbeat?(rest[i]) }

          heartbeat_indexes.each do |i|
            violations << violation(RULE_FRAMESET_HEARTBEAT, "frameset", "frameset[#{i + 1}]",
                                    "a heartbeat frame cannot be part of a content frameset", {})
          end

          if header_indexes.empty?
            violations << violation(RULE_FRAMESET_HEADER_EXPECTED, "frameset", "frameset[1]",
                                    "#{method_class.name} carries content and must be followed by a header frame",
                                    {:method => method_class.name})
            return
          end

          if header_indexes.size > 1
            violations << violation(RULE_FRAMESET_HEADER_DUPLICATE, "frameset",
                                    "frameset[#{header_indexes[1] + 1}]",
                                    "a content frameset must contain exactly one header frame",
                                    {:header_frame_positions => header_indexes.map { |i| i + 1 }})
          end

          header_index = header_indexes.first
          unless header_index == 0
            violations << violation(RULE_FRAMESET_BODY_ORDER, "frameset", "frameset[#{header_index + 1}]",
                                    "the header frame must immediately follow the method frame",
                                    {:position => header_index + 1})
          end

          if body_indexes.any? { |i| i < header_index }
            violations << violation(RULE_FRAMESET_BODY_ORDER, "frameset", "frameset[*]",
                                    "body frames must follow the header frame", {})
          end

          check_header_matches_method(violations, method_frame, rest[header_index], method_class)
          check_body_size(violations, rest, header_index)
        end

        def check_contentless_frameset(violations, rest, method_class)
          rest.each_with_index do |frame, i|
            next if frame.nil?

            if header_frame?(frame)
              violations << violation(RULE_FRAMESET_HEADER_FORBIDDEN, "frameset", "frameset[#{i + 1}]",
                                      "#{method_class.name} carries no content and cannot be followed by a header frame",
                                      {:method => method_class.name})
            elsif body_frame?(frame)
              violations << violation(RULE_FRAMESET_BODY_ORDER, "frameset", "frameset[#{i + 1}]",
                                      "#{method_class.name} carries no content and cannot be followed by body frames",
                                      {:method => method_class.name})
            elsif heartbeat?(frame)
              violations << violation(RULE_FRAMESET_HEARTBEAT, "frameset", "frameset[#{i + 1}]",
                                      "a heartbeat frame cannot be part of a frameset", {})
            end
          end
        end

        def check_header_matches_method(violations, method_frame, header_frame, method_class)
          return if header_frame.nil? || header_frame.payload.nil? || method_frame.payload.nil?
          return if header_frame.payload.bytesize < HEADER_MIN_PAYLOAD_SIZE

          header_class_id = header_frame.payload.byteslice(HEADER_CLASS_ID_OFFSET, 2).unpack1(PACK_UINT16)
          method_class_id = method_frame.payload.byteslice(0, 2).unpack1(PACK_UINT16)

          return if header_class_id == method_class_id

          violations << violation(RULE_FRAMESET_CONTENT_CLASS, "class_id", "frameset[1].payload[0..1]",
                                  "header content class #{header_class_id} does not match method class #{method_class_id} (#{method_class.name})",
                                  {:header_class_id => header_class_id, :method_class_id => method_class_id,
                                   :method => method_class.name})
        end

        def check_body_size(violations, rest, header_index)
          header_frame = rest[header_index]
          return if header_frame.nil? || header_frame.payload.nil?
          return if header_frame.payload.bytesize < HEADER_MIN_PAYLOAD_SIZE

          declared_size = header_frame.payload.byteslice(HEADER_BODY_SIZE_OFFSET, 8).unpack1(PACK_UINT64_BE)
          actual_size = rest[(header_index + 1)..-1].reject(&:nil?)
                           .select { |frame| body_frame?(frame) }
                           .sum(0) { |frame| frame.payload ? frame.payload.bytesize : 0 }

          return if declared_size == actual_size

          violations << violation(RULE_FRAMESET_BODY_SIZE, "body_size", "frameset[1].payload[4..11]",
                                  "header body size #{declared_size} does not match the #{actual_size} byte(s) carried by body frames",
                                  {:declared_body_size => declared_size, :actual_body_size => actual_size})
        end

        def resolve_method(method_frame)
          payload = method_frame.payload
          return nil if payload.nil? || payload.bytesize < METHOD_IDS_SIZE

          class_id, method_id = payload.byteslice(0, 4).unpack(PACK_UINT16_X2)
          method_class_for(class_id, method_id)
        end

        def frame_type_id(frame)
          frame.class.id
        end

        def method_frame?(frame)
          !frame.nil? && frame.class.id == Frame::TYPES[:method]
        end

        def header_frame?(frame)
          !frame.nil? && frame.class.id == Frame::TYPES[:headers]
        end

        def body_frame?(frame)
          !frame.nil? && frame.class.id == Frame::TYPES[:body]
        end

        def heartbeat?(frame)
          !frame.nil? && frame.class.id == Frame::TYPES[:heartbeat]
        end

        def violation(rule, field, location, message, details)
          Violation.new(:rule => rule, :field => field, :location => location,
                        :message => message, :details => details)
        end
      end
    end
  end
end
