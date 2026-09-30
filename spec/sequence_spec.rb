require "spec_helper"

RSpec.describe "RFC 7464 JSON Text Sequences" do
  before(:all) do
    require "multi_json/adapters/json_gem"
    require "multi_json/adapters/json_pure"
    require "multi_json/adapters/ok_json"
  end

  let(:adapters) do
    [MultiJson::Adapters::JsonGem, MultiJson::Adapters::JsonPure, MultiJson::Adapters::OkJson]
  end

  def with_each_adapter(&example)
    adapters.each do |adapter|
      MultiJson.with_adapter(adapter) { example.call(adapter) }
    end
  end

  # Thread-safe, blocking byte IO backed by a queue. Stands in for a pipe so
  # that framing can be exercised one byte at a time as chunks arrive.
  class QueuedIO
    def initialize
      @queue = Queue.new
    end

    def write(chunk)
      chunk.each_byte { |byte| @queue << byte }
      chunk.bytesize
    end

    def getbyte
      @queue.pop
    end

    def close
      @queue << nil
    end

    def flush
    end
  end

  def seq_bytes(*records)
    records.map { |record| "\x1e#{record}\n" }.join.b
  end

  def frame(*objects)
    MultiJson.dump_sequence(objects)
  end

  describe MultiJson::SequenceReader do
    it "parses records produced by the writer" do
      with_each_adapter do
        data = frame(1, "two", [3], {"four" => 4}, nil, true)
        expect(MultiJson.load_sequence(data).to_a).to eq([1, "two", [3], {"four" => 4}, nil, true])
      end
    end

    it "yields records to a block" do
      MultiJson.with_adapter(MultiJson::Adapters::JsonGem) do
        seen = []
        MultiJson.load_sequence(frame("a", "b")) { |value| seen << value }
        expect(seen).to eq(["a", "b"])
      end
    end

    it "ignores leading junk before the first RS" do
      data = "junk garbage\x1e1\n".b
      expect(MultiJson.load_sequence(data).to_a).to eq([1])
    end

    it "skips empty and whitespace-only records from consecutive separators" do
      data = "\x1e\n\x1e  \n\x1e\n\x1e1\n".b
      expect(MultiJson.load_sequence(data).to_a).to eq([1])
    end

    it "handles records whose payload contains escaped control characters" do
      value = "tab\ttab newline\nbell\abell escape\x1eescape quote\"end"
      with_each_adapter do |adapter|
        expect(MultiJson.load_sequence(frame(value)).to_a).to eq([value])
      end

      raw = "\x1e\"a\\tb\\nc\\u001ed\"\n".b
      expect(MultiJson.load_sequence(raw).to_a).to eq(["a\tb\nc\u001Ed"])
    end

    it "does not treat a truncated top-level number as a complete value" do
      data = "\x1e123".b
      expect { MultiJson.load_sequence(data).to_a }.to raise_error(MultiJson::RecordTruncated) { |error|
        expect(error.byte_range).to eq(1...4)
        expect(error.raw_bytes).to eq("123".b)
      }

      expect(MultiJson.load_sequence(data, on_bad_record: :skip).to_a).to eq([])
    end

    it "treats a record split by a new RS as truncated and resynchronizes" do
      data = "\x1e123\x1e456\n".b

      truncations = []
      values = MultiJson.load_sequence(data, on_bad_record: ->(error) { truncations << error }).to_a

      expect(values).to eq([456])
      expect(truncations.map(&:class)).to eq([MultiJson::RecordTruncated])
      expect(truncations.first.byte_range).to eq(1...4)
      expect(truncations.first.raw_bytes).to eq("123".b)
    end

    it "raises on a malformed record by default" do
      data = seq_bytes("{bad}", "1")

      expect { MultiJson.load_sequence(data).to_a }.to raise_error(MultiJson::MalformedRecord) { |error|
        expect(error.byte_range).to eq(1...6)
        expect(error.raw_bytes).to eq("{bad}".b)
      }
    end

    it "skips malformed records and continues with later legal records" do
      data = seq_bytes("{bad}", "[1,2]", "nope{")
      expect(MultiJson.load_sequence(data, on_bad_record: :skip).to_a).to eq([[1, 2]])
    end

    it "passes each malformed record to a callable with its raw byte range" do
      data = seq_bytes("{bad}", "[1]")
      errors = []

      MultiJson.load_sequence(data, on_bad_record: ->(error) { errors << error }).to_a

      expect(errors.size).to eq(1)
      error = errors.first
      expect(error).to be_a(MultiJson::MalformedRecord)
      expect(error.byte_range).to eq(1...6)
      expect(data.bytes[error.byte_range].pack("C*")).to eq("{bad}")
      expect(error.raw_bytes).to eq("{bad}".b)
      expect(error.cause).to be_a(MultiJson::ParseError)
    end

    it "rejects trailing garbage after an otherwise valid value" do
      data = seq_bytes("1 2", "3")
      errors = []
      values = MultiJson.load_sequence(data, on_bad_record: ->(error) { errors << error }).to_a

      expect(values).to eq([3])
      expect(errors.first.byte_range).to eq(1...4)
      expect(errors.first.raw_bytes).to eq("1 2".b)
    end

    it "enforces the per-record size limit without buffering the oversized payload" do
      limit = 8
      payload = "x" * 20
      data = seq_bytes(%Q("#{payload}"), "1")

      errors = []
      values = MultiJson.load_sequence(data, max_record_bytes: limit, on_bad_record: ->(error) { errors << error }).to_a

      expect(values).to eq([1])
      expect(errors.size).to eq(1)
      expect(errors.first).to be_a(MultiJson::RecordTooLarge)
      expect(errors.first.limit).to eq(limit)
      expect(errors.first.byte_range).to eq(1...(payload.bytesize + 3))
      expect(errors.first.raw_bytes).to be_nil
    end

    it "accepts records exactly at the size limit" do
      data = seq_bytes("12345", "6")
      values = MultiJson.load_sequence(data, max_record_bytes: 5, on_bad_record: :skip).to_a
      expect(values).to eq([12345, 6])
    end

    it "reads one byte at a time" do
      source = StringIO.new(frame("a", "b"))
      calls = 0
      original = source.method(:getbyte)
      source.define_singleton_method(:getbyte) do
        calls += 1
        original.call
      end

      values = MultiJson.load_sequence(source).to_a

      expect(values).to eq(["a", "b"])
      expect(calls).to eq(frame("a", "b").bytesize + 1)
    end

    it "wraps sources that only expose #read" do
      source = Object.new
      data = frame(1, 2)
      offset = 0
      source.define_singleton_method(:read) do |length|
        slice = data.byteslice(offset, length)
        return nil if slice.nil?

        offset += slice.bytesize
        slice
      end

      expect(MultiJson.load_sequence(source).to_a).to eq([1, 2])
    end

    it "consumes records arriving incrementally over a stream" do
      source = QueuedIO.new
      enum = MultiJson.load_sequence(source).each

      thread = Thread.new do
        source.write("\x1e{")
        sleep 0.02
        source.write("\"a\":1}\n")
        sleep 0.02
        source.write("\x1e2\n")
        source.close
      end

      begin
        expect(enum.next).to eq({"a" => 1})
        expect(enum.next).to eq(2)
        expect { enum.next }.to raise_error(StopIteration)
      ensure
        thread.join
      end
    end

    it "keeps only the current record in memory" do
      source = QueuedIO.new
      enum = MultiJson.load_sequence(source, max_record_bytes: 64 * 1024).each
      large = "y" * 1024

      thread = Thread.new do
        100.times do
          source.write("\x1e\"#{large}\"\n")
          sleep 0.001
        end
        source.close
      end

      count = 0
      begin
        loop { enum.next; count += 1 }
      rescue StopIteration
      ensure
        thread.join
      end

      expect(count).to eq(100)
    end

    it "forwards adapter load options" do
      data = frame({"a" => 1})
      expect(MultiJson.load_sequence(data, symbolize_keys: true).to_a).to eq([{a: 1}])
    end

    it "rejects an invalid bad-record policy" do
      expect {
        MultiJson.load_sequence("", on_bad_record: :explode)
      }.to raise_error(MultiJson::SequenceError, /:on_bad_record/)
    end

    it "rejects unsupported sources" do
      expect {
        MultiJson.load_sequence(42)
      }.to raise_error(MultiJson::SequenceError, /String or respond to/)
    end

    it "tracks the consumed byte position" do
      data = seq_bytes("1", "2")
      reader = MultiJson::SequenceReader.new(data)
      enum = reader.each
      enum.next
      expect(reader.position).to eq(3)
      enum.next
      expect(reader.position).to eq(6)
    end
  end

  describe MultiJson::SequenceWriter do
    it "frames every record with RS and LF" do
      expect(frame(1, 2)).to eq("\x1e1\n\x1e2\n".b)
    end

    it "escapes embedded control characters so framing bytes never appear raw" do
      expect(frame("a\x1eb\nc")).to eq("\x1e\"a\\u001eb\\nc\"\n".b)
    end

    it "writes records directly into an IO" do
      io = StringIO.new("".b)
      MultiJson.write_sequence(io, [1, 2])
      expect(io.string).to eq("\x1e1\n\x1e2\n".b)
    end

    it "supports incremental block writes to an IO" do
      io = StringIO.new("".b)
      MultiJson.write_sequence(io) do |writer|
        writer.write(1)
        writer << 2
      end
      expect(io.string).to eq("\x1e1\n\x1e2\n".b)
    end

    it "streams records as they are written" do
      source = QueuedIO.new

      thread = Thread.new do
        MultiJson.write_sequence(source) do |writer|
          writer.write(1)
          writer.flush
          sleep 0.02
          writer.write(2)
          writer.flush
        end
        source.close
      end

      begin
        expect(source.getbyte).to eq(0x1E)
        rest = [source.getbyte, source.getbyte].pack("C*")
        expect(rest).to eq("1\n".b)
        expect(source.getbyte).to eq(0x1E)
        expect([source.getbyte, source.getbyte].pack("C*")).to eq("2\n".b)
        expect(source.getbyte).to be_nil
      ensure
        thread.join
      end
    end

    it "round-trips through the reader" do
      objects = [1, "two", [3], {"four" => 4}, "x\x1ey\nz"]
      with_each_adapter do |adapter|
        expect(MultiJson.load_sequence(frame(*objects)).to_a).to eq(objects)
      end
    end
  end

  describe "constants" do
    it "exposes the registered media type" do
      expect(MultiJson::SEQUENCE_CONTENT_TYPE).to eq("application/json-seq")
    end
  end
end
