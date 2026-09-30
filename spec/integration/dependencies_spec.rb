# frozen_string_literal: true

RSpec.describe "Explicit dependency declarations" do
  let(:container) { Dry::Container.new }

  describe "injecting dependencies" do
    it "passes declared dependency values to the factory as keyword arguments" do
      container.register(:logger) { "logger" }
      container.register(:repo, deps: [:logger]) { |logger:| "repo using #{logger}" }

      expect(container.resolve(:repo)).to eql("repo using logger")
    end

    it "resolves dependencies declared with a single key" do
      container.register(:a) { 1 }
      container.register(:b, deps: :a) { |a:| a + 1 }

      expect(container.resolve(:b)).to be(2)
    end

    it "resolves the whole chain before building the target" do
      order = []
      container.register(:cfg) { order << :cfg; {url: "db://x"} }
      container.register(:db, deps: [:cfg]) { |cfg:| order << :db; [:db, cfg] }
      container.register(:repo, deps: [:db]) { |db:| order << :repo; [:repo, db] }
      container.register(:app, deps: [:repo]) { |repo:| order << :app; [:app, repo] }

      container.resolve(:app)

      expect(order).to eql(%i[cfg db repo app])
    end

    it "works with blocks registered through register" do
      container.register(:one) { 1 }
      container.register(:two, deps: [:one]) { |one:| one + 1 }

      expect(container[:two]).to be(2)
    end

    it "accepts string and symbol keys interchangeably" do
      container.register("svc.logger") { "L" }
      container.register(:worker, deps: ["svc.logger"]) { |**deps| deps[:"svc.logger"] }

      expect(container.resolve(:worker)).to eql("L")
    end
  end

  describe "memoization of shared dependencies" do
    it "builds a dependency shared by two factories only once" do
      builds = Hash.new(0)

      container.register(:store, memoize: true) { builds[:store] += 1; :store }
      container.register(:reader, deps: [:store]) { |store:| builds[:reader] += 1; [:reader, store] }
      container.register(:writer, deps: [:store]) { |store:| builds[:writer] += 1; [:writer, store] }
      container.register(:app, deps: %i[reader writer store]) do |reader:, writer:, store:|
        builds[:app] += 1
        [reader, writer, store]
      end

      app = container.resolve(:app)

      expect(builds).to eql(store: 1, reader: 1, writer: 1, app: 1)
      expect(app[0][1]).to be(app[1][1])
      expect(app[0][1]).to be(app[2])
    end

    it "returns the same target instance on repeated resolution" do
      container.register(:a) { "a" }
      container.register(:b, deps: [:a]) { |a:| {a: a} }

      expect(container.resolve(:b)).to be(container.resolve(:b))
    end

    it "rebuilds on every resolution when memoize is disabled" do
      builds = 0
      container.register(:a) { "a" }
      container.register(:b, deps: [:a], memoize: false) { |a:| builds += 1; {a: a} }

      container.resolve(:b)
      container.resolve(:b)

      expect(builds).to be(2)
      expect(container.resolve(:b)).not_to be(container.resolve(:b))
    end

    it "still memoizes a plain proc declared with memoize: true" do
      count = 0
      container.register(:p, proc { count += 1 }, memoize: true)

      container.resolve(:p)
      container.resolve(:p)

      expect(count).to be(1)
    end
  end

  describe "partial resolution" do
    it "does not instantiate anything outside the target's reachable chain" do
      touched = []

      container.register(:a) { touched << :a; :a }
      container.register(:b, deps: [:a]) { |a:| touched << :b; [:b, a] }
      container.register(:unrelated_boom) { touched << :boom; raise "kaboom" }
      container.register(:unrelated_chain, deps: [:unrelated_boom]) { |unrelated_boom:| unrelated_boom }

      expect(container.resolve(:b)).to eql([:b, :a])
      expect(touched).to eql(%i[a b])
    end

    it "can resolve a healthy target again after an unrelated factory fails" do
      container.register(:good) { :good }
      container.register(:bad) { raise "boom" }

      expect { container.resolve(:bad) }.to raise_error(RuntimeError, "boom")
      expect(container.resolve(:good)).to be(:good)
    end

    it "validates lazily without calling factories" do
      container.register(:a, deps: [:b]) { |b:| b }
      container.register(:b, deps: [:c]) { |c:| c }
      # intentionally missing :c

      expect { container.resolve(:a) }.to raise_error(Dry::Container::DependencyError)
    end
  end

  describe "missing dependencies" do
    it "reports the path from the target to the missing key" do
      container.register(:a, deps: [:b]) { |b:| b }
      container.register(:b, deps: [:missing]) { |missing:| missing }

      expect { container.resolve(:a) }.to raise_error(
        Dry::Container::DependencyError,
        %r{Missing dependency "missing".*chain a -> b -> missing}
      )
    end

    it "reports a missing direct dependency" do
      container.register(:a, deps: [:nope]) { |nope:| nope }

      expect { container.resolve(:a) }.to raise_error(
        Dry::Container::DependencyError,
        %r{Missing dependency "nope".*chain a -> nope}
      )
    end

    it "reports a missing target of a lazy edge with its chain" do
      container.register(:a, lazy_deps: [:b]) { |b:| b }

      expect { container.resolve(:a) }.to raise_error(
        Dry::Container::DependencyError,
        %r{Missing dependency "b".*lazy.*chain a -> b}
      )
    end

    it "still raises KeyError when the target itself is missing" do
      expect { container.resolve(:ghost) }.to raise_error(KeyError)
    end

    it "still supports the fallback block for a missing target" do
      expect(container.resolve(:ghost) { :fallback }).to be(:fallback)
    end
  end

  describe "circular dependencies" do
    it "rejects a plain two-node cycle" do
      container.register(:a, deps: [:b]) { |b:| b }
      container.register(:b, deps: [:a]) { |a:| a }

      expect { container.resolve(:a) }.to raise_error(
        Dry::Container::DependencyError,
        /Circular dependency detected: a -> b -> a/
      )
    end

    it "rejects a longer eager cycle reachable from the target" do
      container.register(:a, deps: [:b]) { |b:| b }
      container.register(:b, deps: [:c]) { |c:| c }
      container.register(:c, deps: [:b]) { |b:| b }

      expect { container.resolve(:a) }.to raise_error(
        Dry::Container::DependencyError,
        /Circular dependency detected: b -> c -> b/
      )
    end

    it "rejects a self dependency" do
      container.register(:a, deps: [:a]) { |a:| a }

      expect { container.resolve(:a) }.to raise_error(Dry::Container::DependencyError, /a -> a/)
    end

    it "allows a diamond without reporting a cycle" do
      container.register(:d) { :d }
      container.register(:b, deps: [:d]) { |d:| [:b, d] }
      container.register(:c, deps: [:d]) { |d:| [:c, d] }
      container.register(:a, deps: %i[b c]) { |b:, c:| [b, c] }

      expect(container.resolve(:a)).to eql([[:b, :d], [:c, :d]])
    end
  end

  describe "lazy dependencies" do
    it "hands over a reference which does not resolve the target during construction" do
      built = []
      container.register(:b, deps: [:a]) { |a:| built << :b; [:b, a] }
      container.register(:a, lazy_deps: [:b]) { |b:| built << :a; [:a, b] }

      a = container.resolve(:a)

      expect(built).to eql([:a])
      expect(a.first).to eql(:a)
      expect(a.last).to be_a(Dry::Container::Resolver::LazyReference)
      expect(built).to eql([:a])

      expect(a.last.__getobj__.first).to eql(:b)
      expect(built).to include(:b)
    end

    it "resolves a lazy reference on first method call and memoizes it" do
      builds = 0
      container.register(:b) { builds += 1; "b-value" }
      container.register(:a, lazy_deps: [:b]) { |b:| b }

      a = container.resolve(:a)

      expect(builds).to be(0)
      expect(a.to_s).to eql("b-value")
      expect(a.upcase).to eql("B-VALUE")
      expect(builds).to be(1)
    end

    it "allows a cycle that goes through a lazy edge" do
      container.register(:a, deps: [:b]) { |b:| {a: b} }
      container.register(:b, lazy_deps: [:a]) { |a:| {b: a} }

      a = container.resolve(:a)
      lazy_b = a.fetch(:a).fetch(:b)

      expect(lazy_b).to be_a(Dry::Container::Resolver::LazyReference)
      expect(lazy_b.__getobj__).to equal(a)
    end

    it "validates the lazy target's own chain only when dereferenced" do
      container.register(:a, lazy_deps: [:b]) { |b:| b }
      container.register(:b, deps: [:c]) { |c:| c }
      # :c missing only matters when :b is actually resolved

      expect { container.resolve(:a) }.not_to raise_error

      expect { container.resolve(:a).__getobj__ }.to raise_error(
        Dry::Container::DependencyError,
        /Missing dependency "c"/
      )
    end

    it "does not validate chains of lazy edges while the target builds" do
      # b -> c -> b is a plain cycle, but it is reachable only through a
      # lazy edge and therefore must not block resolving :a
      container.register(:a, lazy_deps: [:b]) { |b:| b }
      container.register(:b, deps: [:c]) { |c:| c }
      container.register(:c, deps: [:b]) { |b:| b }

      expect { container.resolve(:a) }.not_to raise_error
      expect { container.resolve(:a).__getobj__ }.to raise_error(
        Dry::Container::DependencyError, /Circular dependency detected: b -> c -> b/
      )
    end

    it "raises when a factory dereferences a dependency still under construction" do
      container.register(:x, lazy_deps: [:x]) { |x:| x.__getobj__ }

      expect { container.resolve(:x) }.to raise_error(
        Dry::Container::DependencyError, /Circular dependency detected: x -> x/
      )
    end
  end

  describe "invalid declarations" do
    it "raises when dependencies are declared for a non-callable value" do
      expect {
        container.register(:a, "value", deps: [:b])
      }.to raise_error(Dry::Container::Error, /Dependencies can only be declared/)
    end

    it "raises when dependencies are combined with call: false" do
      expect {
        container.register(:a, ->(b:) { b }, deps: [:b], call: false)
      }.to raise_error(Dry::Container::Error, /not supported together with `call: false`/)
    end

    it "raises when a key is declared both eager and lazy" do
      expect {
        container.register(:a, ->(b:) { b }, deps: [:b], lazy_deps: [:b])
      }.to raise_error(Dry::Container::Error, /eager and lazy/)
    end
  end

  describe "decoration" do
    it "keeps dependency wiring when decorating a factory item" do
      container.register(:a) { "a" }
      container.register(:b, deps: [:a]) { |a:| "b(#{a})" }
      require "delegate"
      container.decorate(:b, with: SimpleDelegator)

      expect(container.resolve(:b).__getobj__).to eql("b(a)")
    end
  end

  describe "stubbing" do
    before do
      require "dry/container/stub"
      container.enable_stubs!
      container.register(:cfg) { "real" }
      container.register(:svc, deps: [:cfg]) { |cfg:| "svc:#{cfg}" }
    end

    after { container.unstub }

    it "uses the stubbed value when resolving a dependent" do
      container.stub(:cfg, "stubbed")

      expect(container.resolve(:svc)).to eql("svc:stubbed")
    end
  end
end
