# frozen_string_literal: true

require "dry/system/container"

RSpec.describe Dry::System::Container, ".import" do
  subject(:app) { Class.new(Dry::System::Container) }

  let(:db) do
    Class.new(Dry::System::Container) do
      register(:users, %w[jane joe])
    end
  end

  it "imports one container into another" do
    app.import(from: db, as: :persistence)

    expect(app.registered?("persistence.users")).to be(false)

    app.finalize!

    expect(app["persistence.users"]).to eql(%w[jane joe])
  end

  context "when container has been finalized" do
    it "raises an error" do
      app.finalize!

      expect do
        app.import(from: db, as: :persistence)
      end.to raise_error(Dry::System::ContainerAlreadyFinalizedError)
    end
  end

  describe ".rebind_import" do
    let(:db) do
      Class.new(Dry::System::Container) do
        configure { |config| config.exports = %w[users sessions] }

        register(:users, %w[jane joe])
        register(:sessions, %w[abc])
      end
    end

    before do
      app.import(keys: %w[users sessions], from: db, as: :persistence)
    end

    it "rebuilds the given imported key from the source container" do
      expect(app["persistence.users"]).to eql(%w[jane joe])

      db._container.delete("users")
      db.register(:users, %w[alice])

      app.rebind_import(from: db, as: :persistence, keys: ["users"])

      expect(app["persistence.users"]).to eql(%w[alice])
    end

    it "leaves imported keys not included in the rebind untouched" do
      app["persistence.users"]
      original_sessions = app["persistence.sessions"]

      db._container.delete("users")
      db.register(:users, %w[alice])

      app.rebind_import(from: db, as: :persistence, keys: ["users"])

      expect(app["persistence.sessions"]).to be(original_sessions)
    end

    it "raises when a key was not declared for import" do
      expect {
        app.rebind_import(from: db, as: :persistence, keys: ["unknown"])
      }.to raise_error(Dry::System::ImportedKeyNotFoundError, /unknown/)
    end

    it "leaves existing registrations in place when the rebuild fails" do
      original_users = app["persistence.users"]
      original_sessions = app["persistence.sessions"]

      db._container.delete("users")

      expect {
        app.rebind_import(from: db, as: :persistence, keys: ["users"])
      }.to raise_error(Dry::System::ImportedKeyNotFoundError)

      expect(app["persistence.users"]).to be(original_users)
      expect(app["persistence.sessions"]).to be(original_sessions)
    end

    it "raises an error when the container has been finalized" do
      app.finalize!

      expect {
        app.rebind_import(from: db, as: :persistence, keys: ["users"])
      }.to raise_error(Dry::System::ContainerAlreadyFinalizedError)
    end
  end

  describe "import module" do
    it "loads system when it was not loaded in the imported container yet" do
      class Test::Other < Dry::System::Container
        configure do |config|
          config.root = SPEC_ROOT.join("fixtures/import_test").realpath
          config.component_dirs.add "lib"
        end
      end

      class Test::Container < Dry::System::Container
        configure do |config|
          config.root = SPEC_ROOT.join("fixtures/test").realpath
          config.component_dirs.add "lib"
        end

        import from: Test::Other, as: :other
      end

      module Test
        Import = Container.injector
      end

      class Test::Foo
        include Test::Import["other.test.bar"]
      end

      expect(Test::Foo.new.bar).to be_instance_of(Test::Bar)
    end
  end
end
