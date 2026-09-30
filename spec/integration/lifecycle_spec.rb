# frozen_string_literal: true

RSpec.describe "lifecycles and scopes" do
  subject(:container) { Dry::Container.new }

  describe "lifecycle: :resolution" do
    it "builds a fresh instance on every resolution" do
      counter = 0
      container.register(:thing, -> { counter += 1 }, lifecycle: :resolution)

      expect(container[:thing]).to be(1)
      expect(container[:thing]).to be(2)
    end
  end

  describe "lifecycle: :application" do
    it "memoizes the instance for the whole application" do
      counter = 0
      container.register(:thing, -> { counter += 1 }, lifecycle: :application)

      container.with_scope do
        expect(container[:thing]).to be(1)
      end

      container.with_scope do
        expect(container[:thing]).to be(1)
      end
    end

    it "is equivalent to memoize: true" do
      counter = 0
      container.register(:thing, -> { counter += 1 }, memoize: true)

      expect(container[:thing]).to be(1)
      expect(container[:thing]).to be(1)
    end
  end

  describe "lifecycle: :scope" do
    it "cannot be resolved without an active scope" do
      container.register(:current_user, -> { :user }, lifecycle: :scope)

      expect { container[:current_user] }.to raise_error(
        Dry::Container::ScopeError, /outside of an active scope/
      )
    end

    it "memoizes the instance within a single scope" do
      counter = 0
      container.register(:current_user, -> { counter += 1 }, lifecycle: :scope)

      container.with_scope do
        expect(container[:current_user]).to be(1)
        expect(container[:current_user]).to be(1)
      end
    end

    it "builds a separate instance for every scope" do
      counter = 0
      container.register(:current_user, -> { counter += 1 }, lifecycle: :scope)

      first = container.with_scope { container[:current_user] }
      second = container.with_scope { container[:current_user] }

      expect(first).to be(1)
      expect(second).to be(2)
    end

    it "does not leak an instance resolved inside a scope after it closes" do
      container.register(:current_user, -> { Object.new }, lifecycle: :scope)

      first_user = container.with_scope { container[:current_user] }
      second_user = container.with_scope { container[:current_user] }

      expect(first_user).to_not be(second_user)
    end

    it "supports nested scopes with separate caches" do
      seen = []
      container.register(:current_user, -> { Object.new }, lifecycle: :scope)

      container.with_scope(:outer) do
        outer_user = container[:current_user]

        container.with_scope(:inner) do
          seen << container[:current_user]
        end

        expect(container[:current_user]).to be(outer_user)
      end

      expect(seen.size).to be(1)
    end

    it "exposes the currently active scope" do
      expect(container.current_scope).to be_nil

      container.with_scope(:request) do |scope|
        expect(container.current_scope).to be(scope)
        expect(scope.name).to be(:request)
      end

      expect(container.current_scope).to be_nil
    end
  end

  describe "scope lifecycle boundaries" do
    it "prevents an application service from capturing the request-level current_user" do
      container.register(:current_user, -> { :request_user }, lifecycle: :scope)
      container.register(:user_service,
                        -> { container[:current_user] },
                        lifecycle: :application)

      expect { container[:user_service] }.to raise_error(
        Dry::Container::ScopeLeakError,
        /"user_service" \(application\) -> "current_user" \(scope\)/
      )
    end

    it "allows a scope-scoped object to depend on an application-scoped object" do
      container.register(:settings, -> { :settings }, lifecycle: :application)
      container.register(:current_user,
                        -> { [container[:settings], :user] },
                        lifecycle: :scope)

      expect(container.with_scope { container[:current_user] }).to eql(%i[settings user])
    end

    it "allows a scope-scoped object to depend on another scope-scoped object" do
      container.register(:current_user, -> { :user }, lifecycle: :scope)
      container.register(:greeting,
                        -> { "hello #{container[:current_user]}" },
                        lifecycle: :scope)

      expect(container.with_scope { container[:greeting] }).to eql("hello user")
    end

    it "rejects an application factory that captures a scope-scoped dependency" do
      container.register(:current_user, -> { :user }, lifecycle: :scope)
      container.register(:auditor,
                        -> { container[:current_user] },
                        lifecycle: :application)

      expect { container[:auditor] }.to raise_error(
        Dry::Container::ScopeLeakError,
        /"auditor" \(application\).*"current_user" \(scope\)/m
      )
    end

    it "reports the construction chain and never memoizes the leaked object" do
      container.register(:request_context, -> { :ctx }, lifecycle: :scope)
      container.register(:repository,
                        -> { container[:request_context] },
                        lifecycle: :scope)
      container.register(:service,
                        -> { container[:repository] },
                        lifecycle: :application)

      expect { container[:service] }.to raise_error(
        Dry::Container::ScopeLeakError,
        /"service" \(application\) -> "repository" \(scope\)/
      )

      container.with_scope do
        expect { container[:service] }.to raise_error(Dry::Container::ScopeLeakError)
      end
    end

    it "raises the boundary error while the object is being constructed" do
      container.register(:current_user, -> { :user }, lifecycle: :scope)
      constructed = false
      container.register(:auditor,
                        -> { container[:current_user]; constructed = true },
                        lifecycle: :application)

      expect { container[:auditor] }.to raise_error(Dry::Container::ScopeLeakError)
      expect(constructed).to be(false)
    end
  end

  describe "explicit scopes" do
    it "can be opened, activated and closed manually" do
      counter = 0
      container.register(:current_user, -> { counter += 1 }, lifecycle: :scope)

      scope = container.open_scope(:request)
      expect(container.current_scope).to be_nil

      scope.activate
      first = container[:current_user]
      expect(container[:current_user]).to be(first)
      scope.close!

      expect(container.current_scope).to be_nil

      new_scope = container.open_scope(:request)
      new_scope.activate
      expect(container[:current_user]).to_not be(first)
      new_scope.close!
    end

    it "can interleave two scopes on the same fiber" do
      counter = 0
      container.register(:current_user, -> { counter += 1 }, lifecycle: :scope)

      request_a = container.open_scope(:a)
      request_b = container.open_scope(:b)

      request_a.activate
      user_a = container[:current_user]

      request_b.activate
      expect(container[:current_user]).to_not be(user_a)
      expect(container[:current_user]).to_not be_nil
      request_b.close!

      expect(container[:current_user]).to be(user_a)
      request_a.close!
    end

    it "cannot be reactivated once closed" do
      scope = container.open_scope(:closed)
      scope.activate
      scope.close!

      expect(scope).to be_closed
      expect { scope.activate }.to raise_error(
        Dry::Container::ScopeClosedError, /closed and cannot be reactivated/
      )
      expect(container.current_scope).to be_nil
    end

    it "cannot resolve scoped objects through a reference to a closed scope" do
      counter = 0
      container.register(:current_user, -> { counter += 1 }, lifecycle: :scope)

      scope = container.open_scope(:request)
      scope.activate
      container[:current_user]
      scope.close!

      expect {
        scope.memoize("current_user") { raise "factory must not run" }
      }.to raise_error(Dry::Container::ScopeClosedError)
    end

    it "cannot be closed twice" do
      scope = container.open_scope
      scope.activate
      scope.close!

      expect { scope.close! }.to raise_error(Dry::Container::ScopeClosedError)
    end

    it "releases its cache when closed" do
      container.register(:current_user, -> { Object.new }, lifecycle: :scope)
      scope = container.open_scope
      scope.activate
      user = container[:current_user]
      scope.close!

      new_scope = container.open_scope
      new_scope.activate
      begin
        expect(container[:current_user]).to_not be(user)
      ensure
        new_scope.close!
      end
    end
  end

  describe "concurrency" do
    it "isolates scopes across interleaved fibers on the same thread" do
      container.register(:current_user, -> { Object.new }, lifecycle: :scope)

      outer_user = nil

      container.with_scope do
        outer_user = container[:current_user]

        Fiber.new do
          expect(container.current_scope).to be_nil

          container.with_scope do
            expect(container[:current_user]).to_not be(outer_user)
          end
        end.resume

        expect(container[:current_user]).to be(outer_user)
      end
    end

    it "isolates scopes across threads" do
      container.register(:current_user, -> { Object.new }, lifecycle: :scope)

      users = Array.new(2) { {} }
      ready = Queue.new
      release = Queue.new

      threads = 2.times.map do |index|
        Thread.new do
          container.with_scope do
            users[index][:first] = container[:current_user]
            ready << true
            release.pop
            users[index][:second] = container[:current_user]
          end
        end
      end

      2.times { ready.pop }
      2.times { release << true }
      threads.each(&:join)

      expect(users[0][:first]).to be(users[0][:second])
      expect(users[1][:first]).to be(users[1][:second])
      expect(users[0][:first]).to_not be(users[1][:first])
    end

    it "does not allow a scope closed in one fiber to be used in another" do
      container.register(:current_user, -> { :user }, lifecycle: :scope)

      scope = container.open_scope
      started = Queue.new
      release = Queue.new

      worker = Thread.new do
        scope.activate
        started << true
        release.pop
        expect(container[:current_user]).to be(:user)
        scope.close!
      end

      started.pop

      expect { scope.activate }.to raise_error(
        Dry::Container::ScopeError, /active in another context/
      )

      release << true
      worker.join

      expect { scope.activate }.to raise_error(Dry::Container::ScopeClosedError)
    end
  end

  describe "registration" do
    it "rejects unknown lifecycles" do
      expect {
        container.register(:thing, -> {}, lifecycle: :bogus)
      }.to raise_error(Dry::Container::ScopeError, /unknown lifecycle :bogus/)
    end

    it "rejects conflicting memoize and lifecycle options" do
      expect {
        container.register(:thing, -> {}, lifecycle: :scope, memoize: true)
      }.to raise_error(Dry::Container::ScopeError, /conflicting options/)

      expect {
        container.register(:thing, -> {}, lifecycle: :application, memoize: false)
      }.to raise_error(Dry::Container::ScopeError, /conflicting options/)
    end

    it "accepts lifecycle as a string" do
      container.register(:thing, -> { :thing }, lifecycle: "scope")

      expect(container.with_scope { container[:thing] }).to be(:thing)
    end
  end
end
