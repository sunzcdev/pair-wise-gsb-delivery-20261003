require "spec_helper"
require "multi_json/selective_loader"
require "json"
require "stringio"
require "support/chunked_io"

RSpec.describe MultiJson, ".load_paths" do
  let(:adapter_name) { :json_gem }

  def load_paths(io, paths, **options)
    options = {adapter: adapter_name, chunk_size: 7}.merge(options)
    described_class.load_paths(io, paths, **options)
  end

  let(:document) do
    {
      "meta" => {"version" => "1.2.3", "count" => 42, "note" => nil},
      "items" => [
        {"id" => 1, "name" => "alpha"},
        {"id" => 2, "name" => "beta"},
        {"id" => 3, "name" => "gamma"}
      ],
      "blob" => "x" * 5000,
      "nothing" => nil,
      "nested" => {"deep" => {"value" => [true, false, -3.5e2]}}
    }.then { |data| JSON.generate(data) }
  end

  def chunked(string, size = 7)
    ChunkedIO.new(string, size)
  end

  describe "selection results" do
    it "decodes every requested path that exists" do
      result = load_paths(chunked(document), [["meta", "version"], ["meta", "count"]])
      expect(result.values).to eq(
        ["meta", "version"] => "1.2.3",
        ["meta", "count"] => 42
      )
    end

    it "supports integer array indices" do
      result = load_paths(chunked(document), [["items", 1, "name"], ["items", 0, "id"]])
      expect(result.values).to eq(
        ["items", 1, "name"] => "beta",
        ["items", 0, "id"] => 1
      )
    end

    it "supports scalars, booleans and numbers of all shapes" do
      doc = '{"s":"abc","t":true,"f":false,"i":-12,"d":0.25,"e":1.5e-3,"z":0}'
      result = load_paths(chunked(doc),
        [%w[s], %w[t], %w[f], %w[i], %w[d], %w[e], %w[z]])
      expect(result.values).to eq(
        %w[s] => "abc", %w[t] => true, %w[f] => false,
        %w[i] => -12, %w[d] => 0.25, %w[e] => 0.0015, %w[z] => 0
      )
    end

    it "returns selected whole objects and arrays" do
      result = load_paths(chunked(document), [["meta"], ["items", 2]])
      expect(result.values[["meta"]]).to eq("version" => "1.2.3", "count" => 42, "note" => nil)
      expect(result.values[["items", 2]]).to eq("id" => 3, "name" => "gamma")
    end

    it "lists paths that matched explicit null separately from missing paths" do
      result = load_paths(chunked(document),
        [["nothing"], ["meta", "note"], ["meta", "absent"], ["items", 99]])
      expect(result.null).to match_array([["nothing"], ["meta", "note"]])
      expect(result.missing).to match_array([["meta", "absent"], ["items", 99]])
      expect(result.values).to be_empty
      expect(result.null?(["nothing"])).to be(true)
      expect(result.missing?(["meta", "absent"])).to be(true)
      expect(result.found?(["meta", "note"])).to be(true)
      expect(result.found?(["meta", "absent"])).to be(false)
    end

    it "distinguishes an index past the end from a present null element" do
      doc = '{"a":[1,null,3]}'
      result = load_paths(chunked(doc), [["a", 1], ["a", 2], ["a", 3]])
      expect(result.null).to eq([["a", 1]])
      expect(result.values).to eq(["a", 2] => 3)
      expect(result.missing).to eq([["a", 3]])
    end

    it "handles paths into a scalar value as missing" do
      doc = '{"a":5}'
      result = load_paths(chunked(doc), [["a", "b"], ["a", 0]])
      expect(result.missing).to match_array([["a", "b"], ["a", 0]])
      expect(result.values).to be_empty
    end

    it "accepts several overlapping subtrees" do
      result = load_paths(chunked(document),
        [["meta"], ["meta", "version"], ["nested"], ["nested", "deep", "value", 2]])
      expect(result.values[["meta", "version"]]).to eq("1.2.3")
      expect(result.values[["meta"]]).to eq("version" => "1.2.3", "count" => 42, "note" => nil)
      expect(result.values[["nested", "deep", "value", 2]]).to eq(-350.0)
      expect(result.values[["nested"]]).to eq("deep" => {"value" => [true, false, -350.0]})
    end

    it "deduplicates repeated requested paths" do
      result = load_paths(chunked(document), [["meta", "version"], ["meta", "version"]])
      expect(result.values.size).to eq(1)
      expect(result.missing).to be_empty
    end

    it "returns a frozen key path usable for lookup" do
      result = load_paths(chunked(document), [["meta", "version"]])
      path = result.values.keys.first
      expect(path).to eq(["meta", "version"])
      expect(path).to be_frozen
    end

    it "treats \\u-escaped and raw UTF-8 spellings of a key as the same key" do
      doc = '{"café":1,"café":2}'
      expect {
        load_paths(chunked(doc, 4), [["café"]])
      }.to raise_error(described_class::SelectiveLoader::DuplicateKeyError, /café/)
    end

    it "selects a raw UTF-8 key split across chunks" do
      doc = '{"café":"x"}'
      result = load_paths(chunked(doc, 2), [["café"]], chunk_size: 2)
      expect(result.values[["café"]]).to eq("x")
    end

    it "selects a unicode escaped key against a UTF-8 path" do
      doc = '{"café":"x"}'
      result = load_paths(chunked(doc, 3), [["café"]], chunk_size: 3)
      expect(result.values[["café"]]).to eq("x")
    end

    it "selects values from top-level array documents" do
      doc = '[10, {"k":"v"}, null]'
      result = load_paths(chunked(doc, 2), [[1, "k"], [2], [5]])
      expect(result.values[[1, "k"]]).to eq("v")
      expect(result.null).to eq([[2]])
      expect(result.missing).to eq([[5]])
    end

    it "reports paths missing when the root is a scalar" do
      result = load_paths(chunked("123", 2), [["a"], [0]])
      expect(result.missing).to match_array([["a"], [0]])
      expect(result.values).to be_empty
    end

    it "validates a bare scalar document fully" do
      expect { load_paths(chunked("12x", 1), [["a"]]) }
        .to raise_error(described_class::ParseError)
    end
  end

  describe "streaming behavior" do
    it "never reads the whole document in one call and reads it to the end" do
      io = GuardedIO.new(document, 64, max_length: 64)
      result = load_paths(io, [["meta", "version"]], chunk_size: 64)
      expect(result.values[["meta", "version"]]).to eq("1.2.3")
      expect(io.max_request).to eq(64)
      expect(io.max_returned).to eq(64)
      expect(io.total_returned).to eq(document.bytesize)
      expect(io).to be_eof
    end

    it "works with single byte chunks including split escape sequences" do
      doc = '{"a":"line1\\nline2\\u00e9","b":1}'
      result = load_paths(chunked(doc, 1), [%w[a], %w[b]], chunk_size: 1)
      expect(result.values[%w[a]]).to eq("line1\nline2é")
      expect(result.values[%w[b]]).to eq(1)
    end

    it "handles a backslash separated from its escape code by a chunk boundary" do
      doc = '{"a":"tab\\there"}'
      (1..6).each do |size|
        result = load_paths(chunked(doc, size), [%w[a]], chunk_size: size)
        expect(result.values[%w[a]]).to eq("tab\there")
      end
    end

    it "handles a split unicode escape and surrogate pairs" do
      doc = '{"a":"x\\u00e9y","emoji":"\\uD83D\\uDE00"}'
      result = load_paths(chunked(doc, 3), [%w[a], %w[emoji]], chunk_size: 3)
      expect(result.values[%w[a]]).to eq("xéy")
      expect(result.values[%w[emoji]]).to eq("\u{1F600}")
    end

    it "does not hand the giant unselected string to the adapter" do
      huge = "y" * 200_000
      doc = "{\"small\":1,\"blob\":\"#{huge}\",\"tail\":2}"
      seen = []
      allow(MultiJson::Adapters::JsonGem).to receive(:load) do |raw, _opts|
        seen << raw.bytesize
        JSON.parse(raw)
      end
      io = GuardedIO.new(doc, 256, max_length: 256)
      result = load_paths(io, [%w[small]], chunk_size: 256)
      expect(result.values[%w[small]]).to eq(1)
      expect(seen).to eq([1])
    end

    it "only hands small selected snippets to the adapter" do
      doc = "{\"blob\":\"#{"z" * 100_000}\",\"kept\":\"abc\"}"
      seen = []
      allow(MultiJson::Adapters::JsonGem).to receive(:load) do |raw, _opts|
        seen << raw.bytesize
        JSON.parse(raw)
      end
      load_paths(chunked(doc, 128), [%w[kept]], chunk_size: 128)
      expect(seen).to eq([5])
    end

    it "does not call MultiJson.load or read the document up front" do
      io = GuardedIO.new(document, 32, max_length: 32)
      expect(described_class).not_to receive(:load)
      load_paths(io, [["meta", "version"]], chunk_size: 32)
    end

    it "bounds its read requests when using the default chunk size" do
      huge = "q" * 100_000
      doc = "{\"blob\":\"#{huge}\",\"kept\":\"v\"}"
      # max_length equals the loader default; a read without an explicit
      # length (i.e. "read the rest") would raise.
      io = GuardedIO.new(doc, 4096, max_length: 4096)
      result = described_class.load_paths(io, [%w[kept]], adapter: :json_gem)
      expect(result.values[%w[kept]]).to eq("v")
      expect(io.max_request).to eq(4096)
    end
  end

  describe "whole document validation" do
    it "fails on a syntax error at the end even though the path was found" do
      doc = '{"meta":{"version":"1.2"},"tail":} '
      io = chunked(doc, 1)
      expect {
        load_paths(io, [["meta", "version"]], chunk_size: 1)
      }.to raise_error(described_class::ParseError)
      # The found path ends around byte 24; parsing streamed all the way to
      # the bad byte near the end instead of returning success early.
      expect(io.total_returned).to be >= doc.bytesize - 2
    end

    it "fails on trailing garbage after a complete document" do
      doc = '{"a":1} garbage'
      expect {
        load_paths(chunked(doc), [%w[a]])
      }.to raise_error(described_class::ParseError, /trailing data/)
    end

    it "fails on truncated documents" do
      doc = '{"a":"unterminated'
      expect {
        load_paths(chunked(doc, 4), [%w[a]])
      }.to raise_error(described_class::ParseError)
    end

    it "rejects trailing commas in objects and arrays" do
      expect { load_paths(chunked('{"a":1,}', 3), [%w[a]]) }
        .to raise_error(described_class::ParseError)
      expect { load_paths(chunked('{"a":[1,2,]}', 3), [%w[a]]) }
        .to raise_error(described_class::ParseError)
    end

    it "rejects malformed numbers and control characters in strings" do
      expect { load_paths(chunked('{"a":01}', 2), [%w[a]]) }
        .to raise_error(described_class::ParseError)
      expect { load_paths(chunked('{"a":1.}', 2), [%w[a]]) }
        .to raise_error(described_class::ParseError)
      expect { load_paths(chunked("{\"a\":\"\x01\"}", 2), [%w[a]]) }
        .to raise_error(described_class::ParseError)
      expect { load_paths(chunked('{"a":tru}', 2), [%w[a]]) }
        .to raise_error(described_class::ParseError)
    end

    it "rejects invalid escape sequences and lone surrogates, even unselected" do
      expect { load_paths(chunked('{"a":"bad \\x escape"}', 3), [%w[z]]) }
        .to raise_error(described_class::ParseError)
      expect { load_paths(chunked('{"a":"lone \\uD800"}', 3), [%w[z]]) }
        .to raise_error(described_class::ParseError)
      expect { load_paths(chunked('{"a":"\\uDC00 low"}', 3), [%w[z]]) }
        .to raise_error(described_class::ParseError)
    end

    it "rejects malformed UTF-8 anywhere" do
      expect { load_paths(chunked(%({"a":"\xFF"}), 2), [%w[z]]) }
        .to raise_error(described_class::ParseError)
    end

    it "rejects blank documents" do
      expect { load_paths(chunked("   ", 2), [%w[a]]) }
        .to raise_error(described_class::ParseError)
    end

    it "fails when nothing is selected but the document is invalid" do
      doc = '{"unrelated": [1, 2, }'
      expect {
        load_paths(chunked(doc, 5), [%w[wanted]])
      }.to raise_error(described_class::ParseError)
    end
  end

  describe "duplicate object keys" do
    it "raises DuplicateKeyError for duplicates in the whole document" do
      doc = '{"a":1,"other":{"x":1,"x":2}}'
      expect {
        load_paths(chunked(doc, 4), [%w[a]])
      }.to raise_error(described_class::SelectiveLoader::DuplicateKeyError, /x/)
    end

    it "rejects duplicates even inside fully unselected subtrees" do
      doc = '{"keep":1,"blob":{"k":1,"k":2}}'
      expect {
        load_paths(chunked(doc, 3), [%w[keep]])
      }.to raise_error(described_class::SelectiveLoader::DuplicateKeyError)
    end

    it "is a kind of ParseError" do
      expect(described_class::SelectiveLoader::DuplicateKeyError.ancestors)
        .to include(described_class::ParseError)
    end

    it "detects duplicates split across chunk boundaries" do
      doc = '{"name":"a","name":"b"}'
      expect {
        load_paths(chunked(doc, 1), [%w[name]], chunk_size: 1)
      }.to raise_error(described_class::SelectiveLoader::DuplicateKeyError, /name/)
    end
  end

  describe "arguments" do
    it "requires an explicit adapter" do
      expect {
        described_class.load_paths(StringIO.new("{}"), [%w[a]])
      }.to raise_error(ArgumentError, /adapter/)
    end

    it "rejects IO-like inputs without #read" do
      expect {
        load_paths("{}", [%w[a]])
      }.to raise_error(ArgumentError, /IO/)
    end

    it "rejects paths that are not arrays of String/Integer segments" do
      expect { load_paths(StringIO.new("{}"), "a") }
        .to raise_error(ArgumentError)
      expect { load_paths(StringIO.new("{}"), ["a"]) }
        .to raise_error(ArgumentError)
      expect { load_paths(StringIO.new("{}"), [[nil]]) }
        .to raise_error(ArgumentError)
      expect { load_paths(StringIO.new("{}"), [[:a]]) }
        .to raise_error(ArgumentError)
      expect { load_paths(StringIO.new("{}"), [[-1]]) }
        .to raise_error(ArgumentError)
    end

    it "rejects unknown duplicate key strategies" do
      expect {
        load_paths(StringIO.new("{}"), [%w[a]], duplicate_keys: :ignore)
      }.to raise_error(ArgumentError)
    end

    it "rejects non-positive chunk sizes" do
      expect {
        load_paths(StringIO.new("{}"), [%w[a]], chunk_size: 0)
      }.to raise_error(ArgumentError)
    end
  end

  describe "adapter integration" do
    it "uses the explicitly chosen adapter, not the current one" do
      described_class.use :ok_json
      result = load_paths(chunked(document), [["meta", "version"]])
      expect(result.values[["meta", "version"]]).to eq("1.2.3")
      expect(described_class.adapter).to eq(MultiJson::Adapters::OkJson)
    end

    it "works with the json_pure adapter", :json_pure do
      result = described_class.load_paths(chunked(document, 5),
        [["meta", "version"], ["items", 0, "name"]],
        adapter: :json_pure, chunk_size: 5)
      expect(result.values[["meta", "version"]]).to eq("1.2.3")
    end

    it "works with the vendored ok_json adapter" do
      require "multi_json/adapters/ok_json"
      result = described_class.load_paths(chunked(document, 5),
        [["meta", "version"], ["meta", "count"], ["items", 1]],
        adapter: :ok_json, chunk_size: 5)
      expect(result.values[["meta", "version"]]).to eq("1.2.3")
      expect(result.values[["meta", "count"]]).to eq(42)
      expect(result.values[["items", 1]]).to eq("id" => 2, "name" => "beta")
    end

    it "passes symbolize_keys through to the adapter" do
      result = load_paths(chunked(document),
        [["items", 0]], symbolize_keys: true)
      expect(result.values[["items", 0]]).to eq(id: 1, name: "alpha")
    end

    it "surfaces adapter parse failures as MultiJson::ParseError" do
      # The selected value is validated structurally, but a crafted snippet
      # rejected by the chosen adapter must still surface consistently.
      allow(MultiJson::Adapters::JsonGem).to receive(:load).and_raise(JSON::ParserError.new("boom"))
      expect {
        load_paths(chunked(document), [["meta", "version"]])
      }.to raise_error(described_class::ParseError)
    end
  end

  describe "SelectiveLoader reuse" do
    it "can be constructed once and run against several IO objects" do
      loader = described_class::SelectiveLoader.new(
        [["meta", "version"]], adapter: :json_gem, chunk_size: 11
      )
      r1 = loader.load(chunked(document, 11))
      r2 = loader.load(StringIO.new(document))
      expect(r1.values[["meta", "version"]]).to eq("1.2.3")
      expect(r2.values[["meta", "version"]]).to eq("1.2.3")
    end
  end
end
