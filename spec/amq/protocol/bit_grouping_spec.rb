# encoding: binary

module AMQ
  module Protocol
    class Basic
      RSpec.describe Nack do
        describe "bit grouping" do
          it "packs adjacent multiple/requeue bits into a single byte" do
            frame = Nack.encode(1, 99, true, true)
            expect(frame.payload).to eq("\x00<\x00x\x00\x00\x00\x00\x00\x00\x00c\x03")
          end

          it "encodes each bit at its own offset within the shared byte" do
            expect(Nack.encode(1, 99, true, false).payload.bytes.last).to eq(0b00000001)
            expect(Nack.encode(1, 99, false, true).payload.bytes.last).to eq(0b00000010)
            expect(Nack.encode(1, 99, false, false).payload.bytes.last).to eq(0b00000000)
          end

          it "decodes grouped bits back into positional booleans" do
            decoded = Nack.decode("\x00\x00\x00\x00\x00\x00\x00\x63\x02")
            expect(decoded.multiple).to eq(false)
            expect(decoded.requeue).to eq(true)
          end
        end
      end

      RSpec.describe Deliver do
        describe "a bit group flanked by string fields" do
          let(:raw) do
            "ctag".length.chr + "ctag" +
              AMQ::Pack.pack_uint64_big_endian(7) +
              "\x01" +
              "ex".length.chr + "ex" +
              "rk".length.chr + "rk"
          end

          it "reads the bit byte between the strings and keeps argument order" do
            decoded = Deliver.decode(raw)
            expect(decoded.consumer_tag).to eq("ctag")
            expect(decoded.delivery_tag).to eq(7)
            expect(decoded.redelivered).to eq(true)
            expect(decoded.exchange).to eq("ex")
            expect(decoded.routing_key).to eq("rk")
          end
        end
      end

      RSpec.describe GetOk do
        describe "a bit between a longlong and strings with a trailing long" do
          let(:raw) do
            AMQ::Pack.pack_uint64_big_endian(6) +
              "\x01" +
              "ex".length.chr + "ex" +
              "rk".length.chr + "rk" +
              [94].pack(PACK_UINT32)
          end

          it "decodes every field in protocol order without reusing the bit offset" do
            decoded = GetOk.decode(raw)
            expect(decoded.delivery_tag).to eq(6)
            expect(decoded.redelivered).to eq(true)
            expect(decoded.exchange).to eq("ex")
            expect(decoded.routing_key).to eq("rk")
            expect(decoded.message_count).to eq(94)
          end
        end
      end
    end

    class Channel
      RSpec.describe Flow do
        describe "single-bit method" do
          it "round-trips the bit in its own byte" do
            expect(Flow.encode(1, true).payload).to eq("\x00\x14\x00\x14\x01")
            expect(Flow.decode("\x00").active).to eq(false)
            expect(Flow.decode("\x01").active).to eq(true)
          end
        end
      end
    end

    RSpec.describe "strict bit encoding" do
      after { Method.disable_strict_parameters! }
      context "with strict parameters enabled" do
        before { Method.enable_strict_parameters! }

        it "treats an omitted nil bit and an explicit false bit identically on the wire" do
          omitted = Basic::Ack.encode(1, 1, nil)
          explicit = Basic::Ack.encode(1, 1, false)
          truthy = Basic::Ack.encode(1, 1, true)

          expect(omitted.payload).to eq(explicit.payload)
          expect(omitted.payload.bytes.last).to eq(0)
          expect(truthy.payload.bytes.last).to eq(1)
        end

        it "raises for a truthy object while accepting false and nil" do
          expect { Basic::Ack.encode(1, 1, false) }.not_to raise_error
          expect { Basic::Ack.encode(1, 1, nil) }.not_to raise_error
          expect { Basic::Ack.encode(1, 1, "true") }.to raise_error(InvalidParameterError)
          expect { Basic::Ack.encode(1, 1, 1) }.to raise_error(InvalidParameterError)
        end

        it "decoded booleans are real true/false objects, never truthy integers" do
          expect(Basic::Nack.decode("\x00" * 8 + "\x00").multiple).to equal(false)
          expect(Basic::Nack.decode("\x00" * 8 + "\x01").multiple).to equal(true)
        end
      end
    end
  end
end
