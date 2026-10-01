shared_examples_for "JSON-like adapter" do |adapter|
  before { MultiJson.use adapter }

  describe ".dump" do
    before { MultiJson.dump_options = MultiJson.adapter.dump_options = nil }

    describe "with :pretty option set to true" do
      it "passes default pretty options" do
        object = "foo"
        expect(object).to receive(:to_json).with({
          indent: "  ",
          space: " ",
          object_nl: "\n",
          array_nl: "\n"
        })
        MultiJson.dump(object, pretty: true)
      end
    end

    describe "with :indent option" do
      it "passes it on dump" do
        object = "foo"
        expect(object).to receive(:to_json).with({indent: "\t"})
        MultiJson.dump(object, indent: "\t")
      end
    end

    describe "with a MultiJson::RawNumber" do
      it "dumps the number bare, without string quotes" do
        number = MultiJson::RawNumber.new("1.2300")
        expect(MultiJson.dump(number)).to eq("1.2300")
        expect(MultiJson.dump({"n" => [number, "1.2300"]})).to eq('{"n":[1.2300,"1.2300"]}')
      end
    end
  end

  describe ".load" do
    it "passes :quirks_mode option" do
      expect(JSON).to receive(:parse).with("[123]", {quirks_mode: false, create_additions: false})
      MultiJson.load("[123]", quirks_mode: false)
    end

    it "refuses the :raw_numbers option instead of losing precision" do
      expect { MultiJson.load("[1.2300]", raw_numbers: true) }.to raise_error(ArgumentError, /raw_numbers/)
    end
  end
end
