# encoding: binary

module AMQ
  module Protocol
    RSpec.describe ContentAssembler do
      let(:assembler) { ContentAssembler.new }

      def publish_frames(channel, payload, properties = { content_type: "text/plain" }, frame_size = 131072)
        Basic::Publish.encode(channel, payload, properties, "amq.fanout", "notifications", false, false, frame_size)
      end

      def header_frame_with_class(klass_id, body_size, channel)
        payload  = [klass_id, 0].pack(PACK_UINT16_X2)
        payload += AMQ::Pack.pack_uint64_big_endian(body_size)
        payload += [0].pack(PACK_UINT16)
        HeaderFrame.new(payload, channel)
      end

      describe "#feed" do
        it "returns nil until all declared body bytes have arrived" do
          method_frame, header_frame, *body_frames = publish_frames(1, "hello world", { content_type: "text/plain" }, 12)
          expect(body_frames.size).to be > 1

          expect(assembler.feed(method_frame)).to be_nil
          expect(assembler.feed(header_frame)).to be_nil
          body_frames[0..-2].each do |frame|
            expect(assembler.feed(frame)).to be_nil
          end

          message = assembler.feed(body_frames[-1])
          expect(message).to be_a(ContentAssembler::Message)
          expect(message.method_frame).to eq(method_frame)
          expect(message.header_frame).to eq(header_frame)
          expect(message.body).to eq("hello world")
          expect(message.channel).to eq(1)
          expect(message.properties[:content_type]).to eq("text/plain")
        end

        it "completes right after the header when the declared body size is zero" do
          method_frame, header_frame = publish_frames(1, "")
          expect(header_frame.body_size).to eq(0)

          expect(assembler.feed(method_frame)).to be_nil
          message = assembler.feed(header_frame)
          expect(message).to be_a(ContentAssembler::Message)
          expect(message.body).to eq("")
        end

        it "assembles interleaved channels independently" do
          method1, header1, body1 = publish_frames(1, "first")
          method2, header2, body2 = publish_frames(2, "second")

          expect(assembler.feed(method1)).to be_nil
          expect(assembler.feed(method2)).to be_nil
          expect(assembler.feed(header1)).to be_nil
          expect(assembler.feed(header2)).to be_nil

          message2 = assembler.feed(body2)
          expect(message2.body).to eq("second")
          expect(message2.channel).to eq(2)

          message1 = assembler.feed(body1)
          expect(message1.body).to eq("first")
          expect(message1.channel).to eq(1)
        end

        it "ignores heartbeat frames without advancing any message" do
          method_frame, header_frame, body_frame = publish_frames(1, "hello")

          assembler.feed(method_frame)
          expect(assembler.feed(HeartbeatFrame.new(EMPTY_STRING, 0))).to be_nil
          assembler.feed(header_frame)
          expect(assembler.feed(HeartbeatFrame.new(EMPTY_STRING, 0))).to be_nil

          message = assembler.feed(body_frame)
          expect(message.body).to eq("hello")
        end

        it "rejects a duplicate content header" do
          method_frame, header_frame, = publish_frames(1, "hello")

          assembler.feed(method_frame)
          assembler.feed(header_frame)
          expect { assembler.feed(header_frame) }.to raise_error(DuplicateHeaderError)
        end

        it "rejects body bytes beyond the declared size" do
          method_frame, header_frame, = publish_frames(1, "hello")

          assembler.feed(method_frame)
          assembler.feed(header_frame)
          expect { assembler.feed(BodyFrame.new("hello!", 1)) }.to raise_error(BodySizeExceededError)
        end

        it "rejects a header whose class does not match the method" do
          method_frame, = publish_frames(1, "hello")

          assembler.feed(method_frame)
          expect { assembler.feed(header_frame_with_class(50, 0, 1)) }.to raise_error(ContentClassMismatchError)
        end

        it "rejects a body frame without a preceding method" do
          expect { assembler.feed(BodyFrame.new("hello", 1)) }.to raise_error(UnexpectedFrameError)
        end

        it "rejects a header frame without a preceding method" do
          _, header_frame, = publish_frames(1, "hello")
          expect { assembler.feed(header_frame) }.to raise_error(UnexpectedFrameError)
        end

        it "rejects a method frame while another message on the channel is incomplete" do
          method_frame, header_frame, = publish_frames(1, "hello")
          other_method, = publish_frames(1, "other")

          assembler.feed(method_frame)
          assembler.feed(header_frame)
          expect { assembler.feed(other_method) }.to raise_error(UnexpectedFrameError)
        end

        it "rejects methods that do not carry content" do
          expect { assembler.feed(Basic::Ack.encode(1, 1, false)) }.to raise_error(UnexpectedFrameError)
        end
      end

      describe "#close_channel" do
        it "discards the incomplete state of the channel" do
          method_frame, header_frame, = publish_frames(1, "hello")

          assembler.feed(method_frame)
          assembler.feed(header_frame)
          expect(assembler.pending?(1)).to be true

          assembler.close_channel(1)
          expect(assembler.pending?(1)).to be false
        end

        it "does not affect other channels" do
          method1, header1, = publish_frames(1, "first")
          method2, header2, body2 = publish_frames(2, "second")

          assembler.feed(method1)
          assembler.feed(header1)
          assembler.feed(method2)
          assembler.feed(header2)

          assembler.close_channel(1)

          message = assembler.feed(body2)
          expect(message.body).to eq("second")
        end

        it "never appends frames of the next message to a truncated body" do
          method_frame, header_frame, = publish_frames(1, "hello")

          assembler.feed(method_frame)
          assembler.feed(header_frame)
          assembler.feed(BodyFrame.new("he", 1))
          assembler.close_channel(1)

          method2, header2, body2 = publish_frames(1, "hi")
          assembler.feed(method2)
          assembler.feed(header2)
          message = assembler.feed(body2)
          expect(message.body).to eq("hi")
        end
      end
    end
  end
end
