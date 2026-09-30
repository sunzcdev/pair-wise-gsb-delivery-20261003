# frozen_string_literal: true

RSpec.describe "Explicit dependency declarations" do
  let(:container) { Dry::Container.new }

  describe "injecting declared dependencies" do
    it "passes resolved dependency values to the factory as keyword arguments" do
      container.register(:db, -> { "database" })
      container.register(:repo, depends_on: [:db]) { |db:| "repo using #{db}" }

      expect(container.resolve(:repo)).to eql("repo using database")
    end

    it "resolves the whole chain before calling the target factory" do
      started = []

      container.register(:a, -> { started << :a; "a" })
      container.register(:b, depends_on: [:a]) do |a:|
        started << :b
        "b/#{a}"
      end
      container.register(:c, depends_on: [:b]) do |b:|
        started << :c
        "c/#{b}"
      end

      expect(container.resolve(:c)).to eql("c/b/a")
      expect(started).to eql(%i[a b c])
    end

    it "supports custom argument names" do
      container.register("adapters.sql", -> { :sql })
      container.register(:repo, depends_on: {db: "adapters.sql"}) { |db:| db }

      expect(container.resolve(:repo)).to be(:sql)
    end

    it "keeps dependency declarations when an item is decorated" do
      container.register(:dep, -> { :dep_value })
      container.register(:service, depends_on: [:dep]) { |dep:| [:raw, dep] }
      container.decorate(:service) { |value| [:decorated] + value }

      expect(container.resolve(:service)).to eql(%i[decorated raw dep_value])
    end

    it "resolves dependency keys relative to the surrounding namespace" do
      container.namespace("jobs") do
        register(:queue, -> { :queued })
        register(:worker, depends_on: [:queue]) { |queue:| "worker/#{queue}" }
        register(:other, depends_on: {q: :queue}) { |q:| q }
      end

      expect(container.resolve("jobs.worker")).to eql("worker/queued")
      expect(container.resolve("jobs.other")).to be(:queued)
    end
  end

  describe "missing dependencies" do
    it "reports the path from the target down to the missing registration" do
      container.register(:b, depends_on: [:c]) { |c:| c }
      container.register(:a, depends_on: [:b]) { |b:| b }

      expect { container.resolve(:a) }.to raise_error(
        Dry::Container::MissingDependencyError,
        %r{from "a": a -> b -> c}
      )
    end

    it "does not call any factory while validating the chain" do
      called = []
      container.register(:b, depends_on: [:missing]) { called << :b }
      container.register(:a, depends_on: [:b]) { called << :a }

      expect { container.resolve(:a) }.to raise_error(
        Dry::Container::MissingDependencyError
      )
      expect(called).to be_empty
    end

    it "still raises KeyError when the target itself is not registered" do
      expect { container.resolve(:nope) }.to raise_error(KeyError)
    end

    it "keeps the fallback block behaviour for a missing target" do
      expect(container.resolve(:nope) { :fallback }).to be(:fallback)
    end
  end

  describe "circular dependencies" do
    it "rejects an ordinary cycle of eager dependencies" do
      container.register(:a, depends_on: [:b]) { |b:| "a/#{b}" }
      container.register(:b, depends_on: [:c]) { |c:| "b/#{c}" }
      container.register(:c, depends_on: [:a]) { |a:| "c/#{a}" }

      expect { container.resolve(:a) }.to raise_error(
        Dry::Container::CircularDependencyError,
        /a -> b -> c -> a/
      )
    end

    it "allows a cycle as soon as one edge is declared lazy" do
      container.register(:logger, depends_on: {sink: :sinks}) do |sink:|
        Object.new.tap { |logger| logger.define_singleton_method(:sink) { sink } }
      end
      container.register(:sinks, lazy_depends_on: [:logger]) { |logger:| logger }

      logger = container.resolve(:logger)
      expect(logger.sink.call).to be(logger)
    end
  end

  describe "lazy dependencies" do
    it "hands the factory a callable and only resolves when it is called" do
      initialized = []
      container.register(:heavy, -> { initialized << :heavy; :heavy })
      container.register(:app, lazy_depends_on: [:heavy]) { |heavy:| heavy }

      thunk = container.resolve(:app)
      expect(initialized).to be_empty
      expect(thunk.call).to be(:heavy)
      expect(initialized).to eql([:heavy])
    end

    it "allows the dependency to be registered after the declaration" do
      container.register(:app, lazy_depends_on: [:later]) { |later:| later }
      app = container.resolve(:app)

      expect { app.call }.to raise_error(Dry::Container::MissingDependencyError)

      container.register(:later, -> { :ready })
      expect(app.call).to be(:ready)
    end

    it "does not traverse a lazy edge while validating the eager chain" do
      initialized = []
      container.register(:eager_dep, -> { initialized << :eager_dep; :eager })
      container.register(:missing_eager_dep, depends_on: [:absent]) { :x }
      container.register(:app,
                         depends_on: [:eager_dep],
                         lazy_depends_on: [:missing_eager_dep]) do |eager_dep:, missing_eager_dep:|
        [eager_dep, missing_eager_dep]
      end

      eager, lazy = container.resolve(:app)
      expect(initialized).to eql([:eager_dep])
      expect(eager).to be(:eager)

      expect { lazy.call }.to raise_error(
        Dry::Container::MissingDependencyError,
        /missing_eager_dep -> absent/
      )
    end
  end

  describe "reachable scope" do
    it "activates only dependencies reachable from the resolved target" do
      started = []

      container.register(:unused_dep, -> { started << :unused_dep; :unused })
      container.register(:unused, depends_on: [:unused_dep]) do
        started << :unused
      end
      container.register(:boom) { raise "this component must never be started" }
      container.register(:used_dep, -> { started << :used_dep; :used })
      container.register(:used, depends_on: [:used_dep]) do |used_dep:|
        started << :used
        used_dep
      end

      expect(container.resolve(:used)).to be(:used)
      expect(started).to eql(%i[used_dep used])
    end
  end

  describe "memoization shared by two factories" do
    it "builds a memoized dependency once for both consumers and across resolutions" do
      builds = Hash.new(0)

      container.register(:shared, memoize: true) { builds[:shared] += 1; Object.new }
      container.register(:left, depends_on: [:shared]) { |shared:| shared }
      container.register(:right, depends_on: [:shared]) { |shared:| shared }
      container.register(:pair, depends_on: %i[left right]) do |left:, right:|
        [left, right]
      end

      first_left, first_right = container.resolve(:pair)
      expect(first_left).to be(first_right)
      expect(builds[:shared]).to be(1)

      expect(container.resolve(:left)).to be(first_left)
      expect(container.resolve(:right)).to be(first_right)
      expect(builds[:shared]).to be(1)
    end

    it "still resolves a target when an unrelated registered factory raises" do
      container.register(:shared, memoize: true) { Object.new }
      container.register(:healthy, depends_on: [:shared]) { |shared:| shared }
      container.register(:broken) { raise "boom in unrelated factory" }

      expect { container.resolve(:healthy) }.not_to raise_error
      expect { container.resolve(:broken) }.to raise_error(RuntimeError, /boom/)
    end
  end
end
