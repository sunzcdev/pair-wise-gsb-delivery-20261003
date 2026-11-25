# frozen_string_literal: true

require "dry/system/provider_sources"

RSpec.describe "Settings groups through a full container" do
  let(:root_a) { make_tmp_directory }
  let(:root_b) { make_tmp_directory }

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

  let(:container) do
    root_a = self.root_a

    Class.new(Dry::System::Container) do
      setting :env

      configure do |config|
        config.root = root_a
        config.env = :test
      end

      register_provider(:settings, from: :dry_system) do
        settings do
          settings_group(:connection_pool, validate: -> values {
            raise "pool_max must be greater than or equal to pool_min" if values[:pool_max] < values[:pool_min]
          }) do
            setting :pool_min, constructor: Types::Coercible::Integer
            setting :pool_max, constructor: Types::Coercible::Integer
          end

          setting :app_name, constructor: Types::String
        end
      end
    end
  end

  before do
    write(root_a.join(".env.test"), "POOL_MIN=5\nPOOL_MAX=10\nAPP_NAME=from-root-a\n")
    write(root_b.join(".env.test"), "POOL_MIN=1\nPOOL_MAX=20\nAPP_NAME=from-root-b\n")
  end

  after do
    ENV.delete("POOL_MIN")
    ENV.delete("POOL_MAX")
    ENV.delete("APP_NAME")
  end

  def settings_for(roots:, root: roots.first)
    settings_class.load(root: root, env: :test, roots: roots)
  end

  describe "with real process environment" do
    before do
      # A parseable POOL_MIN that makes min (30) exceed max (10), and a valid unrelated override.
      ENV["POOL_MIN"] = "30"
      ENV["APP_NAME"] = "env-app"
    end

    it "falls the connection pool group back while keeping the unrelated override" do
      settings = settings_for(roots: [root_a, root_b])

      expect(settings.pool_min).to eq 5
      expect(settings.pool_max).to eq 10
      expect(settings.app_name).to eq "env-app"
    end

    it "does not leak dotenv values into the live environment" do
      settings_for(roots: [root_a, root_b])

      expect(ENV["POOL_MAX"]).to be_nil
      expect(ENV["POOL_MIN"]).to eq "30"
    end

    it "resolves a later load against another root from that root's own values" do
      settings_for(roots: [root_a, root_b])

      second = settings_for(roots: [root_b], root: root_b)

      expect(second.pool_min).to eq 1
      expect(second.pool_max).to eq 20
      # APP_NAME still comes from the live environment, independently of either root
      expect(second.app_name).to eq "env-app"
    end
  end

  describe "through the registered provider" do
    it "exposes the settings via the container using the single configured root" do
      ENV["APP_NAME"] = "env-app"

      expect(container[:settings].app_name).to eq "env-app"
      expect(container[:settings].pool_min).to eq 5
      expect(container[:settings].pool_max).to eq 10
    end
  end
end