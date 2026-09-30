# frozen_string_literal: true

require "thread"

RSpec.describe "dependency-aware replacement" do
  subject(:container) { Dry::Container.new }

  # A reusable two-phase barrier. Waiting blocks until #release is called,
  # then every wait passes immediately; the barrier is one-shot.
  class TestBarrier
    def initialize
      @mutex = Mutex.new
      @condition = ConditionVariable.new
      @open = false
    end

    def wait
      @mutex.synchronize do
        @condition.wait(@mutex) until @open
      end
    end

    def release
      @mutex.synchronize do
        @open = true
        @condition.broadcast
      end
    end

    def open?
      @mutex.synchronize { @open }
    end
  end

  def join_with_timeout(thread, seconds: 5)
    thread.join(seconds) || raise("thread did not finish within #{seconds}s")
  end

  # Domain objects: two distinguishable "client" generations and a
  # memoized service that captures the client it was built with.
  let(:client_v1) { Object.new }
  let(:client_v2) { Object.new }

  describe "#replace" do
    before do
      container.register(:client, client_v1)
      container.register(:unrelated, memoize: true) { Object.new }
      container.register(
        :service,
        memoize: true,
        depends_on: [:client]
      ) { [container[:client], Object.new] }
    end

    it "replaces the registration with the given value" do
      container.replace(:client, client_v2)

      expect(container[:client]).to be(client_v2)
    end

    it "accepts a block as the new registration" do
      container.replace(:client, memoize: true) { client_v2 }

      expect(container[:client]).to be(client_v2)
      expect(container[:client]).to be(container[:client])
    end

    it "keeps unrelated memoized objects identical" do
      unrelated_before = container[:unrelated]

      container.replace(:client, client_v2)

      expect(container[:unrelated]).to be(unrelated_before)
    end

    it "invalidates memoized dependents so they are rebuilt on next resolve" do
      service_v1 = container[:service]
      expect(service_v1[0]).to be(client_v1)

      container.replace(:client, client_v2)

      service_v2 = container[:service]
      expect(service_v2[0]).to be(client_v2)
      expect(service_v2).not_to be(service_v1)
    end

    it "does not rebuild dependents until they are actually resolved" do
      service_v1 = container[:service]

      builds = 0
      container.replace(:client, client_v2)
      container.replace(:service, depends_on: [:client]) {
        builds += 1
        [container[:client], Object.new]
      }

      expect(builds).to be(0)
      container[:service]
      expect(builds).to be(1)
    end

    it "invalidates the replaced memoized key itself" do
      builds = 0
      container.replace(:service, memoize: true, depends_on: [:client]) {
        builds += 1
        Object.new
      }

      first = container[:service]
      container.replace(:service, memoize: true, depends_on: [:client]) {
        builds += 1
        Object.new
      }
      second = container[:service]

      expect(second).not_to be(first)
      expect(builds).to be(2)
    end

    context "with transitive dependencies" do
      before do
        container.register(
          :workflow,
          memoize: true,
          depends_on: [:service]
        ) { [container[:service], Object.new] }
      end

      it "invalidates the whole dependent closure, transitively" do
        workflow_v1 = container[:workflow]
        expect(workflow_v1[0][0]).to be(client_v1)

        container.replace(:client, client_v2)

        workflow_v2 = container[:workflow]
        expect(workflow_v2[0][0]).to be(client_v2)
        expect(workflow_v2).not_to be(workflow_v1)
      end

      it "leaves branches that do not depend on the replaced key untouched" do
        unrelated_before = container[:unrelated]
        container.register(
          :other_branch,
          memoize: true,
          depends_on: [:unrelated]
        ) { container[:unrelated] }

        other_before = container[:other_branch]
        workflow_v1 = container[:workflow]

        container.replace(:client, client_v2)

        expect(container[:workflow]).not_to be(workflow_v1)
        expect(container[:other_branch]).to be(other_before)
        expect(container[:unrelated]).to be(unrelated_before)
      end
    end

    context "when the replacement fails" do
      it "raises when the key was never registered" do
        expect { container.replace(:missing, Object.new) }.to raise_error(KeyError)
      end

      it "leaves every registration intact for an unknown key" do
        expect { container.replace(:missing, Object.new) }.to raise_error(KeyError)

        expect(container[:client]).to be(client_v1)
        expect(container[:service][0]).to be(client_v1)
      end

      it "raises when a declared dependency is unknown" do
        expect {
          container.replace(:service, -> { :x }, depends_on: [:nope])
        }.to raise_error(Dry::Container::Error, /nope/)
      end

      it "leaves every registration intact for an undeclared dependency" do
        expect {
          container.replace(:service, -> { :x }, depends_on: [:nope])
        }.to raise_error(Dry::Container::Error)

        expect(container[:service][0]).to be(client_v1)
        expect(container._dependency_state.dependencies("service")).to eq(["client"])
      end

      it "raises inside the transaction but keeps the old registration usable" do
        service_v1 = container[:service]

        expect {
          container.replace(:service, -> { :x }, depends_on: [:does_not_exist])
        }.to raise_error(Dry::Container::Error)

        # the memo was not invalidated by the failed transaction either
        expect(container[:service]).to be(service_v1)
      end

      it "keeps everything intact when building the new registration itself fails" do
        client_v1_resolved = container[:client]
        service_v1 = container[:service]

        expect {
          # memoize only supports callables, so the new item cannot be built
          container.replace(:client, "plain value", memoize: true)
        }.to raise_error(Dry::Container::Error, /Memoize/)

        expect(container[:client]).to be(client_v1_resolved)
        expect(container[:service]).to be(service_v1)
      end
    end

    context "with a factory running against the old dependency" do
      # Start construction -> replace while construction is in flight ->
      # let construction finish. The stale factory result must not be
      # published; the cache must end up holding the new-dependency build.
      it "never publishes the stale result" do
        started = TestBarrier.new
        release = TestBarrier.new
        built = []

        container.replace(:service, memoize: true, depends_on: [:client]) {
          built << container[:client]
          # only the first, stale build blocks; retries run to completion
          started.release
          release.wait if built.size == 1
          [container[:client], Object.new]
        }

        worker = Thread.new { container[:service] }
        started.wait
        expect(built).to eq([client_v1])

        container.replace(:client, client_v2)
        release.release
        service_resolved = join_with_timeout(worker)

        # the waiting resolution retries and never observes old client
        expect(service_resolved.value[0]).to be(client_v2)
        expect(built).to eq([client_v1, client_v2])

        cached = container[:service]
        expect(cached[0]).to be(client_v2)
        expect(cached).to be(service_resolved.value)
      end

      it "does not publish a half-old/half-new combination of dependencies" do
        repo_v1 = Object.new
        repo_v2 = Object.new
        container.register(:repo, repo_v1)
        container.register(
          :handler,
          memoize: true,
          depends_on: %i[client repo]
        ) { [container[:client], container[:repo], Object.new] }

        # warm both leaves, then replace them while the handler factory
        # is in flight
        expect(container[:client]).to be(client_v1)
        expect(container[:repo]).to be(repo_v1)

        started = TestBarrier.new
        release = TestBarrier.new
        built = []

        container.replace(:handler, memoize: true, depends_on: %i[client repo]) {
          pair = [container[:client], container[:repo]]
          built << pair
          started.release
          release.wait if built.size == 1
          [*pair, Object.new]
        }

        worker = Thread.new { container[:handler] }
        started.wait
        expect(built.last).to eq([client_v1, repo_v1])

        container.replace(:client, client_v2)
        container.replace(:repo, repo_v2)
        release.release
        resolved = join_with_timeout(worker)

        # the half-old/half-new build never reached the caller...
        expect(resolved.value[0..1]).to eq([client_v2, repo_v2])
        # ...and never became the cached version
        expect(container[:handler][0..1]).to eq([client_v2, repo_v2])
        expect(container[:handler]).to be(resolved.value)
        expect(built).to eq([[client_v1, repo_v1], [client_v2, repo_v2]])
      end

      it "pins dependencies the factory resolves before the barrier" do
        # The factory only reads the client before blocking; the repo is
        # read after the replacement happens. Explicit edges must pin
        # both, so the stale attempt retries instead of returning a mix.
        repo_v1 = Object.new
        repo_v2 = Object.new
        container.register(:repo, repo_v1)
        container.register(
          :handler,
          memoize: true,
          depends_on: %i[client repo]
        ) { Object.new }

        started = TestBarrier.new
        release = TestBarrier.new
        builds = 0

        container.replace(:handler, memoize: true, depends_on: %i[client repo]) {
          seen_client = container[:client]
          first = (builds += 1) == 1
          started.release
          release.wait if first
          [seen_client, container[:repo], Object.new]
        }

        worker = Thread.new { container[:handler] }
        started.wait
        container.replace(:client, client_v2)
        container.replace(:repo, repo_v2)
        release.release
        resolved = join_with_timeout(worker)

        expect(resolved.value[0..1]).to eq([client_v2, repo_v2])
        expect(container[:handler][0..1]).to eq([client_v2, repo_v2])
        expect(container[:handler]).to be(resolved.value)
        expect(builds).to eq(2)
      end
    end

    it "rebuilds a lazy memoized dependent the first time it is resolved after replace" do
      container.register(
        :lazy_service,
        memoize: true,
        depends_on: [:client]
      ) { container[:client] }

      container.replace(:client, client_v2)

      expect(container[:lazy_service]).to be(client_v2)
    end

    it "returns the container" do
      expect(container.replace(:client, client_v2)).to be(container)
    end

    it "always re-resolves non-memoized dependents, so they follow the new client" do
      container.register(:dynamic_service, depends_on: [:client]) { container[:client] }

      expect(container[:dynamic_service]).to be(client_v1)
      container.replace(:client, client_v2)
      expect(container[:dynamic_service]).to be(client_v2)
    end

    it "supports nil as a memoized value" do
      builds = 0
      container.register(:maybe, memoize: true, depends_on: [:client]) { builds += 1; nil }

      expect(container[:maybe]).to be_nil
      expect(container[:maybe]).to be_nil
      expect(builds).to be(1)

      container.replace(:client, client_v2)

      expect(container[:maybe]).to be_nil
      expect(builds).to be(2)
    end

    it "leaves a duplicated container with an independent cache and dependency graph" do
      copy = container.dup
      copy.register(:copy_only, memoize: true, depends_on: [:client]) { Object.new }

      # replacement inside the copy only invalidates the copy's caches
      expect { copy.replace(:client, client_v2) }.not_to change {
        container[:client]
      }

      # the original container's token for :client did not move
      expect(container._dependency_state.token("client")).to be(1)
      expect(copy._dependency_state.token("client")).to be(2)

      # keys registered after the duplication stay local to the copy
      expect(copy.key?(:copy_only)).to be(true)
      expect(container.key?(:copy_only)).to be(false)

      # original memoized service retains its identity
      expect(container[:service]).to be(container[:service])
    end

    it "keeps consistency under concurrent replacements and resolutions" do
      versions = (1..20).map { |i| :"client_#{i}" }
      container.register(:counter, memoize: true, depends_on: [:client]) {
        [container[:client], Object.new]
      }

      errors = []
      writers = 2.times.map { |w|
        Thread.new {
          versions.each_with_index do |version, i|
            next unless (i + w).even?

            container.replace(:client, version)
          end
        }
      }

      readers = 8.times.map {
        Thread.new {
          500.times do
            client, _object = container[:counter]
            acceptable = versions + [client_v1]
            acceptable.include?(client) || errors << "unknown version #{client.inspect}"
          end
        }
      }

      (writers + readers).each { |t| join_with_timeout(t, seconds: 15) }

      expect(errors).to be_empty
      expect(versions).to include(container[:counter][0])
    end
  end

  describe "explicit dependency relations" do
    before do
      container.register(:client, client_v1)
    end

    it "rebuilds dependents even when the factory does not resolve through the container itself" do
      # The service is handed the client externally; nothing would tell
      # the container about the relation without depends_on
      given_client = client_v1
      container.register(:opaque_service, memoize: true, depends_on: [:client]) {
        Object.new.tap { |obj| obj.define_singleton_method(:client) { given_client } }
      }

      service_v1 = container[:opaque_service]
      given_client = client_v2
      container.replace(:client, client_v2)

      expect(container[:opaque_service]).not_to be(service_v1)
      expect(container[:opaque_service].client).to be(client_v2)
    end

    it "allows changing the declared edges on replacement" do
      container.register(:other, memoize: true) { Object.new }
      container.register(:node, memoize: true, depends_on: [:client]) { Object.new }

      container.replace(:node, -> { Object.new }, depends_on: [:other])

      expect(container._dependency_state.dependencies("node")).to eq(["other"])
    end
  end
end
