# frozen_string_literal: true

require "dry/system/provider_sources/settings/config"
require "dry/system/provider_sources/settings/loader"

RSpec.describe "Settings source-based resolution and groups" do
  # A loader that reads from a plain hash instead of the live ENV, so each load is fully
  # isolated and cannot observe environment side effects from any other load.
  let(:loader_class) do
    store = env_store

    Class.new(Dry::System::ProviderSources::Settings::Loader) do
      define_method(:initialize) do |root:, env:, roots: nil|
        super(root: root, env: env, roots: roots, store: store)
      end
    end
  end

  let(:env_store) { {} }

  let(:settings_class) do
    Class.new(Dry::System::ProviderSources::Settings::Config) do
      settings_group(:connection_pool, validate: -> values {
        raise "pool_max must be greater than or equal to pool_min" if values[:pool_max] < values[:pool_min]
      }) do
        setting :pool_min, constructor: Types::Coercible::Integer
        setting :pool_max, constructor: Types::Coercible::Integer
      end

      setting :app_name, constructor: Types::String
    end
  end

  let(:root_a) { make_tmp_directory }
  let(:root_b) { make_tmp_directory }

  before do
    write(root_a.join(".env.test"), "POOL_MIN=5\nPOOL_MAX=10\nAPP_NAME=from-root-a\n")
    write(root_b.join(".env.test"), "POOL_MIN=1\nPOOL_MAX=20\nAPP_NAME=from-root-b\n")
  end

  def load_settings(roots:, root: roots.first)
    settings_class.load(root: root, env: :test, roots: roots, loader: loader_class)
  end

  describe "group fallback to a jointly-valid source" do
    context "when the highest precedence value parses but breaks the group validation" do
      let(:env_store) { {"POOL_MIN" => "30", "APP_NAME" => "env-app"} }

      it "falls the whole group back to one coherent lower source" do
        settings = load_settings(roots: [root_a, root_b])

        # min 30 (env) > max 10, so the group rejects the env and root_a's coherent set wins
        expect(settings.pool_min).to eq 5
        expect(settings.pool_max).to eq 10
      end

      it "keeps ordinary per-key overrides in effect" do
        settings = load_settings(roots: [root_a, root_b])

        expect(settings.app_name).to eq "env-app"
      end
    end

    context "when only the lowest source is jointly valid" do
      before do
        write(root_a.join(".env.test"), "POOL_MIN=50\nPOOL_MAX=10\nAPP_NAME=from-root-a\n")
      end

      let(:env_store) { {"POOL_MIN" => "30"} }

      it "falls back past every invalid complete source" do
        settings = load_settings(roots: [root_a, root_b])

        expect(settings.pool_min).to eq 1
        expect(settings.pool_max).to eq 20
      end
    end

    context "when no source provides a jointly-valid group" do
      before do
        write(root_a.join(".env.test"), "POOL_MIN=50\nPOOL_MAX=10\n")
        write(root_b.join(".env.test"), "POOL_MIN=9\nPOOL_MAX=2\n")
      end

      let(:env_store) { {"POOL_MIN" => "30"} }

      it "raises with the group key and the group failure type only" do
        expect { load_settings(roots: [root_a, root_b]) }.to raise_error(
          Dry::System::ProviderSources::Settings::InvalidSettingsError,
          /connection_pool: group/
        )
      end
    end

    context "when a group key is missing from every source" do
      before do
        write(root_a.join(".env.test"), "POOL_MIN=5\nAPP_NAME=from-root-a\n")
      end

      let(:env_store) { {} }

      it "reports the missing key and the group failure" do
        expect { load_settings(roots: [root_a]) }.to raise_error(
          Dry::System::ProviderSources::Settings::InvalidSettingsError
        ) { |error|
          lines = error.message.lines.map(&:strip).reject(&:empty?)
          expect(lines).to include("pool_max: missing")
          expect(lines).to include("connection_pool: group")
        }
      end
    end
  end

  describe "ordinary per-key precedence" do
    let(:env_store) { {"APP_NAME" => "from-env"} }

    it "takes the highest source that carries each independent key" do
      settings = load_settings(roots: [root_a, root_b])

      expect(settings.app_name).to eq "from-env"
    end
  end

  describe "explicit empty values" do
    context "on an ordinary key" do
      let(:env_store) { {"APP_NAME" => ""} }

      it "mask lower sources and surface a blank failure" do
        expect { load_settings(roots: [root_a, root_b]) }.to raise_error(
          Dry::System::ProviderSources::Settings::InvalidSettingsError,
          /app_name \(source: env\): blank/
        )
      end
    end

    context "on a group key" do
      let(:env_store) { {"POOL_MIN" => "", "POOL_MAX" => ""} }

      it "does not pick lower values key-by-key; the empty source simply fails as a group candidate" do
        settings = load_settings(roots: [root_a, root_b])

        expect(settings.pool_min).to eq 5
        expect(settings.pool_max).to eq 10
      end
    end
  end

  describe "diagnostics" do
    let(:invalid_settings_class) do
      Class.new(Dry::System::ProviderSources::Settings::Config) do
        setting :strict_value, constructor: Types::Integer
      end
    end

    let(:env_store) { {"STRICT_VALUE" => "foo"} }

    it "only ever exposes the key, the source and the failure type" do
      expect {
        invalid_settings_class.load(root: root_a, env: :test, roots: [root_a], loader: loader_class)
      }.to raise_error(Dry::System::ProviderSources::Settings::InvalidSettingsError) { |error|
        lines = error.message.lines.map(&:strip).reject(&:empty?).drop(1)
        expect(lines).to contain_exactly("strict_value (source: env): invalid")
        expect(error.message).not_to include("foo")
      }
    end
  end

  describe "load isolation" do
    it "does not let one load's environment leak into a later load against another root" do
      # First load carries an env override that forces the group to fall back.
      env_store.replace("POOL_MIN" => "30")
      first = load_settings(roots: [root_a, root_b])
      expect(first.pool_min).to eq 5
      expect(first.pool_max).to eq 10

      # A fresh store and a different root must resolve from that root's own values only.
      env_store.clear
      second = load_settings(roots: [root_b], root: root_b)
      expect(second.pool_min).to eq 1
      expect(second.pool_max).to eq 20
      expect(second.app_name).to eq "from-root-b"
    end

    it "never mutates the live process environment while parsing .env files" do
      env_store.replace({})
      expect {
        load_settings(roots: [root_a, root_b])
      }.not_to(change { ENV["POOL_MIN"] })
    end
  end
end