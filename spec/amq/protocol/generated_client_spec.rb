# encoding: binary

module AMQ
  module Protocol
    RSpec.describe "generated client.rb" do
      let(:generated_source) do
        File.read(File.expand_path("../../../../lib/amq/protocol/client.rb", __FILE__))
      end

      it "is regenerated from the codegen templates and pulls in strict parameter support" do
        expect(generated_source).to include('require "amq/protocol/strict_parameters"')
      end

      it "emits strict-aware inline checks instead of truthiness-only bit lines" do
        expect(generated_source).not_to include("if passive")
        expect(generated_source).not_to include("if nowait")
        expect(generated_source.scan(/StrictParameters\.enabled\?/).size).to be > 20
      end

      it "validates every generated method encode body against its PARAMETERS metadata" do
        method_classes = generated_source.scan(/class \w+ < Protocol::Method/).size
        expect(method_classes).to be > 30
        # 39 client-encodable AMQP methods; encode_body/encode_properties are
        # shared helpers and do not carry PARAMETERS
        expect(generated_source.scan(/StrictParameters\.validate!\(@name, PARAMETERS/).size).to eq(39)
      end

      it "declares PARAMETERS metadata for every generated method class" do
        expect(generated_source.scan(/PARAMETERS = \[/).size).to eq(
          generated_source.scan(/class \w+ < Protocol::Method/).size
        )
      end

      it "uses getbyte/byteslice/unpack1 primitives in generated decoders" do
        expect(generated_source).to include("data.getbyte(offset)")
        expect(generated_source).to include("data.byteslice(offset, length)")
        expect(generated_source).not_to include("bitindex")
      end
    end

    RSpec.describe "generated bit-group methods" do
      it "encodes the five exchange.declare flags into one shared byte" do
        frame = Exchange::Declare.encode(1, "ex", "direct", true, true, false, true, false, nil)
        # payload ends with the flags byte followed by a 4-byte empty table
        flags_byte, table_length = frame.payload.bytes[-5], frame.payload.bytes[-4..-1]
        expect(table_length).to eq([0, 0, 0, 0])
        expect(flags_byte).to eq(0b00001011)
      end

      it "encodes the five queue.declare flags into one shared byte" do
        frame = Queue::Declare.encode(1, "q", false, true, true, false, true, nil)
        flags_byte, table_length = frame.payload.bytes[-5], frame.payload.bytes[-4..-1]
        expect(table_length).to eq([0, 0, 0, 0])
        expect(flags_byte).to eq(0b00010110)
      end

      it "keeps basic.publish mandatory/immediate adjacent in one byte" do
        frames = Basic::Publish.encode(1, "b", {content_type: "text/plain"}, "ex", "rk", true, true, 131_072)
        expect(frames.first.payload.bytes.last).to eq(0b00000011)
        frames2 = Basic::Publish.encode(1, "b", {content_type: "text/plain"}, "ex", "rk", true, false, 131_072)
        expect(frames2.first.payload.bytes.last).to eq(0b00000001)
      end
    end
  end
end
