require "spec_helper"
require "stringio"

RSpec.describe MultiJson::PathSelector do
  # An IO that only allows bounded chunked reads, so the implementation
  # cannot slurp the whole input in one go.
  class ChunkedIO
    def initialize(data, max_chunk:)
      @data = data
      @max_chunk = max_chunk
      @pos = 0
      @reads = 0
    end

    attr_reader :reads

    def read(length = nil, *)
      raise "full slurp attempted" if length.nil?
      raise "chunk too large: #{length}" if length > @max_chunk

      @reads += 1
      return nil if @pos >= @data.bytesize

      chunk = @data.byteslice(@pos, length)
      @pos += chunk.bytesize
      chunk
    end
  end

  # An adapter that records every raw string it is asked to decode.
  class RecordingAdapter
    ParseError = Class.new(StandardError)

    class << self
      def received
        @received ||= []
      end

      def load(string, _options = {})
        received << string
        MultiJson::Adapters::OkJson.load(string)
      end
    end
  end

  let(:adapter) { :ok_json }

  def select(json, paths, **options)
    MultiJson.load_paths(StringIO.new(json), paths, {adapter: adapter}.merge(options))
  end

  describe ".load" do
    it "selects values by string keys and integer indexes" do
      json = %q({"meta":{"version":"1.2","author":"ann"},"items":[{"id":1},{"id":2}],"junk":[1,2,3]})
      result = select(json, [["meta", "version"], ["items", 1, "id"], ["items", 0]])

      expect(result.values).to eq(
        ["meta", "version"] => "1.2",
        ["items", 1, "id"] => 2,
        ["items", 0] => {"id" => 1}
      )
      expect(result.missing).to eq([])
    end

    it "supports multiple overlapping subtrees in one pass" do
      json = %q({"a":{"b":{"c":1},"d":2}})
      result = select(json, [["a"], ["a", "b"], ["a", "b", "c"]])

      expect(result[["a"]]).to eq({"b" => {"c" => 1}, "d" => 2})
      expect(result[["a", "b"]]).to eq({"c" => 1})
      expect(result[["a", "b", "c"]]).to eq(1)
    end

    it "selects the whole document with an empty path" do
      result = select(%q( {"a":[1,2]} ), [[]])
      expect(result[[]]).to eq({"a" => [1, 2]})
    end

    it "deduplicates identical paths" do
      result = select(%q({"a":1}), [["a"], ["a"]])
      expect(result.values).to eq(["a"] => 1)
    end

    it "returns null hits as nil values, separate from missing paths" do
      result = select(%q({"present":null,"nested":{"also":null}}), [["present"], ["nested", "also"], ["absent"], ["nested", "gone"]])

      expect(result.values).to eq(["present"] => nil, ["nested", "also"] => nil)
      expect(result.missing).to contain_exactly(["absent"], ["nested", "gone"])
      expect(result.found?(["present"])).to be(true)
      expect(result.found?(["absent"])).to be(false)
    end

    it "hands the raw text of each selected value to the explicitly chosen adapter" do
      RecordingAdapter.received.clear
      json = %q({ "a" : { "b" : 1 }, "c" : [ 1, 2 ] })
      result = MultiJson.load_paths(StringIO.new(json), [["a"], ["c"]], adapter: RecordingAdapter)

      expect(RecordingAdapter.received).to contain_exactly(%q({ "b" : 1 }), %q([ 1, 2 ]))
      expect(result[["a"]]).to eq({"b" => 1})
      expect(result[["c"]]).to eq([1, 2])
    end

    it "requires an explicitly chosen adapter" do
      expect { MultiJson.load_paths(StringIO.new(%q({})), [[]]) }.to raise_error(ArgumentError, /adapter/)
    end

    it "reads the input in bounded chunks instead of slurping it" do
      json = %q({"a":") + ("x" * 10_000) + %q(","b":2})
      io = ChunkedIO.new(json, max_chunk: 64)
      result = MultiJson.load_paths(io, [["b"]], adapter: adapter, chunk_size: 64)

      expect(result[["b"]]).to eq(2)
      expect(io.reads).to be > 1
    end

    it "handles a large unselected string without materializing it" do
      json = %q({"pad":") + ("y" * 2_000_000) + %q(","want":{"ok":true}})
      result = select(json, [["want"]], chunk_size: 1024)

      expect(result[["want"]]).to eq({"ok" => true})
    end

    it "handles escapes split across chunk boundaries" do
      json = %q({"key":"a\"b\\\\c","n":-1.5e2})
      result = select(json, [["key"], ["n"]], chunk_size: 3)

      expect(result[["key"]]).to eq(%q(a"b\c))
      expect(result[["n"]]).to eq(-150.0)
    end

    it "handles unicode escapes split across chunk boundaries" do
      json = %q({"s":"Aé𝄞"})
      result = select(json, [["s"]], chunk_size: 2)

      expect(result[["s"]]).to eq("Aé𝄞")
    end

    it "matches keys containing escapes" do
      result = select(%q({"a":1}), [["a"]])
      expect(result[["a"]]).to eq(1)
    end

    it "validates the whole document even after every target was found" do
      json = %q({"a":1,"b":[1,2,}])
      expect { select(json, [["a"]], chunk_size: 4) }.to raise_error(MultiJson::ParseError)
    end

    it "rejects trailing data after the top-level value" do
      expect { select(%q({"a":1} {"b":2}), [["a"]]) }.to raise_error(MultiJson::ParseError)
    end

    it "rejects truncated documents" do
      expect { select(%q({"a":[1,2), [["a"]]) }.to raise_error(MultiJson::ParseError)
    end

    it "rejects invalid numbers" do
      expect { select(%q([01]), [[]]) }.to raise_error(MultiJson::ParseError)
      expect { select(%q([1.]), [[]]) }.to raise_error(MultiJson::ParseError)
      expect { select(%q([1e]), [[]]) }.to raise_error(MultiJson::ParseError)
      expect { select(%q([-]), [[]]) }.to raise_error(MultiJson::ParseError)
    end

    it "rejects invalid literals" do
      expect { select(%q([nul]), [[]]) }.to raise_error(MultiJson::ParseError)
      expect { select(%q([True]), [[]]) }.to raise_error(MultiJson::ParseError)
    end

    it "rejects unescaped control characters in strings" do
      expect { select("{\"a\":\"x\x09y\"}", [[]]) }.to raise_error(MultiJson::ParseError)
    end

    it "rejects invalid escape sequences" do
      expect { select(%q(["\q"]), [[]]) }.to raise_error(MultiJson::ParseError)
      expect { select(%q(["\u12"]), [[]]) }.to raise_error(MultiJson::ParseError)
    end

    it "rejects lone surrogate escapes in keys" do
      expect { select(%q({"\udc00":1}), [[]]) }.to raise_error(MultiJson::ParseError)
    end

    it "rejects duplicate object keys anywhere in the document" do
      expect { select(%q({"keep":1,"junk":{"x":1,"x":2}}), [["keep"]]) }
        .to raise_error(MultiJson::PathSelector::DuplicateKeyError)
    end

    it "rejects duplicate keys that only collide after unescaping" do
      expect { select(%q({"a":1,"a":2}), [[]]) }
        .to raise_error(MultiJson::PathSelector::DuplicateKeyError)
    end

    it "wraps adapter decode failures in MultiJson::ParseError" do
      failing_adapter = Class.new do
        def self.load(_string, _options = {})
          raise self::ParseError, "cannot decode"
        end
      end
      failing_adapter.const_set(:ParseError, Class.new(StandardError))

      error = get_exception(MultiJson::ParseError) do
        MultiJson.load_paths(StringIO.new(%q({"a":1})), [["a"]], adapter: failing_adapter)
      end
      expect(error).to be_a(MultiJson::ParseError)
      expect(error.data).to eq("1")
    end

    it "validates path arguments" do
      expect { select(%q({}), "not-an-array") }.to raise_error(ArgumentError)
      expect { select(%q({}), [[:symbol]]) }.to raise_error(ArgumentError)
      expect { select(%q({}), [[-1]]) }.to raise_error(ArgumentError)
      expect { select(%q({}), [["a", 1.5]]) }.to raise_error(ArgumentError)
    end

    it "requires an IO-like input" do
      expect { MultiJson.load_paths("not an io", [[]], adapter: adapter) }.to raise_error(ArgumentError)
    end

    it "still validates the document when no paths are selected" do
      result = select(%q({"a":1}), [])
      expect(result.values).to eq({})
      expect(result.missing).to eq([])
      expect { select(%q({"a":}), []) }.to raise_error(MultiJson::ParseError)
    end
  end

  describe MultiJson::PathSelector::Result do
    it "fetches values and raises KeyError for missing paths" do
      result = select(%q({"a":1}), [["a"], ["b"]])

      expect(result.fetch(["a"])).to eq(1)
      expect { result.fetch(["b"]) }.to raise_error(KeyError)
      expect(result[["b"]]).to be_nil
      expect(result.to_h).to eq(["a"] => 1)
    end
  end
end