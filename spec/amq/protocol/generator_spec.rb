# encoding: binary

module AMQ
  module Protocol
    RSpec.describe Generator do
      let(:description) { Spec::Builtin.demo(version: "1.2.0", tag: "first") }

      before { Registry.clear! }

      describe ".generate" do
        it "emits identical source for repeated generation of one description" do
          first = described_class.generate(description, "AMQ::Protocol::Generated::DemoA")
          second = described_class.generate(description, "AMQ::Protocol::Generated::DemoA")

          expect(second).to eq(first)
        end

        it "produces Ruby that defines a namespace bound to the description" do
          source = described_class.generate(description, "AMQ::Protocol::Generated::DemoB")
          eval(source, TOPLEVEL_BINDING) # rubocop:disable Security/Eval

          generated = AMQ::Protocol::Generated::DemoB
          expect(generated::VERSION.version).to eq("1.2.0")
          expect(generated::NAMESPACE::Demo::Ping.argument_names).to eq(%i[label loud repeat])
          expect(generated::DESCRIPTION).to eq(description)
          expect(generated::PORT).to eq(description.port)
        end

        it "survives a generate/load/generate round trip unchanged" do
          once = described_class.generate(description, "AMQ::Protocol::Generated::DemoC")
          eval(once, TOPLEVEL_BINDING) # rubocop:disable Security/Eval
          reloaded = AMQ::Protocol::Generated::DemoC::DESCRIPTION

          twice = described_class.generate(reloaded, "AMQ::Protocol::Generated::DemoC")
          expect(twice).to eq(once)
        end
      end

      describe ".manifest" do
        it "is deterministic and preserves declaration order" do
          first = described_class.manifest(description)
          second = described_class.manifest(Spec::Loader.from_document(description.to_h))

          expect(second).to eq(first)
          expect(first).to include("method ping 10 content=false label:shortstr loud:bit repeat:bit")
        end

        it "changes when a field is added" do
          document = Marshal.load(Marshal.dump(description.to_h))
          document["classes"][0]["methods"][0]["arguments"] << { "type" => "bit", "name" => "extra" }

          expect(described_class.manifest(Spec::Loader.from_document(document)))
            .not_to eq(described_class.manifest(description))
        end
      end
    end

    RSpec.describe Codec do
      before { Registry.clear! }

      describe "generated encoder parity with the legacy namespace" do
        let(:version) { Registry.register(Spec::Builtin.amqp_0_9_1) }

        # Representative methods covering empty, bit-run, table, longlong and
        # timestamp arguments. The legacy file is generated from the same
        # description, so a regression in field order or bit grouping shows up
        # here as a byte diff. Arguments are the full declared list, including
        # the ones the old generator hard-coded.
        examples = {
          "connection.start-ok" => [
            { "platform" => "Ruby" }, "PLAIN", "\x00guest\x00guest", "en_GB"
          ],
          "connection.close" => [320, "bye", 10, 10],
          "connection.close-ok" => [],
          "channel.flow" => [true],
          "exchange.declare" => [0, "ex", "topic", false, true, false, false, false, {}],
          "exchange.delete" => [0, "ex", true, false],
          "queue.declare" => [0, "q", false, true, false, false, false, {}],
          "queue.purge" => [0, "q", false],
          "basic.consume" => [0, "q", "tag", false, true, false, true, {}],
          "basic.qos" => [1024, 10, true],
          "basic.cancel" => ["tag", true],
          "basic.recover" => [true],
          "confirm.select" => [false]
        }.freeze

        # The legacy generator predates these arguments and hard-codes them,
        # so they are supplied here but omitted from the legacy call.
        LEGACY_HARD_CODED = %w[ticket capabilities insist out-of-band known-hosts].freeze

        examples.each do |qualified_name, arguments|
          it "encodes #{qualified_name} the same way as the legacy namespace" do
            class_name, method_name = qualified_name.split(".")
            spec = version.description.class_named(class_name).method_named(method_name)
            legacy = AMQ::Protocol.const_get(class_name.split(/[- ]/).map { |p| p[0].upcase + p[1..].to_s }.join)
                                 .const_get(method_name.split(/[- ]/).map { |p| p[0].upcase + p[1..].to_s }.join)

            legacy_arguments = spec.arguments.zip(arguments)
                                     .reject { |field, _| LEGACY_HARD_CODED.include?(field.name) }
                                     .map { |_, value| value }

            modern = version.method_class(spec.class_id, spec.id).encode_arguments(*arguments)
            legacy_payload = if class_name == "connection"
                               legacy.encode(*legacy_arguments).payload
                             else
                               legacy.encode(1, *legacy_arguments).payload
                             end

            expect(modern).to eq(legacy_payload.byteslice(4..-1))
          end
        end
      end

      describe Versioned::ProtocolClass do
        let(:version) { Registry.register(Spec::Builtin.amqp_0_9_1) }
        let(:basic) { version.class_constant("basic") }

        describe ".encode_properties" do
          it "matches the legacy header payload for a sparse property set" do
            modern = basic.encode_properties(10, priority: 0, delivery_mode: 2, content_type: "application/octet-stream")
            legacy = AMQ::Protocol::Basic.encode_properties(10, priority: 0, delivery_mode: 2, content_type: "application/octet-stream")

            expect(modern).to eq(legacy)
          end

          it "keeps declaration order when every property is set" do
            properties = { content_type: "x", headers: { "hello" => "world" }, timestamp: Time.at(123) }

            payload = basic.encode_properties(0, properties)
            # 0xA040 = content-type (1 << 15) | headers (1 << 13) | timestamp (1 << 6)
            # plus the continuation bit, which is clear in a single flag word.
            expect(payload.byteslice(0, 2).unpack1("n")).to eq(60)
            expect(payload.byteslice(12, 2).unpack1("n")).to eq(0xA040)
            # content-type comes before the headers table, as declared
            expect(payload.byteslice(14, 3)).to eq("\x01x\x00".b)
          end

          it "rejects properties the version does not declare" do
            expect { basic.encode_properties(0, bogus: 1) }.to raise_error(ArgumentError, /unknown basic properties/)
          end

          it "refuses to encode properties for a class without content" do
            expect { version.class_constant("connection").encode_properties(0, {}) }
              .to raise_error(ArgumentError, /does not carry content/)
          end
        end

        describe ".decode_properties" do
          it "matches the legacy decoder" do
            payload = "\xB8@\x18application/octet-stream\x00\x00\x00\x10\x05helloS\x00\x00\x00\x05world\x02\x00\x00\x00\x00\x00\x00\x00\x00{".b

            expect(basic.decode_properties(payload)).to eq(AMQ::Protocol::Basic.decode_properties(payload))
          end

          it "round-trips a property set that spans two flag words" do
            properties = basic.properties.each_with_index.each_with_object({}) do |(name, index), acc|
              next unless index.even?
              acc[name] = case name
                          when :headers then { "hello" => "world" }
                          when :delivery_mode, :priority then index
                          else "v#{index}"
                          end
            end
            payload = basic.encode_properties(0, properties)

            decoded = basic.decode_properties(payload.byteslice(12..-1))
            expect(decoded).to eq(properties)
            expect(decoded.keys).to eq(properties.keys)
          end
        end
      end

      describe Versioned::Arguments do
        let(:document) do
          {
            "name" => "Bits",
            "major-version" => 1,
            "minor-version" => 0,
            "revision" => 0,
            "port" => 1,
            "classes" => [
              {
                "name" => "bits",
                "id" => 1,
                "methods" => [
                  {
                    "name" => "pack",
                    "id" => 10,
                    "arguments" => [
                      { "type" => "octet", "name" => "a" },
                      { "type" => "bit", "name" => "b" },
                      { "type" => "bit", "name" => "c" },
                      { "type" => "short", "name" => "d" },
                      { "type" => "bit", "name" => "e" }
                    ]
                  }
                ]
              }
            ]
          }
        end

        let(:fields) do
          Spec::Loader.from_document(document).method_with_id(1, 10).arguments
        end

        it "packs consecutive bit fields into one octet and restarts after a gap" do
          wire = described_class.encode(fields, [7, true, false, 300, true])

          expect(wire.bytes).to eq([7, 0b001, 0x01, 0x2C, 0b1])
        end

        it "round-trips values while keeping field order" do
          values = [7, true, false, 300, true]
          wire = described_class.encode(fields, values)
          decoded, offset = described_class.decode(fields, wire)

          expect(decoded).to eq(values)
          expect(offset).to eq(wire.bytesize)
        end

        it "refuses to encode a partial argument list without declared defaults" do
          expect { described_class.encode(fields, [7]) }.to raise_error(ArgumentError, /missing required arguments/)
        end
      end
    end
  end
end
