require "spec_helper"
require "multi_json/adapter"

RSpec.describe MultiJson::UnsupportedOptionError do
  it "is an ArgumentError" do
    expect(described_class.new("x")).to be_a(ArgumentError)
  end

  it ".build names the adapter and the option" do
        klass = Class.new(MultiJson::Adapter) do
          def self.name
            "MultiJson::Adapters::Example"
          end
        end
        error = described_class.build(klass, :preserve_numbers, "use another adapter")
        expect(error.message).to include("Example")
        expect(error.message).to include("preserve_numbers")
        expect(error.message).to include("use another adapter")
      end
end

RSpec.describe "adapter :preserve_numbers capability" do
  describe "lossless adapters" do
    it "is supported by ok_json" do
      require "multi_json/adapters/ok_json"
      expect(MultiJson::Adapters::OkJson.supports?(:preserve_numbers)).to be(true)
    end

    it "is supported by the json_gem adapter when decimal_class exists", :json do
      require "multi_json/adapters/json_gem"
      expect(MultiJson::Adapters::JsonGem.supports?(:preserve_numbers)).to be(true)
    end

    it "is supported by the json_pure adapter when decimal_class exists", :json_pure do
      require "multi_json/adapters/json_pure"
      expect(MultiJson::Adapters::JsonPure.supports?(:preserve_numbers)).to be(true)
    end
  end

  describe "a backend that can only produce Float" do
    let!(:float_only) { MultiJson::Adapters::FloatOnly }

    before(:all) do
      module MultiJson::Adapters
        class FloatOnly < MultiJson::Adapter
          ParseError = Class.new(StandardError)

          def load(string, _options)
            # Pretend every JSON number became a Float, like many C backends.
            string == "1.20" ? {"x" => 1.2} : 1.2
          end

          def dump(object, _options)
            object.inspect
          end
        end
      end
    end

    it "declares the feature unsupported" do
      expect(float_only.supports?(:preserve_numbers)).to be(false)
    end

    it "refuses explicitly instead of silently rounding through Float" do
      MultiJson.use(float_only)
      expect {
        MultiJson.load("1.20", preserve_numbers: true)
      }.to raise_error(MultiJson::UnsupportedOptionError, /preserve_numbers/)
    end

    it "surfaces the refusal as UnsupportedOptionError, never as a ParseError" do
      MultiJson.use(float_only)
      error = nil
      begin
        MultiJson.load("1.20", preserve_numbers: true)
      rescue StandardError => error
      end
      expect(error).to be_a(MultiJson::UnsupportedOptionError)
      expect(error).not_to be_a(MultiJson::ParseError)
    end

    it "still loads normally without the option" do
      MultiJson.use(float_only)
      result = MultiJson.load("1.20")
      expect(result).to eq("x" => 1.2)
    end

    it "does not pretend the Float result preserves precision" do
      MultiJson.use(float_only)
      expect {
        MultiJson.load("1.20", preserve_numbers: true)
      }.to raise_error(MultiJson::UnsupportedOptionError)
    end
  end

  describe "global load options" do
    it "honours preserve_numbers set globally for a supporting adapter" do
      require "multi_json/adapters/ok_json"
      with_default_options do
        MultiJson.use(MultiJson::Adapters::OkJson)
        MultiJson.load_options = {preserve_numbers: true}
        value = MultiJson.load("1.2300")
        expect(value).to be_a(MultiJson::LexicalNumber)
        expect(value.lexeme).to eq("1.2300")
      end
    end
  end
end
