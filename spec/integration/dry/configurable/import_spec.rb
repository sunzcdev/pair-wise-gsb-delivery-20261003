# frozen_string_literal: true

RSpec.describe Dry::Configurable::Config, "#import / #export with migration aliases" do
  subject(:config) do
    klass.config
  end

  let(:klass) do
    Class.new do
      extend Dry::Configurable

      setting :service_a do
        setting :limit, default: 0, constructor: ->(value) { Integer(value) }, aliases: :max
      end

      setting :service_b do
        setting :limit, default: 0, constructor: ->(value) { Integer(value) }, aliases: :max
      end
    end
  end

  describe "migrating legacy keys across two sections" do
    it "resolves aliases per section and exports canonical keys only" do
      # One section supplies the same value under the old and the new key ("3" vs 3),
      # the other section supplies the old key only.
      config.import(
        service_a: {max: "3", limit: 3},
        service_b: {max: "10"}
      )

      # Aliased values go through the setting's own constructor, so they share its
      # construction and type checks with canonical values.
      expect(config.service_a.limit).to be(3)
      expect(config.service_b.limit).to be(10)

      # Each section's data view contains the current key only.
      expect(config.service_a.values.keys).to eql(%i[limit])
      expect(config.service_b.values.keys).to eql(%i[limit])

      expect(config.export).to eql(
        service_a: {limit: 3},
        service_b: {limit: 10}
      )
      expect(config.export).to eql(config.to_h)
    end

    it "rejects a second, partially-valid import whose later section conflicts and keeps the first import intact" do
      config.import(
        service_a: {max: "3", limit: 3},
        service_b: {max: "10"}
      )

      # The first half of this input converts fine; the second half carries genuinely
      # conflicting values for the same setting.
      expect {
        config.import(
          service_a: {max: "3", limit: 3},
          service_b: {max: "7", limit: 9}
        )
      }.to raise_error(
        Dry::Configurable::AmbiguousValueError,
        /:service_b\.limit.*:max.*:limit/
      )

      # The whole config tree produced by the first import is still readable as-is.
      expect(config.service_a.limit).to be(3)
      expect(config.service_b.limit).to be(10)
      expect(config.export).to eql(
        service_a: {limit: 3},
        service_b: {limit: 10}
      )
    end
  end

  describe "alias resolution" do
    it "works through the regular config accessors and stores under the canonical key" do
      config.service_a[:max] = "4"

      expect(config.service_a.limit).to be(4)
      expect(config.service_a).to respond_to(:max)
      expect(config.service_a.max).to be(4)

      config.service_a.max = "6"
      expect(config.service_a.limit).to be(6)
      expect(config.service_a.values.keys).to eql(%i[limit])
    end

    it "is scoped to the owning section, so the same alias works in both sections" do
      config.import(service_a: {max: "4"}, service_b: {max: "8"})

      expect(config.service_a.limit).to be(4)
      expect(config.service_b.limit).to be(8)
    end

    it "accepts string and symbol keys and string, symbol or array alias declarations" do
      klass = Class.new do
        extend Dry::Configurable

        setting :db do
          setting :pool, default: 1, aliases: "pool_size"
          setting :timeout, default: 0, aliases: %i[time_out open_timeout]
        end
      end

      klass.config.import(
        "db" => {"pool_size" => "5", :time_out => "2"}
      )

      expect(klass.config.db.pool).to eql("5")
      expect(klass.config.db.timeout).to eql("2")
      expect(klass.config.export).to eql(db: {pool: "5", timeout: "2"})
    end

    it "raises naming the section when a key is unknown in that section" do
      expect {
        config.import(service_a: {unknown: 1})
      }.to raise_error(ArgumentError, /\+unknown\+ is not a setting name or a declared alias/)
    end

    it "rejects an alias colliding with a setting name in the same section at declaration time" do
      expect {
        Class.new do
          extend Dry::Configurable

          setting :db do
            setting :pool
            setting :timeout, aliases: :pool
          end
        end
      }.to raise_error(ArgumentError, /:pool.*conflicts with an existing setting name/)
    end

    it "rejects an alias declared by two settings in the same section at declaration time" do
      expect {
        Class.new do
          extend Dry::Configurable

          setting :db do
            setting :pool, aliases: :legacy_pool
            setting :timeout, aliases: :legacy_pool
          end
        end
      }.to raise_error(ArgumentError, /:legacy_pool.*already declared by setting :pool/)
    end

    it "rejects a setting aliasing itself" do
      expect {
        Class.new do
          extend Dry::Configurable

          setting :db do
            setting :pool, aliases: :pool
          end
        end
      }.to raise_error(ArgumentError, /cannot declare itself as an alias/)
    end
  end

  describe "construction and type checking" do
    it "runs the same constructor for aliased and canonical values" do
      config.import(service_a: {max: " 12 "})

      expect(config.service_a.limit).to be(12)
      expect(config.service_a.limit).to be_an(Integer)
    end

    it "treats old and new keys as consistent when the constructed values compare equally" do
      expect {
        config.import(service_a: {max: "3", limit: 3})
      }.not_to raise_error

      expect(config.service_a.limit).to be(3)
    end

    it "treats old and new keys as ambiguous when constructed values differ" do
      expect {
        config.import(service_a: {max: "3", limit: 4})
      }.to raise_error(Dry::Configurable::AmbiguousValueError, /:service_a\.limit/)
    end

    it "detects ambiguity between two aliases for the same setting" do
      multi_klass = Class.new do
        extend Dry::Configurable

        setting :db do
          setting :pool, default: 1, constructor: ->(value) { Integer(value) },
                         aliases: %i[pool_size max_pool]
        end
      end

      expect {
        multi_klass.config.import(db: {pool_size: "3", max_pool: "4"})
      }.to raise_error(
        Dry::Configurable::AmbiguousValueError,
        /:db\.pool.*:pool_size.*:max_pool/
      )

      expect(multi_klass.config.db.pool).to be(1)
    end

    it "does not mutate the config when a constructor fails deep in the import" do
      # service_a converts first (and would succeed), but service_b's constructor fails;
      # the partial work must not leak into the live config.
      expect {
        config.import(
          service_a: {limit: 6},
          service_b: {max: "not-a-number"}
        )
      }.to raise_error(ArgumentError, /invalid value for Integer/)

      expect(config.service_a.limit).to be(0)
      expect(config.service_b.limit).to be(0)
      expect(config.export).to eql(
        service_a: {limit: 0},
        service_b: {limit: 0}
      )
    end

    it "constructs each imported value once rather than constructing again on the write path" do
      calls = 0
      counting_klass = Class.new do
        extend Dry::Configurable

        setting :db do
          setting :pool, default: 1, constructor: ->(value) { calls += 1; Integer(value) }
        end
      end

      counting_klass.config.import(db: {pool: "7"})

      # One constructor call for the imported value (reading it back reuses the stored value).
      expect(calls).to be(1)
      expect(counting_klass.config.db.pool).to eql(7)
      expect(calls).to be(1)
    end
  end

  describe "atomicity" do
    it "keeps previously imported values readable after a failed re-import" do
      config.import(service_a: {max: "3"}, service_b: {max: "10"})

      snapshot = config.export

      expect { config.import(service_a: {max: "nope"}) }.to raise_error(ArgumentError)

      expect(config.export).to eql(snapshot)
    end

    it "rolls back ambiguity in a nested section without touching earlier sections" do
      config.import(service_a: {limit: 2}, service_b: {limit: 2})

      expect {
        config.import(service_a: {limit: 5}, service_b: {max: "7", limit: 9})
      }.to raise_error(Dry::Configurable::AmbiguousValueError, /:service_b\.limit/)

      expect(config.export).to eql(
        service_a: {limit: 2},
        service_b: {limit: 2}
      )
    end

    it "restores pre-read mutable values when a later section fails" do
      mutable_klass = Class.new do
        extend Dry::Configurable

        setting :tags, default: [], cloneable: true

        setting :db do
          setting :ports, default: Set[1], cloneable: true
          setting :limit, default: 0, constructor: ->(value) { Integer(value) }, aliases: :max
        end
      end

      mutable_klass.config.tags << "kept"
      mutable_klass.config.db.ports << 5432

      expect {
        mutable_klass.config.import(tags: ["rejected"], db: {max: "boom"})
      }.to raise_error(ArgumentError, /invalid value for Integer/)

      expect(mutable_klass.config.tags).to eql(["kept"])
      expect(mutable_klass.config.db.ports).to eql(Set[1, 5432])
      expect(mutable_klass.config.db.limit).to be(0)
    end

    it "raises when importing into a finalized config" do
      klass.finalize!

      expect { config.import(service_a: {limit: 1}) }
        .to raise_error(Dry::Configurable::FrozenConfigError)
    end

    it "supports objects that respond to #to_hash" do
      payload = Class.new do
        def to_hash
          {service_a: {max: "9"}}
        end
      end.new

      config.import(payload)

      expect(config.service_a.limit).to be(9)
    end
  end

  describe "nested section ambiguity" do
    it "requires canonical and aliased hashes for a nested section to be equal" do
      klass = Class.new do
        extend Dry::Configurable

        setting :db, aliases: :database do
          setting :pool
        end
      end

      expect {
        klass.config.import(db: {pool: 5}, database: {pool: 6})
      }.to raise_error(Dry::Configurable::AmbiguousValueError, /:db/)

      expect {
        klass.config.import(db: {pool: 5}, database: {pool: 5})
      }.not_to raise_error

      expect(klass.config.export).to eql(db: {pool: 5})
    end

    it "raises when a nested section receives a non-hash value" do
      expect {
        config.import(service_a: "not-a-hash")
      }.to raise_error(ArgumentError, /not a valid setting value/)
    end
  end

  describe "final data view" do
    it "exposes canonical members only through to_data after finalizing" do
      config.import(service_a: {max: "3"}, service_b: {max: "10"})
      klass.finalize!

      data = config.to_data

      expect(data.members).to eql(%i[service_a service_b])
      expect(data.service_a.members).to eql(%i[limit])
      expect(data.service_a.limit).to be(3)
      expect(data.service_b.limit).to be(10)
    end
  end
end
