# frozen_string_literal: true

RSpec.describe Dry::Configurable::Config do
  subject(:config) { klass.config }

  let(:klass) do
    Class.new do
      extend Dry::Configurable
    end
  end

  describe "#source" do
    context "with a plain default" do
      before { klass.setting :timeout, default: 30 }

      it "reports a default provenance" do
        source = config.source(:timeout)

        expect(source).to be_a(Dry::Configurable::Source)
        expect(source.origin).to eq(:default)
        expect(source).to be_default
        expect(source).not_to be_inherited
        expect(source).not_to be_local
        expect(source).not_to be_assigned
        expect(source).not_to be_mutated
      end

      it "stays default after a read" do
        config.timeout

        source = config.source(:timeout)
        expect(source).to be_default
        expect(source).not_to be_assigned
      end
    end

    context "with an explicit assignment" do
      before do
        klass.setting :timeout, default: 30
        klass.setting :tags, default: []
      end

      it "is local even when the assigned value equals the default" do
        config.timeout = 30

        source = config.source(:timeout)
        expect(source).to be_local
        expect(source).to be_assigned
        expect(source).not_to be_mutated
      end

      it "is local after assigning a mutable default back to an equal value" do
        config.tags = []

        source = config.source(:tags)
        expect(source).to be_local
        expect(source).to be_assigned
      end

      it "is local when assigned through #update" do
        config.update(timeout: 30)

        expect(config.source(:timeout)).to be_local
      end
    end

    context "with a mutable default" do
      before { klass.setting :tags, default: %w[a b] }

      it "is default and unassigned after a read only" do
        config.tags

        source = config.source(:tags)
        expect(source).to be_default
        expect(source).not_to be_assigned
        expect(source).not_to be_mutated
      end

      it "is still unassigned but mutated after an in-place change" do
        config.tags << "c"

        source = config.source(:tags)
        expect(source).to be_default
        expect(source).not_to be_assigned
        expect(source).to be_mutated
      end

      it "is not mutated once the contents equal a fresh default" do
        config.tags << "c"
        config.tags.delete("c")

        expect(config.source(:tags)).not_to be_mutated
        expect(config.source(:tags)).to be_default
      end
    end

    context "comparing with #configured?" do
      before { klass.setting :tags, default: [] }

      it "agrees for an untouched default" do
        expect(config.configured?(:tags)).to be false
        source = config.source(:tags)
        expect(source).not_to be_assigned
        expect(source).not_to be_mutated
      end

      it "diverges when a mutable default was changed in place" do
        config.tags << "x"

        # configured? answers "does the value still look like a default?"
        expect(config.configured?(:tags)).to be true

        # source answers "was it assigned / where did it come from?"
        source = config.source(:tags)
        expect(source).to be_default
        expect(source).not_to be_assigned
        expect(source).to be_mutated
      end

      it "diverges when a mutable value is explicitly assigned back to the default" do
        config.tags = []

        expect(config.configured?(:tags)).to be false

        source = config.source(:tags)
        expect(source).to be_local
        expect(source).to be_assigned
      end
    end
  end

  describe "inheritance" do
    before do
      klass.setting :a, default: 1
      klass.setting :b, default: 2
      klass.setting :tags, default: []
    end

    it "marks an ancestor-assigned value as inherited" do
      klass.config.a = 10
      subclass = Class.new(klass)

      source = subclass.config.source(:a)
      expect(source).to be_inherited
      expect(source).to be_assigned
    end

    it "leaves an untouched value as default" do
      klass.config.a = 10
      subclass = Class.new(klass)

      expect(subclass.config.source(:b)).to be_default
    end

    it "does not treat a superclass read of a mutable default as inherited" do
      klass.config.tags
      subclass = Class.new(klass)

      source = subclass.config.source(:tags)
      expect(source).to be_default
      expect(source).not_to be_assigned
    end

    it "inherits an in-place mutation of a mutable default" do
      klass.config.tags << "x"
      subclass = Class.new(klass)

      source = subclass.config.source(:tags)
      expect(source).to be_default
      expect(source).not_to be_assigned
      expect(source).to be_mutated
    end

    it "promotes inherited to local once the subclass assigns it" do
      klass.config.a = 10
      subclass = Class.new(klass)
      subclass.config.a = 20

      expect(subclass.config.source(:a)).to be_local
    end

    it "leaves the parent and sibling provenance unchanged" do
      klass.config.a = 10
      subclass = Class.new(klass)
      subclass.config.a = 20
      sibling = Class.new(klass)

      expect(klass.config.source(:a)).to be_local
      expect(sibling.config.source(:a)).to be_inherited
      expect(subclass.config.source(:a)).to be_local
    end
  end

  describe "nested settings" do
    before do
      klass.setting :db do
        setting :host, default: "localhost"
        setting :port, default: 5432
        setting :tags, default: []
      end
    end

    it "aggregates a fully default group" do
      source = config.source(:db)

      expect(source).to be_default
      expect(source).not_to be_assigned
      expect(source).not_to be_mutated
    end

    it "resolves a nested leaf by path" do
      expect(config.source(:db, :host)).to be_default

      config.db.host = "example.com"

      leaf = config.source(:db, :host)
      expect(leaf).to be_local
      expect(leaf).to be_assigned
    end

    it "aggregates the most specific origin in the group" do
      config.db.host = "example.com"

      source = config.source(:db)
      expect(source).to be_local
      expect(source).to be_assigned
      expect(source).not_to be_mutated

      expect(config.source(:db, :port)).to be_default
    end

    it "aggregates a mutation of a mutable nested default" do
      config.db.tags << "primary"

      expect(config.source(:db)).to be_mutated
      expect(config.source(:db)).to be_default
      expect(config.source(:db, :tags)).to be_mutated
    end

    it "does not mutate provenance when merely reading a path" do
      config.source(:db, :host)

      expect(config.source(:db, :host)).to be_default
      expect(config.configured?(:db)).to be false
    end

    it "propagates nested assignments through inheritance" do
      config.db.host = "example.com"
      subclass = Class.new(klass)

      expect(subclass.config.source(:db, :host)).to be_inherited
      expect(subclass.config.source(:db, :port)).to be_default
      expect(subclass.config.source(:db)).to be_inherited
    end

    it "promotes a nested inherited value to local" do
      config.db.host = "example.com"
      subclass = Class.new(klass)
      subclass.config.db.host = "other"

      expect(subclass.config.source(:db, :host)).to be_local
      expect(klass.config.source(:db, :host)).to be_local
    end
  end

  describe "#dup" do
    before do
      klass.setting :a, default: 1
      klass.setting :b, default: 2
      klass.setting :tags, default: []
    end

    it "preserves local provenance on a plain copy" do
      config.a = 10
      copy = config.dup

      expect(copy.source(:a)).to be_local
      expect(copy.source(:b)).to be_default
    end

    it "preserves mutated-default provenance on a copy" do
      config.tags << "x"

      expect(config.dup.source(:tags)).to be_mutated
    end

    it "keeps the source independent after a copy is modified" do
      config.a = 10
      copy = config.dup
      copy.a = 20

      expect(copy.source(:a)).to be_local
      expect(config.source(:a)).to be_local
    end
  end

  describe "#pristine" do
    before do
      klass.setting :a, default: 1
      klass.setting :tags, default: []
    end

    it "resets provenance back to defaults" do
      config.a = 10
      config.tags << "x"

      fresh = config.pristine

      expect(fresh.source(:a)).to be_default
      expect(fresh.source(:a)).not_to be_assigned
      expect(fresh.source(:tags)).to be_default
      expect(fresh.source(:tags)).not_to be_mutated
      expect(fresh.a).to eq(1)
      expect(fresh.tags).to eq([])
    end

    it "does not affect the original config's provenance" do
      config.a = 10
      config.pristine

      expect(config.source(:a)).to be_local
    end
  end

  describe "#finalize! and #to_data" do
    before do
      klass.setting :a, default: 1
      klass.setting :tags, default: []
      klass.setting :db do
        setting :host, default: "localhost"
      end
    end

    it "keeps provenance after finalization" do
      config.a = 10
      config.finalize!

      expect(config.source(:a)).to be_local
      expect(config.source(:tags)).to be_default
    end

    it "keeps provenance queryable after to_data" do
      config.a = 10
      config.db.host = "example.com"
      config.tags << "x"
      config.finalize!

      data = config.to_data

      expect(data.a).to eq(10)
      expect(config.source(:a)).to be_local
      expect(config.source(:db, :host)).to be_local
      expect(config.source(:tags)).to be_mutated
    end
  end

  describe "invalid paths" do
    before { klass.setting :db }

    it "raises for an unknown setting" do
      expect { config.source(:nope) }.to raise_error(ArgumentError, /nope/)
    end

    it "raises when descending through a leaf setting" do
      expect { config.source(:db, :deep) }.to raise_error(ArgumentError, /db/)
    end
  end

  describe Dry::Configurable::Source do
    subject(:source) { described_class.new(origin: :inherited, assigned: true, mutated: false) }

    it "exposes its data as a hash" do
      expect(source.to_h).to eq(origin: :inherited, assigned: true, mutated: false)
    end

    it "compares by value" do
      expect(source).to eq(described_class.new(origin: :inherited, assigned: true, mutated: false))
      expect(source).not_to eq(described_class.default)
    end

    it "provides predicate constructors" do
      expect(described_class.default).to be_default
      expect(described_class.local).to be_local
    end

    it "has an informative inspect" do
      expect(source.inspect).to include(":inherited")
    end
  end
end
