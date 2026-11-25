require "flipper/adapters/federated"
require "flipper/adapters/memory"
require "flipper/adapters/operation_logger"

RSpec.describe Flipper::Adapters::Federated do
  # The backend the managed flags are being migrated onto.
  let(:local_adapter) { Flipper::Adapters::Memory.new }

  # The old backend that still owns the flags left behind during the migration.
  let(:legacy_adapter) { Flipper::Adapters::Memory.new }
  let(:legacy) { Flipper.new(legacy_adapter, memoize: false) }

  subject do
    described_class.new(
      local_adapter,
      external: { legacy: legacy_adapter },
      dependencies: {
        # :checkout_v2 is explicitly allowed to read :search from :legacy.
        checkout_v2: { search: :legacy }
      }
    )
  end

  # The migrating Flipper instance. FeatureEnabled resolves references through
  # the global Flipper instance, so point it at the federated adapter too.
  let(:flipper) do
    Flipper.new(subject, memoize: false).tap { |instance| Flipper.instance = instance }
  end

  after do
    Thread.current[Flipper::Expressions::FeatureEnabled::ORIGIN_STACK_KEY] = nil
    Thread.current[:flipper_evaluating_features] = nil
  end

  describe "declared external reference" do
    before do
      # The referenced flag lives only in the old backend.
      legacy.enable(:search)
      flipper.enable_expression(:checkout_v2, Flipper.feature_enabled(:search))
    end

    it "follows the reference to the declared external source" do
      expect(flipper.enabled?(:checkout_v2)).to be(true)
      expect(legacy.enabled?(:search)).to be(true)
    end

    it "tracks changes made at the external source after switching adapters" do
      expect(flipper.enabled?(:checkout_v2)).to be(true)

      legacy.disable(:search)
      expect(flipper.enabled?(:checkout_v2)).to be(false)

      legacy.enable(:search)
      expect(flipper.enabled?(:checkout_v2)).to be(true)
    end

    it "does not store the external flag in the local adapter" do
      flipper.enabled?(:checkout_v2)
      expect(local_adapter.features).not_to include("search")
    end
  end

  describe "undeclared external reference with the same name" do
    before do
      # Another managed flag references :search too, but has no declaration.
      flipper.enable_expression(:checkout_v3, Flipper.feature_enabled(:search))
    end

    it "reports a missing dependency instead of querying any adapter" do
      expect do
        flipper.enabled?(:checkout_v3)
      end.to raise_error(Flipper::MissingDependency) { |error|
        expect(error.feature_name).to eq("checkout_v3")
        expect(error.dependency_name).to eq("search")
      }
    end

    it "never reads the external source for the undeclared edge" do
      legacy.enable(:search)
      instrumented = Flipper::Adapters::OperationLogger.new(legacy_adapter)

      adapter = described_class.new(
        local_adapter,
        external: { legacy: instrumented },
        dependencies: { checkout_v2: { search: :legacy } }
      )
      guarded = Flipper.new(adapter, memoize: false)
      Flipper.instance = guarded
      guarded.enable_expression(:checkout_v3, Flipper.feature_enabled(:search))

      expect { guarded.enabled?(:checkout_v3) rescue nil }.
        not_to change { instrumented.count(:get) }.from(0)
    end

    it "reports a missing dependency inside a composed expression too" do
      flipper.enable_expression(
        :checkout_v3,
        Flipper.any(
          Flipper.feature_enabled(:search),
          Flipper.property(:plan).eq("basic")
        )
      )

      expect { flipper.enabled?(:checkout_v3) }.
        to raise_error(Flipper::MissingDependency) { |error|
          expect(error.feature_name).to eq("checkout_v3")
          expect(error.dependency_name).to eq("search")
        }
    end

    it "leaves no origin on the thread after raising" do
      begin
        flipper.enabled?(:checkout_v3)
      rescue Flipper::MissingDependency
        nil
      end

      expect(Thread.current[Flipper::Expressions::FeatureEnabled::ORIGIN_STACK_KEY]).to be_nil
    end
  end

  describe "side by side" do
    it "makes the declared flag follow the source while the undeclared one reports" do
      legacy.enable(:search)
      flipper.enable_expression(:checkout_v2, Flipper.feature_enabled(:search))
      flipper.enable_expression(:checkout_v3, Flipper.feature_enabled(:search))

      # Declared edge: follows the actual external source value.
      expect(flipper.enabled?(:checkout_v2)).to be(true)

      # Same referenced name, no declaration: reported as a missing dependency.
      expect { flipper.enabled?(:checkout_v3) }.
        to raise_error(Flipper::MissingDependency)

      # Flip the source and the declared edge follows it; the undeclared one
      # still reports rather than silently resolving.
      legacy.disable(:search)
      expect(flipper.enabled?(:checkout_v2)).to be(false)
      expect { flipper.enabled?(:checkout_v3) }.
        to raise_error(Flipper::MissingDependency)
    end
  end

  describe "ordinary local rules" do
    it "keeps evaluating a managed flag's own gates locally" do
      flipper.enable(:checkout_v2)
      expect(flipper.enabled?(:checkout_v2)).to be(true)

      flipper.disable(:checkout_v2)
      expect(flipper.enabled?(:checkout_v2)).to be(false)
      expect(local_adapter.features).to include("checkout_v2")
    end

    it "evaluates local actor rules without consulting an external source" do
      actor = Flipper::Actor.new("user-1")
      flipper.enable_actor(:checkout_v2, actor)

      expect(flipper.enabled?(:checkout_v2, actor)).to be(true)
      expect(flipper.enabled?(:checkout_v2, Flipper::Actor.new("user-2"))).to be(false)
    end

    it "supports feature_disabled through the same declared edge" do
      legacy.disable(:search)
      flipper.enable_expression(:checkout_v2, Flipper.feature_disabled(:search))

      expect(flipper.enabled?(:checkout_v2)).to be(true)

      legacy.enable(:search)
      expect(flipper.enabled?(:checkout_v2)).to be(false)
    end
  end

  describe "multiple external sources" do
    let(:other_adapter) { Flipper::Adapters::Memory.new }
    let(:other) { Flipper.new(other_adapter, memoize: false) }

    subject do
      described_class.new(
        local_adapter,
        external: { legacy: legacy_adapter, other: other_adapter },
        dependencies: {
          checkout_v2: { search: :legacy },
          billing_v2: { invoices: :other }
        }
      )
    end

    it "routes each edge to the adapter named in its declaration" do
      other.enable(:invoices)
      flipper.enable_expression(:billing_v2, Flipper.feature_enabled(:invoices))

      expect(flipper.enabled?(:billing_v2)).to be(true)
      expect(legacy_adapter.features).not_to include("invoices")
      expect(local_adapter.features).not_to include("invoices")

      other.disable(:invoices)
      expect(flipper.enabled?(:billing_v2)).to be(false)
    end
  end

  describe "#initialize" do
    it "rejects a declaration naming an unknown external source" do
      expect do
        described_class.new(
          local_adapter,
          external: { legacy: legacy_adapter },
          dependencies: { checkout_v2: { search: :nowhere } }
        )
      end.to raise_error(ArgumentError, /Unknown external source :nowhere/)
    end
  end

  describe "#adapter_stack" do
    it "describes the local and external adapters" do
      expect(subject.adapter_stack).to eq(
        "federated(local: memory, external: {legacy: memory})"
      )
    end
  end

  describe "circular references" do
    it "breaks the cycle and cleans up the origin stack" do
      adapter = described_class.new(
        local_adapter,
        external: { legacy: legacy_adapter },
        dependencies: {
          alpha: { beta: :legacy },
          beta: { alpha: :legacy }
        }
      )
      circular = Flipper.new(adapter, memoize: false)
      Flipper.instance = circular

      circular.enable_expression(:alpha, Flipper.feature_enabled(:beta))
      circular.enable_expression(:beta, Flipper.feature_enabled(:alpha))

      expect(circular.enabled?(:alpha)).to be(false)
      expect(Thread.current[Flipper::Expressions::FeatureEnabled::ORIGIN_STACK_KEY]).to be_nil
    end
  end

  describe "with memoization enabled" do
    it "still routes nested references and reports undeclared ones" do
      legacy.enable(:search)
      memoized = Flipper.new(subject)
      Flipper.instance = memoized
      memoized.enable_expression(:checkout_v2, Flipper.feature_enabled(:search))
      memoized.enable_expression(:checkout_v3, Flipper.feature_enabled(:search))

      memoized.memoize = true
      expect(memoized.enabled?(:checkout_v2)).to be(true)
      expect(memoized.enabled?(:checkout_v2)).to be(true)
      expect { memoized.enabled?(:checkout_v3) }.
        to raise_error(Flipper::MissingDependency)
      memoized.memoize = false

      legacy.disable(:search)
      memoized.memoize = true
      expect(memoized.enabled?(:checkout_v2)).to be(false)
      memoized.memoize = false
    end
  end
end
