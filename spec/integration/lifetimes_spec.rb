# frozen_string_literal: true

RSpec.describe "lifetimes and scopes" do
  let(:container) { Dry::Container.new }

  describe "lifetimes" do
    describe ":resolution (default for callables)" do
      it "builds a new value on every resolution" do
        builds = 0
        container.register(:item) { builds += 1 }

        expect(container.resolve(:item)).to be(1)
        expect(container.resolve(:item)).to be(2)
      end

      it "is selected with lifetime: :resolution explicitly" do
        builds = 0
        container.register(:item, lifetime: :resolution) { builds += 1 }

        container.resolve(:item)
        expect(container.resolve(:item)).to be(2)
      end
    end

    describe ":application" do
      it "is selected with memoize: true and caches globally" do
        builds = 0
        container.register(:item, memoize: true) { builds += 1 }

        container.with_scope { container.resolve(:item) }
        container.with_scope { container.resolve(:item) }

        expect(container.resolve(:item)).to be(1)
      end

      it "is selected with lifetime: :application" do
        builds = 0
        container.register(:item, lifetime: :application) { builds += 1 }

        container.with_scope { container.resolve(:item) }
        expect(container.resolve(:item)).to be(1)
      end
    end

    describe ":scope" do
      it "caches within a scope" do
        container.register(:item, lifetime: :scope) { Object.new }

        container.with_scope do |scope|
          expect(container.resolve(:item)).to be(scope.resolve(:item))
        end
      end

      it "builds separate values for separate scopes" do
        container.register(:item, lifetime: :scope) { Object.new }

        first = nil
        second = nil

        container.with_scope { first = container.resolve(:item) }
        container.with_scope { second = container.resolve(:item) }

        expect(first).not_to be(second)
      end

      it "restores the outer scope after a nested scope ends" do
        container.register(:item, lifetime: :scope) { Object.new }

        container.with_scope do
          outer = container.resolve(:item)

          container.with_scope do
            expect(container.resolve(:item)).not_to be(outer)
          end

          expect(container.resolve(:item)).to be(outer)
        end
      end

      it "cannot be resolved without an active scope" do
        container.register(:item, lifetime: :scope) { Object.new }

        expect { container.resolve(:item) }
          .to raise_error(Dry::Container::NoActiveScopeError, /outside of an active scope/)
      end

      it "rejects static values on registration" do
        expect { container.register(:item, "value", lifetime: :scope) }
          .to raise_error(ArgumentError, /scope lifetime is only supported/)
      end
    end

    describe "invalid lifetime" do
      it "raises an ArgumentError at registration time" do
        expect { container.register(:item, lifetime: :request) { 1 } }
          .to raise_error(
            ArgumentError,
            'invalid lifetime :request (expected one of :application, :scope, :resolution)'
          )
      end
    end
  end

  describe "explicit scopes" do
    it "resolves through enter_scope/close! and caches per scope" do
      container.register(:item, lifetime: :scope) { Object.new }

      scope = container.enter_scope

      expect(scope.resolve(:item)).to be(scope.resolve(:item))

      scope.close!
    end

    it "closes the scope after the with_scope block even when it raises" do
      container.register(:item, lifetime: :scope) { Object.new }
      captured = nil

      expect {
        container.with_scope do |scope|
          captured = scope
          raise "boom"
        end
      }.to raise_error("boom")

      expect(captured).to be_closed
    end

    it "makes close! idempotent" do
      scope = container.enter_scope

      expect(scope.close!).to be(scope)
      expect { scope.close! }.not_to raise_error
    end

    it "does not let a closed scope resolve again" do
      container.register(:item, lifetime: :scope) { Object.new }

      scope = container.enter_scope(:request)
      scope.resolve(:item)
      scope.close!

      expect { scope.resolve(:item) }
        .to raise_error(Dry::Container::ScopeClosedError, /:request.*closed/)
    end

    it "isolates two interleaved scopes on the same thread" do
      container.register(:item, lifetime: :scope) { Object.new }

      first_scope = container.enter_scope(:first)
      second_scope = container.enter_scope(:second)

      first_value = first_scope.resolve(:item)
      second_value = second_scope.resolve(:item)

      expect(first_scope.resolve(:item)).to be(first_value)
      expect(second_scope.resolve(:item)).to be(second_value)
      expect(first_value).not_to be(second_value)
    end

    it "uses the top of the scope stack as the ambient scope" do
      container.register(:item, lifetime: :scope) { Object.new }

      first_scope = container.enter_scope
      second_scope = container.enter_scope

      second_value = second_scope.resolve(:item)
      expect(container.resolve(:item)).to be(second_value)

      second_scope.close!

      first_value = first_scope.resolve(:item)
      expect(container.resolve(:item)).to be(first_value)

      first_scope.close!
    end

    it "removes a closed scope from another thread's view through its reference" do
      container.register(:item, lifetime: :scope) { Object.new }

      scope = container.enter_scope
      scope.resolve(:item)
      scope.close!

      error = nil
      Thread.new do
        begin
          scope.resolve(:item)
        rescue => e
          error = e
        end
      end.join

      expect(error).to be_a(Dry::Container::ScopeClosedError)
    end
  end

  describe "captive scope dependencies" do
    it "raises when an application-level factory resolves a scope-level item" do
      container.register(:current_user, lifetime: :scope) { Object.new }
      container.register(:service, lifetime: :application) { container.resolve(:current_user) }

      expect {
        container.with_scope { container.resolve(:service) }
      }.to raise_error(
        Dry::Container::CaptiveDependencyError,
        /"service" captures scope-level dependency "current_user".*service -> current_user/m
      )
    end

    it "reports the full construction chain through other lifetimes" do
      container.register(:current_user, lifetime: :scope) { Object.new }
      container.register(:policy, lifetime: :resolution) { container.resolve(:current_user) }
      container.register(:service, lifetime: :application) { container.resolve(:policy) }

      error = nil
      begin
        container.with_scope { container.resolve(:service) }
      rescue Dry::Container::CaptiveDependencyError => e
        error = e
      end

      expect(error).to be_a(Dry::Container::CaptiveDependencyError)
      expect(error.message).to include("service -> policy -> current_user")
    end

    it "fails while constructing the application item, not when it is used" do
      container.register(:current_user, lifetime: :scope) { Object.new }
      container.register(:service, lifetime: :application) { container.resolve(:current_user) }

      built = false
      begin
        container.with_scope { container.resolve(:service) }
      rescue Dry::Container::CaptiveDependencyError
        built = true
      end

      expect(built).to be(true)
    end

    it "raises on every construction attempt before the application value can be cached" do
      container.register(:current_user, lifetime: :scope) { Object.new }
      container.register(:service, lifetime: :application) { container.resolve(:current_user) }

      2.times do
        expect {
          container.with_scope { container.resolve(:service) }
        }.to raise_error(Dry::Container::CaptiveDependencyError)
      end
    end

    it "allows an application factory that does not capture scope-level items" do
      container.register(:clock, lifetime: :resolution) { :tick }
      container.register(:service, lifetime: :application) { container.resolve(:clock) }

      expect(container.with_scope { container.resolve(:service) }).to be(:tick)
    end

    it "tracks the construction chain through other containers" do
      other = Dry::Container.new
      other.register(:current_user, lifetime: :scope) { Object.new }
      container.register(:service, lifetime: :application) { other.resolve(:current_user) }

      expect {
        other.with_scope { container.resolve(:service) }
      }.to raise_error(
        Dry::Container::CaptiveDependencyError,
        /service -> current_user/
      )
    end
  end

  describe "thread isolation" do
    it "keeps scope caches and ambient scopes per thread" do
      container.register(:item, lifetime: :scope) { Object.new }

      first_value = nil
      second_value = nil

      first = Thread.new do
        container.with_scope do
          first_value = container.resolve(:item)
          sleep 0.05
          expect(container.resolve(:item)).to be(first_value)
        end
      end

      second = Thread.new do
        sleep 0.01
        container.with_scope do
          second_value = container.resolve(:item)
          expect(container.resolve(:item)).to be(second_value)
        end
      end

      first.join
      second.join

      expect(first_value).not_to be(second_value)
    end

    it "does not leak ambient scope to another thread" do
      container.register(:item, lifetime: :scope) { Object.new }

      thread = Thread.new do
        container.with_scope do
          container.resolve(:item)
          sleep 0.05
        end
      end

      sleep 0.01
      expect { container.resolve(:item) }
        .to raise_error(Dry::Container::NoActiveScopeError)

      thread.join
    end
  end
end
