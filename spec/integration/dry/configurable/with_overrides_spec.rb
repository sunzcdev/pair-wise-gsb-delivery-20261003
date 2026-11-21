# frozen_string_literal: true

RSpec.describe Dry::Configurable::Config, "#with_overrides" do
  subject(:config) { klass.config }

  let(:klass) do
    Class.new do
      extend Dry::Configurable

      setting :connect_timeout, default: 2
      setting :endpoint, default: "https://service.internal"
    end
  end

  it "returns the overridden value inside the block and the configured value outside" do
    config.connect_timeout = 10

    config.with_overrides(connect_timeout: 60) do
      expect(config.connect_timeout).to eq(60)
    end

    expect(config.connect_timeout).to eq(10)
  end

  it "leaves other settings untouched" do
    config.with_overrides(connect_timeout: 60) do
      expect(config.endpoint).to eq("https://service.internal")
    end
  end

  it "does not write the overridden value into the config" do
    config.with_overrides(connect_timeout: 60) { nil }

    expect(config.configured?(:connect_timeout)).to be false
    expect(config.connect_timeout).to eq(2)
  end

  context "with two interleaved maintenance tasks (nested blocks)" do
    it "lets each nested block read its own value and restores the enclosing layer" do
      seen = []

      config.with_overrides(connect_timeout: 30) do
        seen << config.connect_timeout

        config.with_overrides(connect_timeout: 120) do
          seen << config.connect_timeout
        end

        seen << config.connect_timeout
      end

      seen << config.connect_timeout

      expect(seen).to eq([30, 120, 30, 2])
    end
  end

  context "with two interleaved maintenance tasks (threads)" do
    it "gives each task its own override layer" do
      results = Queue.new

      task_a = Thread.new do
        config.with_overrides(connect_timeout: 30) do
          sleep 0.05 # let task_b install its own layer
          results << [:a, config.connect_timeout]
        end
      end

      task_b = Thread.new do
        config.with_overrides(connect_timeout: 120) do
          sleep 0.05
          results << [:b, config.connect_timeout]
        end
      end

      [task_a, task_b].each(&:join)

      expect([results.pop, results.pop]).to contain_exactly([:a, 30], [:b, 120])
      expect(config.connect_timeout).to eq(2)
    end
  end

  context "when an exception is raised inside a nested block" do
    it "restores the enclosing layer and then the configured value" do
      config.connect_timeout = 10

      config.with_overrides(connect_timeout: 30) do
        begin
          config.with_overrides(connect_timeout: 120) do
            raise "maintenance failed"
          end
        rescue RuntimeError
          # swallow: the maintenance task failed, the layer must still be popped
        end

        expect(config.connect_timeout).to eq(30)
      end

      expect(config.connect_timeout).to eq(10)
    end

    it "restores the configured value when the exception escapes the outer block" do
      expect {
        config.with_overrides(connect_timeout: 30) do
          raise "maintenance failed"
        end
      }.to raise_error("maintenance failed")

      expect(config.connect_timeout).to eq(2)
    end
  end

  context "with a finalized config" do
    it "raises FrozenConfigError and does not change any value" do
      config.finalize!

      expect {
        config.with_overrides(connect_timeout: 60) { nil }
      }.to raise_error(Dry::Configurable::FrozenConfigError)

      expect(config.connect_timeout).to eq(2)
    end
  end

  describe "the class-level entry point" do
    it "delegates to the config" do
      klass.with_overrides(connect_timeout: 60) do
        expect(klass.config.connect_timeout).to eq(60)
      end

      expect(klass.config.connect_timeout).to eq(2)
    end
  end
end
