# frozen_string_literal: true

RSpec.describe "Dry::Container::Mixin#replace" do
  let(:container) { Dry::Container.new }

  it "resolves the replacement afterwards" do
    container.register("client", :old_client)
    container.replace("client", :new_client)

    expect(container.resolve("client")).to be(:new_client)
  end

  it "returns self" do
    container.register("client", :old_client)

    expect(container.replace("client", :new_client)).to be(container)
  end

  it "accepts a block and options, like register" do
    container.register("service", memoize: true) { Object.new }
    first = container.resolve("service")

    container.replace("service", memoize: true) { Object.new }

    expect(container.resolve("service")).not_to be(first)
    expect(container.resolve("service")).to be(container.resolve("service"))
  end

  it "rebuilds memoized items that declared the replaced key as a dependency" do
    container.register("client", :old_client)
    container.register("service", memoize: true, depends_on: %w[client]) do
      {client: container.resolve("client")}
    end

    before = container.resolve("service")
    expect(before[:client]).to be(:old_client)

    container.replace("client", :new_client)

    rebuilt = container.resolve("service")
    expect(rebuilt[:client]).to be(:new_client)
    expect(rebuilt).not_to be(before)
    expect(container.resolve("service")).to be(rebuilt)
  end

  it "accepts dependencies given as symbols" do
    container.register(:client, :old_client)
    container.register(:service, memoize: true, depends_on: [:client]) do
      {client: container.resolve(:client)}
    end

    container.resolve(:service)
    container.replace(:client, :new_client)

    expect(container.resolve(:service)[:client]).to be(:new_client)
  end

  it "rebuilds transitive dependents of the replaced key" do
    container.register("client", :old_client)
    container.register("service", memoize: true, depends_on: %w[client]) do
      {client: container.resolve("client")}
    end
    container.register("repo", memoize: true, depends_on: %w[service]) do
      {service: container.resolve("service")}
    end

    container.resolve("repo")

    container.replace("client", :new_client)

    repo = container.resolve("repo")
    expect(repo[:service][:client]).to be(:new_client)
  end

  it "keeps the identity of memoized items that do not depend on the replaced key" do
    container.register("client", :old_client)
    container.register("unrelated", memoize: true) { Object.new }
    container.register("service", memoize: true, depends_on: %w[client]) { Object.new }

    unrelated = container.resolve("unrelated")
    container.resolve("service")

    container.replace("client", :new_client)

    expect(container.resolve("unrelated")).to be(unrelated)
  end

  context "when the replacement fails" do
    it "raises when no item is registered with the key and keeps the registrations" do
      container.register("client", :old_client)

      expect { container.replace("missing", :nope) }
        .to raise_error(Dry::Container::Error, /no item registered/i)

      expect(container.resolve("client")).to be(:old_client)
      expect(container.keys).to eq(%w[client])
    end

    it "keeps the original registration when building the replacement fails" do
      container.register("client", :old_client)

      expect { container.replace("client", :not_callable, memoize: true) }
        .to raise_error(Dry::Container::Error, /memoize only supported/i)

      expect(container.resolve("client")).to be(:old_client)
    end

    it "raises FrozenError on a frozen container and keeps the registrations" do
      container.register("client", :old_client)
      container.freeze

      expect { container.replace("client", :new_client) }
        .to raise_error(FrozenError, /can't modify frozen/)

      expect(container.resolve("client")).to be(:old_client)
    end
  end

  describe "concurrent replacement" do
    it "never publishes a memoized value built from pre-replacement dependencies" do
      container.register("client", :old_client)

      construction_started = Queue.new
      finish_construction = Queue.new
      constructions = 0

      container.register("service", memoize: true, depends_on: %w[client]) do
        client = container.resolve("client")
        constructions += 1

        # Pause in the middle of the first construction so the replacement
        # lands between "construction started" and "construction finished"
        if constructions == 1
          construction_started << true
          finish_construction.pop
        end

        {client: client}
      end

      resolved = nil
      thread = Thread.new { resolved = container.resolve("service") }

      # construction has started and captured the old client
      construction_started.pop

      # the replacement lands before the construction finishes
      container.replace("client", :new_client)

      # construction completes with the old client in hand
      finish_construction << true
      thread.join

      # the in-flight resolution noticed the replacement and rebuilt, and the
      # cache holds the version built from the new client - not the value that
      # was mid-construction with the old client when the replacement landed
      expect(resolved[:client]).to be(:new_client)
      expect(container.resolve("service")).to be(resolved)
      expect(constructions).to eq(2)
    end

    it "never returns a mix of pre- and post-replacement values from a single resolution" do
      container.register("client", :old_client)
      container.register("settings", :old_settings)

      client_read = Queue.new
      continue_reading = Queue.new
      constructions = 0

      container.register("service", memoize: true, depends_on: %w[client settings]) do
        client = container.resolve("client")
        constructions += 1

        if constructions == 1
          client_read << true
          continue_reading.pop
        end

        {client: client, settings: container.resolve("settings")}
      end

      resolved = nil
      thread = Thread.new { resolved = container.resolve("service") }

      # the resolution has read the old client and pauses before reading settings
      client_read.pop

      container.replace("client", :new_client)
      container.replace("settings", :new_settings)

      # the first construction now combines the old client with the new
      # settings; it must be neither returned nor cached
      continue_reading << true
      thread.join

      expect(resolved).to eq(client: :new_client, settings: :new_settings)
      expect(container.resolve("service")).to be(resolved)
      expect(constructions).to eq(2)
    end

    it "retries non-memoized resolutions that span a replacement" do
      container.register("client", :old_client)
      container.register("settings", :old_settings)

      client_read = Queue.new
      continue_reading = Queue.new
      calls = 0

      container.register("service") do
        client = container.resolve("client")
        calls += 1

        if calls == 1
          client_read << true
          continue_reading.pop
        end

        {client: client, settings: container.resolve("settings")}
      end

      resolved = nil
      thread = Thread.new { resolved = container.resolve("service") }

      client_read.pop

      container.replace("client", :new_client)
      container.replace("settings", :new_settings)

      continue_reading << true
      thread.join

      expect(resolved).to eq(client: :new_client, settings: :new_settings)
      expect(calls).to eq(2)
    end
  end
end
