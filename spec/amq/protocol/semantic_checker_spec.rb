# encoding: binary

module AMQ
  module Protocol
    RSpec.describe SemanticChecker do
      def method_frame(class_id, method_id, channel)
        MethodFrame.new([class_id, method_id].pack(PACK_UINT16_X2), channel)
      end

      def header_payload(class_id: Basic.method_id, weight: 0, body_size: 0, flags: 0, properties: "")
        [class_id, weight].pack(PACK_UINT16_X2) +
          AMQ::Pack.pack_uint64_big_endian(body_size) +
          [flags].pack(PACK_UINT16) +
          properties
      end

      def rules(report)
        report.violations.map(&:rule)
      end

      describe ".check_frame" do
        it "rejects a nil frame and reports its field location" do
          report = described_class.check_frame(nil)

          expect(report.ok?).to eq(false)
          expect(rules(report)).to include(SemanticChecker::RULE_FRAME_NIL)
          expect(report.violations.first.field).to eq("frame")
          expect(report.violations.first.location).to eq("frame")
        end

        it "rejects a frame object that does not describe a known frame type" do
          unknown_frame = ::Class.new do
            def self.id
              99
            end

            def channel
              0
            end

            def payload
              ""
            end
          end.new

          report = described_class.check_frame(unknown_frame)

          expect(rules(report)).to include(SemanticChecker::RULE_FRAME_TYPE_UNKNOWN)
        end

        it "does not accept a packet merely because its channel number is in range" do
          frame = method_frame(Connection.method_id, Connection::Open.method_id, 7)

          report = described_class.check_frame(frame)

          expect(Frame::CHANNEL_RANGE.include?(7)).to eq(true)
          expect(report.ok?).to eq(false)
          expect(rules(report)).to eq([SemanticChecker::RULE_CONNECTION_CHANNEL_ZERO])
          expect(report.violations.first.details[:method]).to eq("connection.open")
        end

        it "accepts a connection method on channel 0" do
          frame = method_frame(Connection.method_id, Connection::Open.method_id, 0)

          expect(described_class.check_frame(frame)).to be_ok
        end

        it "rejects a channel-scoped method sent on channel 0" do
          frame = method_frame(Channel.method_id, Channel::Open.method_id, 0)

          report = described_class.check_frame(frame)

          expect(rules(report)).to eq([SemanticChecker::RULE_CHANNEL_METHOD_NONZERO])
          expect(report.violations.first.location).to eq("frame.channel")
        end

        it "accepts a channel-scoped method on a nonzero channel" do
          frame = method_frame(Basic.method_id, Basic::Qos.method_id, 1)

          expect(described_class.check_frame(frame)).to be_ok
        end

        it "rejects out of range channel numbers" do
          frame = method_frame(Basic.method_id, Basic::Qos.method_id, 65536)

          expect(rules(described_class.check_frame(frame)))
            .to include(SemanticChecker::RULE_CHANNEL_OUT_OF_RANGE)
        end

        it "rejects a nil payload without attempting to read it" do
          frame = MethodFrame.new(nil, 1)

          report = described_class.check_frame(frame)

          expect(rules(report)).to include(SemanticChecker::RULE_PAYLOAD_NIL)
        end

        it "rejects a method payload shorter than the class and method ids" do
          frame = MethodFrame.new("\x00\x3C", 1)

          report = described_class.check_frame(frame)

          expect(rules(report)).to eq([SemanticChecker::RULE_METHOD_PAYLOAD_TOO_SHORT])
          expect(report.violations.first.location).to eq("frame.payload")
        end

        it "keeps unknown methods in an unsupported, reviewable state" do
          frame = method_frame(99, 99, 1)

          report = described_class.check_frame(frame)

          expect(report.ok?).to eq(false)
          expect(report.supported?).to eq(false)
          expect(rules(report)).to eq([SemanticChecker::RULE_METHOD_UNSUPPORTED])
          expect(report.violations.first.location).to eq("frame.payload[0..3]")
          expect(report.violations.first.details).to eq(:class_id => 99, :method_id => 99)
          expect(described_class.method_class_for(99, 99)).to be_nil
        end

        it "resolves known methods through the shared METHODS description" do
          expect(described_class.method_class_for(60, 40)).to eq(Basic::Publish)
          expect(described_class.method_class_for(10, 40)).to eq(Connection::Open)
        end

        it "rejects heartbeat frames outside channel 0" do
          report = described_class.check_frame(HeartbeatFrame.new("", 3))

          expect(rules(report)).to eq([SemanticChecker::RULE_HEARTBEAT_CHANNEL])
        end

        it "rejects heartbeat frames carrying a payload" do
          report = described_class.check_frame(HeartbeatFrame.new("x", 0))

          expect(rules(report)).to eq([SemanticChecker::RULE_HEARTBEAT_PAYLOAD_EMPTY])
        end

        it "accepts a valid heartbeat frame" do
          expect(described_class.check_frame(HeartbeatFrame.new("", 0))).to be_ok
        end

        it "rejects body frames on channel 0" do
          report = described_class.check_frame(BodyFrame.new("data", 0))

          expect(rules(report)).to eq([SemanticChecker::RULE_BODY_CHANNEL_NONZERO])
        end

        it "rejects header frames whose payload is too short" do
          frame = HeaderFrame.new("\x00<\x00\x00", 1)

          expect(rules(described_class.check_frame(frame)))
            .to include(SemanticChecker::RULE_HEADER_PAYLOAD_TOO_SHORT)
        end

        it "rejects header frames with a nonzero weight" do
          payload = header_payload(:weight => 9)
          frame = HeaderFrame.new(payload, 1)

          report = described_class.check_frame(frame)

          expect(rules(report)).to include(SemanticChecker::RULE_HEADER_WEIGHT)
          expect(report.violations.first.location).to eq("frame.payload[2..3]")
        end

        it "keeps unknown content classes in an unsupported state" do
          frame = HeaderFrame.new(header_payload(:class_id => 70), 1)

          report = described_class.check_frame(frame)

          expect(rules(report)).to include(SemanticChecker::RULE_HEADER_CLASS_UNSUPPORTED)
          expect(report.supported?).to eq(false)
        end

        it "rejects header frames with unknown property flag bits" do
          frame = HeaderFrame.new(header_payload(:flags => 0x0002), 1)

          report = described_class.check_frame(frame)

          expect(rules(report)).to include(SemanticChecker::RULE_HEADER_PROPERTY_FLAGS)
          expect(report.violations.first.location).to eq("frame.payload[12..13]")
        end

        it "rejects header frames whose property values are truncated" do
          truncated_properties = [5].pack(PACK_CHAR) + "ab"
          frame = HeaderFrame.new(header_payload(:flags => 0x8000, :properties => truncated_properties), 1)

          report = described_class.check_frame(frame)

          expect(rules(report)).to include(SemanticChecker::RULE_HEADER_PROPERTY_TRUNCATED)
          expect(report.violations.first.details[:property]).to eq(:content_type)
        end

        it "validates the fixed width of timestamp properties" do
          frame = HeaderFrame.new(header_payload(:flags => 0x0040, :properties => "short"), 1)

          expect(rules(described_class.check_frame(frame)))
            .to include(SemanticChecker::RULE_HEADER_PROPERTY_TRUNCATED)
        end

        it "validates table property length prefixes" do
          truncated_table = [10].pack(PACK_UINT32) + "x"
          frame = HeaderFrame.new(header_payload(:flags => 0x2000, :properties => truncated_table), 1)

          expect(rules(described_class.check_frame(frame)))
            .to include(SemanticChecker::RULE_HEADER_PROPERTY_TRUNCATED)
        end

        it "rejects trailing property bytes" do
          properties = [1].pack(PACK_CHAR) + "x" + "extra"
          frame = HeaderFrame.new(header_payload(:flags => 0x8000, :properties => properties), 1)

          expect(rules(described_class.check_frame(frame)))
            .to include(SemanticChecker::RULE_HEADER_PROPERTY_TRUNCATED)
        end

        it "accepts a header frame produced from the shared basic property description" do
          payload = Basic.encode_properties(0, {:content_type => "text/plain", :delivery_mode => 2})
          frame = HeaderFrame.new(payload, 1)

          expect(described_class.check_frame(frame)).to be_ok
        end
      end

      describe ".check_frameset" do
        let(:publish_method) { method_frame(Basic.method_id, Basic::Publish.method_id, 1) }
        let(:qos_method) { method_frame(Basic.method_id, Basic::Qos.method_id, 1) }

        it "rejects an empty frameset" do
          report = described_class.check_frameset([])

          expect(rules(report)).to eq([SemanticChecker::RULE_FRAMESET_EMPTY])
        end

        it "rejects a non-array frameset" do
          expect(rules(described_class.check_frameset(nil)))
            .to eq([SemanticChecker::RULE_FRAMESET_EMPTY])
        end

        it "accepts a complete publish frameset built by the real encoder" do
          frames = Basic::Publish.encode(1, "hello", {:content_type => "text/plain"},
                                         "amq.fanout", "rk", false, false, 131072)

          report = described_class.check_frameset(frames)

          expect(report).to be_ok
          expect(report).to be_supported
        end

        it "accepts a content frameset with an empty body" do
          header = HeaderFrame.new(header_payload(:flags => 0x8000,
                                                  :properties => [1].pack(PACK_CHAR) + "x"), 1)

          report = described_class.check_frameset([publish_method, header])

          expect(report).to be_ok
        end

        it "rejects frames spread over several channels" do
          header = HeaderFrame.new(header_payload, 2)

          report = described_class.check_frameset([publish_method, header])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_CHANNEL_MIXED)
        end

        it "requires the first frame to be a method frame" do
          report = described_class.check_frameset([BodyFrame.new("data", 1)])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_FIRST_FRAME)
        end

        it "treats heartbeat frames as standalone" do
          report = described_class.check_frameset([HeartbeatFrame.new("", 0)])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_HEARTBEAT)
        end

        it "requires a header frame after a content-bearing method" do
          report = described_class.check_frameset([publish_method])

          expect(rules(report)).to eq([SemanticChecker::RULE_FRAMESET_HEADER_EXPECTED])
        end

        it "forbids a header frame after a contentless method" do
          header = HeaderFrame.new(header_payload, 1)

          report = described_class.check_frameset([qos_method, header])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_HEADER_FORBIDDEN)
        end

        it "forbids body frames after a contentless method" do
          report = described_class.check_frameset([qos_method, BodyFrame.new("data", 1)])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_BODY_ORDER)
        end

        it "treats heartbeats after a contentless method as standalone" do
          report = described_class.check_frameset([qos_method, HeartbeatFrame.new("", 0)])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_HEARTBEAT)
        end

        it "forbids heartbeats inside a content frameset" do
          header = HeaderFrame.new(header_payload, 1)

          report = described_class.check_frameset([publish_method, header, HeartbeatFrame.new("", 0)])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_HEARTBEAT)
        end

        it "requires body frames to follow the header" do
          body = BodyFrame.new("data", 1)
          header = HeaderFrame.new(header_payload(:body_size => 4), 1)

          report = described_class.check_frameset([publish_method, body, header])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_BODY_ORDER)
        end

        it "rejects framesets with more than one header frame" do
          header = HeaderFrame.new(header_payload, 1)

          report = described_class.check_frameset([publish_method, header, header])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_HEADER_DUPLICATE)
        end

        it "checks the declared body size against the body frames" do
          header = HeaderFrame.new(header_payload(:body_size => 99), 1)
          body = BodyFrame.new("hello", 1)

          report = described_class.check_frameset([publish_method, header, body])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_BODY_SIZE)
          expect(report.violations.first.details)
            .to eq(:declared_body_size => 99, :actual_body_size => 5)
        end

        it "accepts a body size that matches the sum of the body frames" do
          header = HeaderFrame.new(header_payload(:body_size => 11), 1)
          bodies = [BodyFrame.new("hello", 1), BodyFrame.new(" world", 1)]

          expect(described_class.check_frameset([publish_method, header, *bodies])).to be_ok
        end

        it "rejects a header whose content class does not match the method class" do
          header = HeaderFrame.new(header_payload(:class_id => 70), 1)

          report = described_class.check_frameset([publish_method, header])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAMESET_CONTENT_CLASS)
          expect(report.violations.first.location).to eq("frameset[1].payload[0..1]")
        end

        it "does not run any processing for an unsupported method in a frameset" do
          unknown = method_frame(99, 99, 1)

          report = described_class.check_frameset([unknown])

          expect(report.supported?).to eq(false)
          expect(rules(report)).to include(SemanticChecker::RULE_METHOD_UNSUPPORTED)
        end

        it "reports frame-level violations together with frameset violations" do
          connection_method = method_frame(Connection.method_id, Connection::Open.method_id, 3)

          report = described_class.check_frameset([connection_method])

          expect(rules(report)).to include(SemanticChecker::RULE_CONNECTION_CHANNEL_ZERO)
        end

        it "survives nil frames inside the frameset" do
          report = described_class.check_frameset([nil, BodyFrame.new("data", 1)])

          expect(rules(report)).to include(SemanticChecker::RULE_FRAME_NIL)
        end

        it "survives frames with nil payloads inside the frameset" do
          report = described_class.check_frameset([MethodFrame.new(nil, 1),
                                                   HeaderFrame.new(nil, 1),
                                                   BodyFrame.new(nil, 1)])

          expect(rules(report)).to include(SemanticChecker::RULE_PAYLOAD_NIL)
        end

        it "never invokes frame decode or processing callbacks" do
          frames = Basic::Publish.encode(1, "hello", {:content_type => "text/plain"},
                                         "amq.fanout", "rk", false, false, 131072)
          frames.each do |frame|
            expect(frame).not_to receive(:decode_payload)
            expect(frame).not_to receive(:method_class)
          end

          expect { described_class.check_frameset(frames) }.not_to raise_error
        end
      end

      describe SemanticChecker::Violation do
        it "exposes the rule, original field position and details" do
          frame = method_frame(Connection.method_id, Connection::Open.method_id, 2)

          violation = SemanticChecker.check_frame(frame).violations.first

          expect(violation.rule).to eq(SemanticChecker::RULE_CONNECTION_CHANNEL_ZERO)
          expect(violation.field).to eq("channel")
          expect(violation.location).to eq("frame.channel")
          expect(violation.message).to include("connection.open")
          expect(violation.details[:channel]).to eq(2)
          expect(violation.to_s).to include("frame.channel")
        end
      end
    end
  end
end
