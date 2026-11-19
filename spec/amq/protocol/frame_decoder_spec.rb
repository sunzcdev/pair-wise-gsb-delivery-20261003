# encoding: binary

module AMQ
  module Protocol
    RSpec.describe FrameDecoder do
      let(:encoded_body_frame)   { Frame.encode(:body, "test payload", 12) }
      let(:encoded_method_frame) { Frame.encode(:method, "\x00\x3C\x00\x50\x00\x00\x00\x00\x00", 1) }

      it "decodes a complete frame in a single chunk" do
        frames = subject << encoded_body_frame
        expect(frames.size).to eq(1)
        expect(frames.first).to be_a(BodyFrame)
        expect(frames.first.channel).to eq(12)
        expect(frames.first.payload).to eq("test payload")
      end

      it "returns an empty array when the chunk holds no complete frame" do
        expect(subject << encoded_body_frame.byteslice(0, 3)).to eq([])
        expect(subject.buffered_bytes).to eq(3)
      end

      it "decodes multiple frames from a single chunk" do
        frames = subject << (encoded_body_frame + HeartbeatFrame.encode)
        expect(frames.map(&:class)).to eq([BodyFrame, HeartbeatFrame])
      end

      it "decodes a frame fed byte by byte" do
        frames = []
        encoded_body_frame.each_byte do |byte|
          frames.concat(subject << byte.chr)
        end
        expect(frames.size).to eq(1)
        expect(frames.first.payload).to eq("test payload")
        expect(frames.first.channel).to eq(12)
      end

      it "decodes a frame split at every possible boundary" do
        (1...encoded_body_frame.bytesize).each do |split|
          decoder = FrameDecoder.new
          frames = decoder << encoded_body_frame.byteslice(0, split)
          expect(frames).to eq([])
          frames = decoder << encoded_body_frame.byteslice(split..-1)
          expect(frames.size).to eq(1)
          expect(frames.first.payload).to eq("test payload")
          expect(decoder.finish).to eq(:complete)
        end
      end

      it "waits for the frame-end octet when it has not arrived yet" do
        head = encoded_body_frame.byteslice(0, encoded_body_frame.bytesize - 1)
        expect(subject << head).to eq([])
        frames = subject << "\xCE"
        expect(frames.size).to eq(1)
      end

      it "decodes heartbeat frames" do
        frames = subject << HeartbeatFrame.encode
        expect(frames.first).to be_a(HeartbeatFrame)
        expect(frames.first.channel).to eq(0)
        expect(frames.first.payload).to eq("")
      end

      it "preserves the raw payload of method frames" do
        frames = subject << encoded_method_frame
        frame = frames.first
        expect(frame).to be_a(MethodFrame)
        expect(frame.method_class).to eq(AMQ::Protocol::Basic::Ack)
        expect(frame.payload).to eq("\x00\x3C\x00\x50\x00\x00\x00\x00\x00")
      end

      it "accepts non-binary chunks by treating them as bytes" do
        frames = subject << Frame.encode(:body, "à bientôt!", 1).dup.force_encoding("UTF-8")
        expect(frames.first.payload).to eq("à bientôt!".b)
      end

      it "round-trips encoded frames through the decoder" do
        original = Frame.encode(:body, "round trip", 7)
        frames = subject << original
        expect(frames.first.encode).to eq(original)
      end

      it "rejects a declared payload size above max_frame_size as soon as the header is complete" do
        decoder = FrameDecoder.new(max_frame_size: 8)
        header = [Frame::TYPES[:body], 1, 9].pack("cnN")
        expect { decoder << header }.to raise_error(OversizedFrameError)
        expect(decoder.failed?).to be(true)
      end

      it "rejects oversized declarations split across chunks" do
        decoder = FrameDecoder.new(max_frame_size: 8)
        header = [Frame::TYPES[:body], 1, 100].pack("cnN")
        expect(decoder << header.byteslice(0, 4)).to eq([])
        expect { decoder << header.byteslice(4..-1) }.to raise_error(OversizedFrameError)
      end

      it "raises FrameTypeError for an unknown frame type" do
        expect { subject << [42, 1, 0].pack("cnN") }.to raise_error(FrameTypeError)
        expect(subject.failed?).to be(true)
      end

      it "raises InvalidFrameEndError when the frame does not end with 0xCE" do
        malformed = Frame.encode(:body, "abc", 1)
        malformed[-1] = "\x00"
        expect { subject << malformed }.to raise_error(InvalidFrameEndError)
      end

      it "does not resynchronize by scanning for the next 0xCE after an error" do
        malformed = Frame.encode(:body, "abc", 1)
        malformed[-1] = "\x00"
        expect { subject << (malformed + encoded_body_frame) }.to raise_error(InvalidFrameEndError)
        expect { subject << encoded_body_frame }.to raise_error(FrameDecoderError)
      end

      it "rejects further input after an error until reset" do
        malformed = Frame.encode(:body, "abc", 1)
        malformed[-1] = "\x00"
        expect { subject << malformed }.to raise_error(InvalidFrameEndError)
        expect { subject << encoded_body_frame }.to raise_error(FrameDecoderError, /failed state/)

        subject.reset
        expect(subject.failed?).to be(false)
        frames = subject << encoded_body_frame
        expect(frames.first.payload).to eq("test payload")
      end

      it "raises InputBufferOverflowError when buffered input exceeds the limit" do
        decoder = FrameDecoder.new(max_frame_size: 1024, max_buffer_size: 16)
        expect { decoder << Frame.encode(:body, "x" * 32, 1) }.to raise_error(InputBufferOverflowError)
        expect(decoder.failed?).to be(true)
      end

      it "raises TypeError for non-string chunks" do
        expect { subject << nil }.to raise_error(TypeError)
      end

      describe "#finish" do
        it "returns :complete when no partial frame remains" do
          subject << encoded_body_frame
          expect(subject.finish).to eq(:complete)
        end

        it "returns :complete when nothing was fed at all" do
          expect(subject.finish).to eq(:complete)
        end

        it "raises TruncatedInputError when a partial frame remains" do
          subject << encoded_body_frame.byteslice(0, 5)
          expect { subject.finish }.to raise_error(TruncatedInputError)
          expect(subject.failed?).to be(true)
        end

        it "raises TruncatedInputError when only the frame-end octet is missing" do
          subject << encoded_body_frame.byteslice(0, encoded_body_frame.bytesize - 1)
          expect { subject.finish }.to raise_error(TruncatedInputError)
        end
      end

      it "decodes a full basic.publish frameset" do
        frames = Basic::Publish.encode(1, "hello", { content_type: "text/plain" }, "", "test", false, false, 131072)
        stream = frames.map(&:encode).join
        decoded = []
        stream.each_byte.each_slice(5) do |slice|
          decoded.concat(subject << slice.pack("C*"))
        end
        expect(decoded.map(&:class)).to eq([MethodFrame, HeaderFrame, BodyFrame])
        expect(decoded.last.payload).to eq("hello")
        expect(subject.finish).to eq(:complete)
      end
    end
  end
end
