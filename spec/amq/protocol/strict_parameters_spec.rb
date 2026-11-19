# encoding: binary

module AMQ
  module Protocol
    RSpec.describe StrictParameters do
      after(:each) do
        described_class.disable!
      end

      describe "global switch" do
        it "is disabled by default" do
          expect(described_class.enabled?).to eq(false)
          expect(Method.strict?).to eq(false)
        end

        it "can be enabled and disabled through Method" do
          Method.enable_strict_parameters!
          expect(Method.strict?).to eq(true)
          expect(described_class.enabled?).to eq(true)

          Method.disable_strict_parameters!
          expect(Method.strict?).to eq(false)
        end
      end

      describe ".validate!" do
        before { described_class.enable! }

        it "skips all checks when strict mode is off" do
          described_class.disable!
          expect {
            described_class.validate!("m", [[:n, :short, false]], {n: "not an int"})
          }.not_to raise_error
        end

        it "allows nil for optional arguments" do
          expect {
            described_class.validate!("m", [[:n, :table, true]], {n: nil})
          }.not_to raise_error
        end

        it "rejects nil for required arguments" do
          expect {
            described_class.validate!("m", [[:n, :shortstr, false]], {n: nil})
          }.to raise_error(InvalidParameterError, /n/)
        end

        it "validates integer domains and their ranges" do
          params = [[:oct, :octet, false], [:s, :short, false], [:l, :long, false], [:ll, :longlong, false]]
          args = {oct: 255, s: 65_535, l: 4_294_967_295, ll: 18_446_744_073_709_551_615}
          expect { described_class.validate!("m", params, args) }.not_to raise_error

          expect { described_class.validate!("m", [[:oct, :octet, false]], {oct: 256}) }.to raise_error(InvalidParameterError)
          expect { described_class.validate!("m", [[:s, :short, false]], {s: -1}) }.to raise_error(InvalidParameterError)
          expect { described_class.validate!("m", [[:l, :long, false]], {l: "4"}) }.to raise_error(InvalidParameterError)
        end

        it "validates string domains" do
          expect { described_class.validate!("m", [[:s, :shortstr, false]], {s: "x"}) }.not_to raise_error
          expect { described_class.validate!("m", [[:s, :shortstr, false]], {s: "a" * 256}) }.to raise_error(InvalidParameterError)
          expect { described_class.validate!("m", [[:s, :longstr, false]], {s: 42}) }.to raise_error(InvalidParameterError)
        end

        it "validates table arguments" do
          expect { described_class.validate!("m", [[:t, :table, true]], {t: {}}) }.not_to raise_error
          expect { described_class.validate!("m", [[:t, :table, true]], {t: []}) }.to raise_error(InvalidParameterError)
        end

        it "validates bit arguments as real booleans while allowing nil" do
          expect { described_class.validate!("m", [[:f, :bit, true]], {f: true}) }.not_to raise_error
          expect { described_class.validate!("m", [[:f, :bit, true]], {f: false}) }.not_to raise_error
          expect { described_class.validate!("m", [[:f, :bit, true]], {f: nil}) }.not_to raise_error
          expect { described_class.validate!("m", [[:f, :bit, true]], {f: 1}) }.to raise_error(InvalidParameterError, /true, false or nil/)
          expect { described_class.validate!("m", [[:f, :bit, true]], {f: "yes"}) }.to raise_error(InvalidParameterError)
        end

        it "validates timestamp arguments as Time or Integer" do
          expect { described_class.validate!("m", [[:ts, :timestamp, true]], {ts: Time.now}) }.not_to raise_error
          expect { described_class.validate!("m", [[:ts, :timestamp, true]], {ts: 12345}) }.not_to raise_error
          expect { described_class.validate!("m", [[:ts, :timestamp, true]], {ts: "now"}) }.to raise_error(InvalidParameterError)
        end
      end
    end

    RSpec.describe Method do
      describe "generated metadata" do
        it "exposes PARAMETERS describing each method argument" do
          expect(Basic::Ack::PARAMETERS).to eq([
            [:delivery_tag, :longlong, false],
            [:multiple, :bit, true]
          ])
          expect(Basic::Ack::ARGUMENT_NAMES).to eq([:delivery_tag, :multiple])
        end

        it "marks bit arguments optional so omitted bits stay encodable" do
          flags = Queue::Declare::PARAMETERS.select { |_, type, _| type == :bit }.map { |_, _, optional| optional }
          expect(flags).to all(eq(true))
        end

        it "marks scalar arguments with blank defaults as required" do
          entry = Queue::Declare::PARAMETERS.detect { |name, _, _| name == :queue }
          expect(entry).to eq([:queue, :shortstr, false])
        end
      end

      describe "strict encoding" do
        after { Method.disable_strict_parameters! }

        it "keeps legacy truthiness semantics when strict mode is off" do
          expect(Basic::Ack.encode(1, 42, Object.new).payload.bytes.last).to eq(1)
        end

        context "with strict mode enabled" do
          before { Method.enable_strict_parameters! }

          it "rejects a truthy object passed as a bit argument" do
            expect { Basic::Ack.encode(1, 42, Object.new) }.to raise_error(InvalidParameterError, /multiple/)
          end

          it "rejects a truthy integer passed as a bit argument" do
            expect { Basic::Ack.encode(1, 42, 1) }.to raise_error(InvalidParameterError)
          end

          it "encodes true, explicit false and omitted nil bits differently only in validation" do
            expect(Basic::Ack.encode(1, 42, true).payload.bytes.last).to eq(1)
            expect(Basic::Ack.encode(1, 42, false).payload.bytes.last).to eq(0)
            expect(Basic::Ack.encode(1, 42, nil).payload.bytes.last).to eq(0)
          end

          it "rejects non-string values for string arguments" do
            expect {
              Queue::Declare.encode(1, 9, false, false, false, false, false, {})
            }.to raise_error(InvalidParameterError, /queue/)
          end

          it "rejects out-of-range integers" do
            expect { Basic::Ack.encode(1, 18_446_744_073_709_551_616, false) }.to raise_error(InvalidParameterError, /delivery_tag/)
          end

          it "still encodes valid method calls byte-identically to legacy mode" do
            legacy = Basic::Nack.encode(1, 7, false, true).payload
            strict = Basic::Nack.encode(1, 7, false, true).payload
            expect(strict).to eq(legacy)
          end

          it "accepts omitted optional table arguments" do
            expect { Queue::Declare.encode(1, "q", false, false, false, false, false, nil) }.not_to raise_error
          end
        end
      end
    end
  end
end
