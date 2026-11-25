# frozen_string_literal: true

RSpec.describe "Container / Imports / Rebinding imported keys" do
  # The "upstream" container publishes two imported keys (plus a third that the
  # downstream service never declares), each memoized so the first resolve pins
  # an instance for the duration of its use.
  let(:upstream) {
    Class.new(Dry::System::Container) {
      configure do |config|
        config.exports = %w[connection_a connection_b connection_c]
      end

      register("connection_a", memoize: true) { Object.new }
      register("connection_b", memoize: true) { Object.new }
      register("connection_c", memoize: true) { Object.new }
    }
  }

  # A downstream service imports exactly two of the upstream keys.
  let(:services) {
    upstream = self.upstream

    Class.new(Dry::System::Container) {
      import keys: %w[connection_a connection_b], from: upstream, as: :upstream
    }
  }

  # A component that holds onto a resolved connection for the duration of its
  # own lifecycle, mimicking a component still "winding down".
  let(:long_lived_component) {
    Class.new {
      attr_reader :connection

      def initialize(connection)
        @connection = connection
      end
    }
  }

  # Publishes a new instance for an upstream key, as if the upstream container
  # had replaced the connection while keeping the same key name.
  def republish(key, instance = Object.new)
    upstream._container.delete(key)
    upstream.register(key, instance)
    instance
  end

  describe "explicit rebinding of an updated imported key" do
    it "resolves the newly published instance for the rebound key only" do
      old_connection_a = services["upstream.connection_a"]
      old_connection_b = services["upstream.connection_b"]

      component = long_lived_component.new(old_connection_a)
      new_connection_a = republish("connection_a")

      services.rebind_import(from: upstream, as: :upstream, keys: ["connection_a"])

      # The key resolved later uses the new instance, thanks to the explicit rebind
      expect(services["upstream.connection_a"]).to be new_connection_a

      # The old component keeps the connection it was already holding until its
      # own lifecycle ends
      expect(component.connection).to be old_connection_a

      # The other imported key keeps its original source and instance
      expect(services["upstream.connection_b"]).to be old_connection_b
    end

    it "does not build keys whose source is unchanged" do
      old_connection_b = services["upstream.connection_b"]

      builds = []
      upstream._container.delete("connection_b")
      upstream.register("connection_b") { builds << :connection_b }

      # Rebinding connection_a (not yet loaded) must not cause connection_b to
      # be built again
      republish("connection_a")
      services.rebind_import(from: upstream, as: :upstream, keys: ["connection_a"])

      expect(builds).to be_empty
      expect(services["upstream.connection_b"]).to be old_connection_b
    end

    it "imports a declared key for the first time when it has not been lazily loaded" do
      expect(services.registered?("upstream.connection_a")).to be false

      new_connection_a = republish("connection_a")
      services.rebind_import(from: upstream, as: :upstream, keys: ["connection_a"])

      expect(services["upstream.connection_a"]).to be new_connection_a
      expect(services.registered?("upstream.connection_b")).to be false
    end
  end

  describe "updating one imported key while keeping the old source for another" do
    it "shows each connection according to its own period of use" do
      old_connection_a = services["upstream.connection_a"]
      old_connection_b = services["upstream.connection_b"]

      new_connection_a = republish("connection_a")
      services.rebind_import(from: upstream, as: :upstream, keys: ["connection_a"])

      expect(services["upstream.connection_a"]).to be new_connection_a
      expect(services["upstream.connection_a"]).not_to be old_connection_a
      expect(services["upstream.connection_b"]).to be old_connection_b

      expect(services.keys).to contain_exactly(
        "upstream.connection_a", "upstream.connection_b"
      )
    end
  end

  describe "a failed rebuild" do
    context "when a key can no longer be resolved from the source container" do
      before do
        services["upstream.connection_a"]
        services["upstream.connection_b"]

        upstream._container.delete("connection_a")
      end

      it "raises and leaves all existing imported registrations untouched" do
        current_a = services["upstream.connection_a"]
        current_b = services["upstream.connection_b"]

        expect {
          services.rebind_import(from: upstream, as: :upstream, keys: ["connection_a"])
        }.to raise_error(Dry::System::ImportedKeyNotFoundError, /connection_a/)

        # Nothing was replaced: both keys still resolve to their prior instances
        expect(services["upstream.connection_a"]).to be current_a
        expect(services["upstream.connection_b"]).to be current_b
      end

      it "does not replace the healthy keys rebuilt alongside the failure" do
        current_a = services["upstream.connection_a"]
        current_b = services["upstream.connection_b"]

        expect {
          services.rebind_import(
            from: upstream, as: :upstream, keys: %w[connection_a connection_b]
          )
        }.to raise_error(Dry::System::ImportedKeyNotFoundError)

        expect(services["upstream.connection_a"]).to be current_a
        expect(services["upstream.connection_b"]).to be current_b
      end
    end

    context "when a key was not previously imported" do
      it "raises without touching the existing registrations" do
        current_b = services["upstream.connection_b"]

        expect {
          services.rebind_import(from: upstream, as: :upstream, keys: ["connection_c"])
        }.to raise_error(Dry::System::ImportedKeyNotFoundError, /connection_c/)

        expect(services["upstream.connection_b"]).to be current_b
      end
    end

    context "when the source container does not match the registered import" do
      it "raises an argument error" do
        other_upstream = Class.new(Dry::System::Container) {
          register "connection_a", Object.new
        }

        expect {
          services.rebind_import(from: other_upstream, as: :upstream, keys: ["connection_a"])
        }.to raise_error(ArgumentError, /not registered for the given source container/)
      end
    end
  end

  describe "import boundaries" do
    it "cannot resolve a third key that was never declared, even after rebinding" do
      expect(services.key?("upstream.connection_c")).to be false

      republish("connection_a")
      services.rebind_import(from: upstream, as: :upstream, keys: ["connection_a"])

      expect(services.key?("upstream.connection_c")).to be false
      expect { services["upstream.connection_c"] }.to raise_error(Dry::Core::Container::KeyError)
    end
  end

  describe "with a nil (root) import namespace" do
    let(:services) {
      upstream = self.upstream

      Class.new(Dry::System::Container) {
        import keys: %w[connection_a connection_b], from: upstream, as: nil
      }
    }

    it "rebinds keys at the root namespace and leaves the others untouched" do
      old_connection_a = services["connection_a"]
      old_connection_b = services["connection_b"]

      new_connection_a = republish("connection_a")
      services.rebind_import(from: upstream, as: nil, keys: ["connection_a"])

      expect(services["connection_a"]).to be new_connection_a
      expect(services["connection_b"]).to be old_connection_b
      expect(services["connection_a"]).not_to be old_connection_a
    end

    it "reports undeclared keys without a namespace prefix in the error" do
      expect {
        services.rebind_import(from: upstream, as: nil, keys: ["connection_c"])
      }.to raise_error(Dry::System::ImportedKeyNotFoundError, /"connection_c"/)
    end
  end

  describe "after finalization" do
    it "cannot be rebound once the container is finalized" do
      services.finalize!

      expect {
        services.rebind_import(from: upstream, as: :upstream, keys: ["connection_a"])
      }.to raise_error(Dry::System::ContainerAlreadyFinalizedError)
    end
  end
end
