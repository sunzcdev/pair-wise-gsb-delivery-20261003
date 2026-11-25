# frozen_string_literal: true

require "dry/system/provider_sources/settings/loader"
require "dry/system/provider_sources/settings/config"

RSpec.describe Dry::System::ProviderSources::Settings::Config do
  let(:env_store) { {} }

  let(:loader_class) do
    store = env_store

    Class.new(Dry::System::ProviderSources::Settings::Loader) do
      define_method(:initialize) do |root:, env:, roots: nil|
        super(root: root, env: env, roots: roots, store: store)
      end
    end
  end

  let(:root) { make_tmp_directory }

  describe ".settings_group" do
    let(:settings_class) do
      Class.new(described_class) do
        settings_group(:connection_pool) do
          setting :pool_min
          setting :pool_max
        end

        setting :app_name
      end
    end

    it "registers the grouped setting names" do
      group = settings_class.setting_groups[:connection_pool]

      expect(group).to be_a(Dry::System::ProviderSources::Settings::Group)
      expect(group.keys).to eq %i[pool_min pool_max]
    end

    it "does not group ordinary settings" do
      expect(settings_class.setting_groups.values.flat_map(&:keys)).not_to include(:app_name)
    end

    it "keeps group declarations on subclasses" do
      subclass = Class.new(settings_class)

      expect(subclass.setting_groups[:connection_pool].keys).to eq %i[pool_min pool_max]
    end
  end

  describe ".load" do
    let(:settings_class) do
      Class.new(described_class) do
        settings_group(:connection_pool, validate: -> values {
          raise "max must be >= min" if values[:pool_max] < values[:pool_min]
        }) do
          setting :pool_min, constructor: Types::Coercible::Integer
          setting :pool_max, constructor: Types::Coercible::Integer
        end

        setting :app_name, constructor: Types::String
      end
    end

    before do
      write(root.join(".env.test"), "POOL_MIN=5\nPOOL_MAX=10\nAPP_NAME=app\n")
    end

    def load(extra_store = env_store)
      settings_class.load(root: root, env: :test, loader: loader_class)
    end

    it "loads grouped and ordinary settings together" do
      settings = load

      expect(settings.pool_min).to eq 5
      expect(settings.pool_max).to eq 10
      expect(settings.app_name).to eq "app"
    end

    context "when the environment breaks the group relationship" do
      let(:env_store) { {"POOL_MIN" => "99"} }

      it "falls back to the coherent lower source for the whole group" do
        settings = load

        expect(settings.pool_min).to eq 5
        expect(settings.pool_max).to eq 10
      end
    end

    context "when no source can satisfy the group" do
      let(:env_store) { {"POOL_MIN" => "99"} }

      before do
        write(root.join(".env.test"), "POOL_MIN=50\nPOOL_MAX=10\n")
      end

      it "raises an InvalidSettingsError naming the group" do
        expect { load }.to raise_error(
          Dry::System::ProviderSources::Settings::InvalidSettingsError,
          /connection_pool: group/
        )
      end
    end
  end

  describe Dry::System::ProviderSources::Settings::Failure do
    it "renders key, source and failure type without the underlying error message" do
      failure = described_class.new(key: :pool_min, type: :invalid, source: "env")

      expect(failure.to_s).to eq "pool_min (source: env): invalid"
    end

    it "omits the source segment when no source is given" do
      failure = described_class.new(key: :pool_max, type: :missing)

      expect(failure.to_s).to eq "pool_max: missing"
    end

    it "rejects unknown failure types" do
      expect { described_class.new(key: :x, type: :nope) }.to raise_error(ArgumentError)
    end
  end
end