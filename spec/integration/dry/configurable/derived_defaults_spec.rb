# frozen_string_literal: true

RSpec.describe Dry::Configurable, "derived defaults" do
  # A thread pool whose queue capacity and worker prefix are derived from
  # other settings. Each derived default records its evaluations so specs can
  # verify that only affected derived values are re-evaluated on a change.
  def build_pool_class(queue_evals:, prefix_evals:)
    Class.new do
      extend Dry::Configurable

      setting :max_threads, default: 4, constructor: -> v { Integer(v) }
      setting :region, default: "us-east"

      setting :queue_capacity,
              default: ->(max_threads:) {
                queue_evals << max_threads
                (max_threads * 2).to_s
              },
              depends_on: %i[max_threads],
              constructor: -> v { Integer(v) }

      setting :worker_prefix,
              default: ->(region:) {
                prefix_evals << region
                "worker-#{region}"
              },
              depends_on: %i[region]
    end
  end

  let(:queue_evals) { [] }
  let(:prefix_evals) { [] }
  let(:klass) { build_pool_class(queue_evals: queue_evals, prefix_evals: prefix_evals) }
  let(:config) { klass.config }

  it "evaluates derived defaults from their dependencies on read, through the constructor" do
    expect(config.queue_capacity).to eq 8 # "8" validated/coerced by the constructor
    expect(config.worker_prefix).to eq "worker-us-east"

    expect(queue_evals).to eq [4]
    expect(prefix_evals).to eq ["us-east"]
  end

  it "caches derived values between reads" do
    2.times { config.queue_capacity }

    expect(queue_evals).to eq [4]
  end

  it "re-evaluates only the derived settings affected by a local change" do
    config.queue_capacity
    config.worker_prefix

    config.max_threads = 10

    expect(config.queue_capacity).to eq 20
    expect(queue_evals).to eq [4, 10]

    # Unrelated derived value is untouched by the change
    expect(config.worker_prefix).to eq "worker-us-east"
    expect(prefix_evals).to eq ["us-east"]
  end

  it "re-evaluates transitively dependent derived settings" do
    chained = Class.new do
      extend Dry::Configurable

      setting :max_threads, default: 4
      setting :queue_capacity,
              default: ->(max_threads:) { max_threads * 2 },
              depends_on: %i[max_threads]
      setting :max_in_flight,
              default: ->(queue_capacity:) { queue_capacity + 1 },
              depends_on: %i[queue_capacity]
    end

    expect(chained.config.max_in_flight).to eq 9

    chained.config.max_threads = 10

    expect(chained.config.queue_capacity).to eq 20
    expect(chained.config.max_in_flight).to eq 21
  end

  it "keeps explicitly assigned values when dependencies change" do
    config.queue_capacity
    config.queue_capacity = 100

    config.max_threads = 16

    expect(config.queue_capacity).to eq 100
    expect(queue_evals).to eq [4]
    expect(config.configured?(:queue_capacity)).to be true
  end

  it "raises a CyclicDependencyError naming the traversed setting path" do
    cyclic = Class.new do
      extend Dry::Configurable

      setting :a, default: ->(c:) { c }, depends_on: %i[c]
      setting :b, default: ->(a:) { a }, depends_on: %i[a]
      setting :c, default: ->(b:) { b }, depends_on: %i[b]
    end

    expect { cyclic.config.a }.to raise_error(
      Dry::Configurable::CyclicDependencyError,
      "Cyclic dependency in derived setting defaults: a -> c -> b -> a"
    )
  end

  it "requires a callable default when depends_on is given" do
    expect {
      Class.new do
        extend Dry::Configurable

        setting :max_threads, default: 4
        setting :queue_capacity, default: 8, depends_on: %i[max_threads]
      end
    }.to raise_error(ArgumentError, /Default for \+queue_capacity\+ must be callable/)
  end
end
