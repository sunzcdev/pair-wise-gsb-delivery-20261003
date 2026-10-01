require "shared/options"
require "stringio"

shared_examples_for "an adapter" do |adapter|
  before { MultiJson.use adapter }

  it_behaves_like "has options", adapter

  it "does not modify argument hashes" do
    options = {symbolize_keys: true, pretty: false, adapter: :ok_json}
    expect { MultiJson.load("{}", options) }.not_to(change { options })
    expect { MultiJson.dump([42], options) }.not_to(change { options })
  end

  describe ".dump" do
    let(:json_pure) { Kernel.const_get("MultiJson::Adapters::JsonPure") rescue nil }
    describe "#dump_options" do
      before { MultiJson.dump_options = MultiJson.adapter.dump_options = {} }

      after do
        MultiJson.dump(1, fizz: "buzz")
        MultiJson.dump_options = MultiJson.adapter.dump_options = nil
      end

      it "respects global dump options" do
        MultiJson.dump_options = {foo: "bar"}
        expect(MultiJson.dump_options).to eq({foo: "bar"})
        expect(MultiJson.adapter.instance).to receive(:dump).with(1, {foo: "bar", fizz: "buzz"})
      end

      it "respects per-adapter dump options" do
        MultiJson.adapter.dump_options = {foo: "bar"}
        expect(MultiJson.adapter.dump_options).to eq({foo: "bar"})
        expect(MultiJson.adapter.instance).to receive(:dump).with(1, {foo: "bar", fizz: "buzz"})
      end

      it "adapter-specific are overridden by global options" do
        MultiJson.adapter.dump_options = {foo: "foo"}
        MultiJson.dump_options = {foo: "bar"}
        expect(MultiJson.adapter.dump_options).to eq({foo: "foo"})
        expect(MultiJson.dump_options).to eq({foo: "bar"})
        expect(MultiJson.adapter.instance).to receive(:dump).with(1, {foo: "bar", fizz: "buzz"})
      end
    end

    it "writes decodable JSON" do
      examples = [
        {"abc" => "def"},
        [],
        1,
        "2",
        true,
        false,
        nil
      ]

      examples.each do |example|
        expect(MultiJson.load(MultiJson.dump(example))).to eq(example)
      end
    end

    it "dumps time in correct format" do
      time = Time.at(1_355_218_745).utc

      dumped_json = MultiJson.dump(time)
      expected = "2012-12-11 09:39:05 UTC"
      expect(MultiJson.load(dumped_json)).to eq(expected)
    end

    it "dumps symbol and fixnum keys as strings" do
      [
        [
          {foo: {bar: "baz"}},
          {"foo" => {"bar" => "baz"}}
        ],
        [
          [{foo: {bar: "baz"}}],
          [{"foo" => {"bar" => "baz"}}]
        ],
        [
          {foo: [{bar: "baz"}]},
          {"foo" => [{"bar" => "baz"}]}
        ],
        [
          {1 => {2 => {3 => "bar"}}},
          {"1" => {"2" => {"3" => "bar"}}}
        ]
      ].each do |example, expected|
        dumped_json = MultiJson.dump(example)
        expect(MultiJson.load(dumped_json)).to eq(expected)
      end
    end

    it "dumps rootless JSON" do
      expect(MultiJson.dump("random rootless string")).to eq('"random rootless string"')
      expect(MultiJson.dump(123)).to eq("123")
    end

    it "passes options to the adapter" do
      expect(MultiJson.adapter).to receive(:dump).with("foo", {bar: :baz})
      MultiJson.dump("foo", {bar: :baz})
    end

    it "dumps custom objects that implement to_json" do
      pending "not supported" if adapter.name == "MultiJson::Adapters::Gson"
      klass = Class.new do
        def to_json(*)
          '"foobar"'
        end
      end
      expect(MultiJson.dump(klass.new)).to eq('"foobar"')
    end

    it "allows to dump JSON values" do
      expect(MultiJson.dump(42)).to eq("42")
    end

    it "allows to dump JSON with UTF-8 characters" do
      expect(MultiJson.dump("color" => "żółć")).to eq('{"color":"żółć"}')
    end
  end

  describe ".load" do
    describe "#load_options" do
      before { MultiJson.load_options = MultiJson.adapter.load_options = {} }

      after do
        MultiJson.load("1", fizz: "buzz")
        MultiJson.load_options = MultiJson.adapter.load_options = nil
      end

      it "respects global load options" do
        MultiJson.load_options = {foo: "bar"}
        expect(MultiJson.load_options).to eq({foo: "bar"})
        expect(MultiJson.adapter.instance).to receive(:load).with("1", {foo: "bar", fizz: "buzz"})
      end

      it "respects per-adapter load options" do
        MultiJson.adapter.load_options = {foo: "bar"}
        expect(MultiJson.adapter.load_options).to eq({foo: "bar"})
        expect(MultiJson.adapter.instance).to receive(:load).with("1", {foo: "bar", fizz: "buzz"})
      end

      it "adapter-specific are overridden by global options" do
        MultiJson.adapter.load_options = {foo: "foo"}
        MultiJson.load_options = {foo: "bar"}
        expect(MultiJson.adapter.load_options).to eq({foo: "foo"})
        expect(MultiJson.load_options).to eq({foo: "bar"})
        expect(MultiJson.adapter.instance).to receive(:load).with("1", {foo: "bar", fizz: "buzz"})
      end
    end

    it "does not modify input" do
      input = %(\n\n  {"foo":"bar"} \n\n\t)
      expect do
        MultiJson.load(input)
      end.not_to(change { input })
    end

    it "does not modify input encoding" do
      input = "[123]"
      input.force_encoding("iso-8859-1")

      expect do
        MultiJson.load(input)
      end.not_to(change { input.encoding })
    end

    it "properly loads valid JSON" do
      expect(MultiJson.load('{"abc":"def"}')).to eq("abc" => "def")
    end

    examples = [nil, '{"abc"}', " ", "\t\t\t", "\n", StringIO.new("")]
    #
    # GSON bug: https://github.com/avsej/gson.rb/issues/3
    examples << "\x82\xAC\xEF" unless adapter.name.include?("Gson")

    examples.each do |input|
      it "raises MultiJson::ParseError on invalid input: #{input.inspect}" do
        expect { MultiJson.load(input) }.to raise_error(MultiJson::ParseError)
      end
    end

    it "raises MultiJson::ParseError with data on invalid JSON" do
      data = "{invalid}"
      exception = get_exception(MultiJson::ParseError) { MultiJson.load data }
      expect(exception.data).to eq(data)
      expect(exception.cause).to match(adapter::ParseError)
    end

    it "catches MultiJson::DecodeError for legacy support" do
      data = "{invalid}"
      exception = get_exception(MultiJson::DecodeError) { MultiJson.load data }
      expect(exception.data).to eq(data)
      expect(exception.cause).to match(adapter::ParseError)
    end

    it "catches MultiJson::LoadError for legacy support" do
      data = "{invalid}"
      exception = get_exception(MultiJson::LoadError) { MultiJson.load data }
      expect(exception.data).to eq(data)
      expect(exception.cause).to match(adapter::ParseError)
    end

    it "stringifys symbol keys when encoding" do
      dumped_json = MultiJson.dump(a: 1, b: {c: 2})
      loaded_json = MultiJson.load(dumped_json)
      expect(loaded_json).to eq("a" => 1, "b" => {"c" => 2})
    end

    it "properly loads valid JSON in StringIOs" do
      json = StringIO.new('{"abc":"def"}')
      expect(MultiJson.load(json)).to eq("abc" => "def")
    end

    it "allows for symbolization of keys" do
      [
        [
          '{"abc":{"def":"hgi"}}',
          {abc: {def: "hgi"}}
        ],
        [
          '[{"abc":{"def":"hgi"}}]',
          [{abc: {def: "hgi"}}]
        ],
        [
          '{"abc":[{"def":"hgi"}]}',
          {abc: [{def: "hgi"}]}
        ]
      ].each do |example, expected|
        expect(MultiJson.load(example, symbolize_keys: true)).to eq(expected)
      end
    end

    it "allows to load JSON values" do
      expect(MultiJson.load("42")).to eq(42)
    end

    it "allows to load JSON with UTF-8 characters" do
      expect(MultiJson.load('{"color":"żółć"}')).to eq("color" => "żółć")
    end

    describe ":preserve_numbers" do
      let(:number) { MultiJson::LexicalNumber }

      it "leaves ordinary parsing as Integer/Float when the option is off" do
        expect(MultiJson.load("1.5")).to eq(1.5)
        expect(MultiJson.load("1.5")).to be_a(Float)
        expect(MultiJson.load("42")).to eq(42)
        expect(MultiJson.load("42")).to be_a(Integer)
      end

      it "returns LexicalNumber objects carrying the exact original spelling" do
        value = MultiJson.load("1.2300", preserve_numbers: true)
        expect(value).to be_a(number)
        expect(value.lexeme).to eq("1.2300")
      end

      it "keeps spellings that Float collapses into the same value" do
        examples = {"1.2300" => "1.2300", "123e-2" => "123e-2", "1.23" => "1.23"}
        parsed = examples.keys.map { |s| MultiJson.load(s, preserve_numbers: true) }
        expect(parsed.map(&:lexeme)).to eq(examples.values)
      end

      it "preserves integers larger than Float can represent without rounding" do
        value = MultiJson.load("9007199254740993", preserve_numbers: true)
        expect(value).to be_a(number)
        expect(value.to_i).to eq(9_007_199_254_740_993)
        expect(value.to_f).to eq(9_007_199_254_740_992.0) # demonstrates Float is lossy
        expect(value.lexeme).to eq("9007199254740993")
      end

      it "preserves an integral value reached through an exponent spelling" do
        value = MultiJson.load("1e3", preserve_numbers: true)
        expect(value).to be_a(number)
        expect(value.to_i).to eq(1000)
        expect(value.lexeme).to eq("1e3")
      end

      it "preserves very long fractional parts" do
        text = "3.14159265358979323846264338327950288"
        value = MultiJson.load(text, preserve_numbers: true)
        expect(value.lexeme).to eq(text)
        expect(value.to_d.to_s).to eq("0.314159265358979323846264338327950288e1")
      end

      it "walks nested objects and arrays" do
        json = '{"a":[{"b":[1.10,2,1e3]}],"c":-0.0}'
        result = MultiJson.load(json, preserve_numbers: true)
        expect(result["a"][0]["b"][0]).to be_a(number)
        expect(result["a"][0]["b"][0].lexeme).to eq("1.10")
        expect(result["a"][0]["b"][1]).to be_a(number) # integers are preserved too
        expect(result["a"][0]["b"][1].to_i).to eq(2)
        expect(result["a"][0]["b"][2]).to be_a(number)
        expect(result["a"][0]["b"][2].lexeme).to eq("1e3")
        expect(result["c"].lexeme).to eq("-0.0")
      end

      it "keeps JSON strings as String, even when they look like numbers" do
        result = MultiJson.load('{"n":1.20,"s":"1.20"}', preserve_numbers: true)
        expect(result["n"]).to be_a(number)
        expect(result["s"]).to be_a(String)
        expect(result["s"]).to eq("1.20")
        expect(result["n"]).not_to be_a(String)
      end

      it "decodes escape sequences in strings while preserving numbers" do
        # JSON text whose string value contains an escaped quote, backslash,
        # newline and tab; the number next to it must stay a LexicalNumber.
        json = '{"s":"quote:\\" back:\\\\ nl:\n tab:\t","n":1.20}'
        result = MultiJson.load(json, preserve_numbers: true)
        expect(result["s"]).to eq("quote:\" back:\\ nl:\n tab:\t")
        expect(result["n"].lexeme).to eq("1.20")
      end

      it "round-trips through dump as an unquoted legal number" do
        json = '{"price":1.2300,"big":12345678901234567890.50}'
        dumped = MultiJson.dump(MultiJson.load(json, preserve_numbers: true))
        expect(dumped).to eq(json)
        expect(dumped).not_to include('"1.2300"')
        reparsed = MultiJson.load(dumped, preserve_numbers: true)
        expect(reparsed["price"].lexeme).to eq("1.2300")
      end

      it "dumps a scalar number without quotes" do
        expect(MultiJson.dump(number.new("1.2300"))).to eq("1.2300")
      end

      it "does not mutate the caller's options hash" do
        options = {preserve_numbers: true}
        MultiJson.load("1.20", options)
        expect(options).to eq(preserve_numbers: true)
      end

      it "works together with symbolize_keys" do
        result = MultiJson.load('{"a":1.20}', preserve_numbers: true, symbolize_keys: true)
        expect(result[:a].lexeme).to eq("1.20")
      end

      %w[01 +1 .5 1. 1e NaN Infinity 0x10 1_000].each do |invalid|
        it "rejects illegal number #{invalid.inspect}" do
          expect {
            MultiJson.load(invalid, preserve_numbers: true)
          }.to raise_error(MultiJson::ParseError)
        end
      end
    end
  end
end
