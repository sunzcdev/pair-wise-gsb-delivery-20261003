require "spec_helper"
require "stringio"
require "multi_json/json_seq"

RSpec.describe MultiJson::JsonSeq do
  RS = described_class::RS
  LF = described_class::LF

  # IO that only ever hands out a single byte per read, to prove the reader
  # never relies on buffered, multibyte reads.
  class OneByteIO
    def initialize(string)
      @string = string
      @index = 0
    end

    def read(count = 1)
      return nil if @index >= @string.bytesize

      byte = @string.byteslice(@index, count)
      @index += 1
      byte
    end
  end

  # IO that blocks in read until bytes (or a closing nil) are pushed in,
  # modelling a stream that is continuously appended to.
  class AppendableIO
    def initialize
      @queue = Thread::Queue.new
    end

    def read(_count = 1)
      @queue.pop
    end

    def write_chunk(string)
      string.each_byte { |byte| @queue << byte.chr.b }
    end

    def close
      @queue << nil
    end
  end

  let(:adapter) { :json_gem }

  before { MultiJson.use(adapter) }

  def encode_sequence(*values)
    values.map { |value| "#{RS}#{MultiJson.dump(value)}#{LF}" }.join
  end

  describe MultiJson::JsonSeq::Writer do
    let(:io) { StringIO.new.binmode }

    it "frames every value with an RS prefix and an LF suffix" do
      writer = described_class.new(io)
      writer.write("name" => "foo")
      writer << [1, 2, 3]
      expect(io.string).to eq("\x1e{\"name\":\"foo\"}\n\x1e[1,2,3]\n".b)
    end

    it "writes each record straight away without buffering the whole stream" do
      writer = described_class.new(io)
      expect { writer.write("a" => 1) }.to change { io.string.length }
    end

    it "escapes RS and LF so framing bytes never occur inside a record" do
      described_class.new(io).write("a\x1eb\nc")
      expect(io.string).to eq("\x1e\"a\\u001eb\\nc\"\n".b)
    end

    it "round trips top level scalar values" do
      writer = described_class.new(io)
      writer.write(42)
      writer.write(nil)
      writer.write(true)
      reader = MultiJson.json_seq_reader(StringIO.new(io.string))
      expect(reader.to_a).to eq([42, nil, true])
    end

    it "supports flush and close when the io does" do
      described_class.new(io).close
      expect(io).to be_closed
    end
  end

  describe MultiJson::JsonSeq::Reader do
    def reader_for(string, **options)
      described_class.new(OneByteIO.new(string), **options)
    end

    it "decodes a sequence of records read one byte at a time" do
      stream = encode_sequence({"a" => 1}, [1, 2], "x")
      expect(reader_for(stream).to_a).to eq([{"a" => 1}, [1, 2], "x"])
    end

    it "skips consecutive record separators and empty records" do
      stream = "#{RS}#{LF}#{RS}#{RS}#{LF}#{RS}#{RS}#{encode_sequence(1)}"
      expect(reader_for(stream).to_a).to eq([1])
    end

    it "decodes strings containing escaped RS and LF control characters" do
      stream = encode_sequence("a\x1eb\nc", "d\x1ee")
      expect(reader_for(stream).to_a).to eq(["a\x1eb\nc", "d\x1ee"])
    end

    it "returns nil for a decoded JSON null, distinct from end of stream" do
      reader = reader_for(encode_sequence(nil))
      expect(reader.read_record).to be_nil
      expect(reader.read_record).to be_nil
      expect(reader.next_record).to equal(MultiJson::JsonSeq::EOF)
    end

    context "bad records" do
      let(:stream) { "#{RS}{\"ok\":1}#{LF}#{RS}{broken#{LF}#{RS}{\"also\":2}#{LF}" }

      it "raises a MalformedRecordError with the raw byte range in stop mode" do
        reader = reader_for(stream)
        expect(reader.read_record).to eq({"ok" => 1})

        begin
          reader.read_record
        rescue MultiJson::JsonSeq::MalformedRecordError => error
          expect(error.start_byte).to eq(10)
          expect(error.end_byte).to eq(19)
          expect(error.raw).to eq("#{RS}{broken#{LF}".b)
          expect(error.record_error).to be_a(MultiJson::ParseError)
        end

        expect(reader.read_record).to eq({"also" => 2})
        expect(reader.read_record).to be_nil
      end

      it "continues at the next record and yields a BadRecord in continue mode" do
        bad_records = []
        reader = reader_for(stream, on_invalid: :continue,
                                    on_bad_record: ->(record) { bad_records << record })

        first = reader.read_record
        second = reader.read_record
        third = reader.read_record

        expect(first).to eq({"ok" => 1})
        expect(second).to be_a(MultiJson::JsonSeq::BadRecord)
        expect(second.start_byte).to eq(10)
        expect(second.end_byte).to eq(19)
        expect(second.raw).to eq("#{RS}{broken#{LF}".b)
        expect(second.bad?).to be(true)
        expect(third).to eq({"also" => 2})
        expect(bad_records).to eq([second])
      end

      it "resynchronises on the next RS even when a bad record lacks an LF" do
        stream = "#{RS}garbage#{RS}{\"ok\":true}#{LF}"
        reader = reader_for(stream, on_invalid: :continue)
        records = reader.to_a
        expect(records.first).to be_a(MultiJson::JsonSeq::BadRecord)
        expect(records.last).to eq({"ok" => true})
      end

      it "ignores leading bytes that appear before the first RS" do
        stream = "junk before stream#{RS}1#{LF}"
        expect(reader_for(stream).to_a).to eq([1])
      end
    end

    context "truncation" do
      it "does not treat a top level number without an LF as complete" do
        reader = reader_for("#{RS}123", on_invalid: :continue)
        bad = reader.read_record
        expect(bad).to be_a(MultiJson::JsonSeq::BadRecord)
        expect(bad.raw).to eq("#{RS}123".b)
        expect(bad.start_byte).to eq(0)
        expect(bad.end_byte).to eq(4)
      end

      it "raises for a truncated number in stop mode" do
        reader = reader_for("#{RS}123")
        expect { reader.read_record }.to raise_error(MultiJson::JsonSeq::MalformedRecordError)
      end

      it "accepts a complete non numeric value terminated by EOF" do
        expect(reader_for("#{RS}{\"a\":1}").to_a).to eq([{"a" => 1}])
        expect(reader_for("#{RS}\"str\"").to_a).to eq(["str"])
        expect(reader_for("#{RS}[1,2]").to_a).to eq([[1, 2]])
      end

      it "accepts the number once the terminating LF finally arrives" do
        io = AppendableIO.new
        reader = described_class.new(io, on_invalid: :continue)
        result = nil
        thread = Thread.new { result = reader.read_record }
        io.write_chunk("#{RS}123")
        sleep 0.05
        expect(thread).to be_alive
        io.write_chunk(LF)
        io.close
        thread.join
        expect(result).to eq(123)
      end

      it "keeps reading records that arrive after the initial EOF of an appended stream" do
        io = AppendableIO.new
        reader = described_class.new(io, on_invalid: :continue)
        results = []
        thread = Thread.new { 3.times { results << reader.read_record } }
        io.write_chunk("#{RS}1#{LF}")
        io.write_chunk("#{RS}nope#{LF}")
        io.write_chunk("#{RS}2#{LF}")
        io.close
        thread.join

        expect(results.first).to eq(1)
        expect(results[1]).to be_a(MultiJson::JsonSeq::BadRecord)
        expect(results.last).to eq(2)
      end
    end

    context "max_record_size" do
      it "raises RecordTooLargeError for an oversized record and keeps going" do
        stream = "#{RS}12345#{LF}#{RS}6#{LF}"
        reader = reader_for(stream, max_record_size: 3)

        begin
          reader.read_record
        rescue MultiJson::JsonSeq::RecordTooLargeError => error
          expect(error.start_byte).to eq(0)
          expect(error.end_byte).to eq(7)
          expect(error.raw).to eq("#{RS}123#{LF}".b)
        end

        expect(reader.read_record).to eq(6)
      end

      it "accounts for the framing bytes in the absolute offsets" do
        stream = "#{RS}1#{LF}#{RS}abcdef#{LF}"
        reader = reader_for(stream, max_record_size: 2)
        reader.read_record
        begin
          reader.read_record
        rescue MultiJson::JsonSeq::RecordTooLargeError => error
          expect(error.start_byte).to eq(3)
          expect(error.end_byte).to eq(11)
        end
      end

      it "does not accumulate the oversized bytes in memory" do
        huge = "#{RS}#{"a" * 50_000}#{LF}#{RS}1#{LF}"
        reader = reader_for(huge, max_record_size: 8)
        expect { reader.read_record }.to raise_error(MultiJson::JsonSeq::RecordTooLargeError)
        expect(reader.read_record).to eq(1)
      end

      it "rejects a non positive size" do
        expect { reader_for("", max_record_size: 0) }.to raise_error(ArgumentError)
      end
    end

    it "forwards load options to the adapter" do
      stream = encode_sequence({"a" => 1})
      reader = reader_for(stream, symbolize_keys: true)
      expect(reader.to_a).to eq([a: 1])
    end

    it "supports a per reader adapter override" do
      stream = encode_sequence("a" => 1)
      reader = described_class.new(OneByteIO.new(stream), adapter: :json_gem)
      expect(reader.to_a).to eq([{"a" => 1}])
    end

    it "is enumerable" do
      reader = reader_for(encode_sequence(1, 2, 3))
      expect(reader.map { |value| value * 2 }).to eq([2, 4, 6])
      expect(reader.each).to be_an(Enumerator)
    end

    it "rejects an unknown on_invalid value" do
      expect { reader_for("", on_invalid: :bogus) }.to raise_error(ArgumentError)
    end

    it "returns an empty sequence for an empty stream" do
      expect(reader_for("").to_a).to eq([])
    end

    it "tracks the number of consumed bytes" do
      stream = "#{RS}1#{LF}#{RS}2#{LF}"
      reader = reader_for(stream)
      reader.read_record
      expect(reader.offset).to eq(3)
      reader.read_record
      expect(reader.offset).to eq(6)
    end
  end
end
