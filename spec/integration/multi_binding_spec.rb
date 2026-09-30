# frozen_string_literal: true

RSpec.describe "multi-bindings" do
  let(:container) { Dry::Container.new }

  describe "registering and resolving a collection" do
    it "resolves all members in their declared order, not registration order" do
      container.register_many(:handlers) do |handlers|
        handlers.add(:second, -> { "second" }, order: 20)
        handlers.add(:first, -> { "first" }, order: 10)
      end

      expect(container.resolve_many(:handlers)).to eq(%w[first second])
    end

    it "keeps registration order as a tie-breaker for members with equal order" do
      container.register_many(:handlers) do |handlers|
        handlers.add(:b, -> { "b" })
        handlers.add(:a, -> { "a" })
        handlers.add(:c, -> { "c" }, order: -1)
      end

      expect(container.resolve_many(:handlers)).to eq(%w[c b a])
    end

    it "does not depend on the order in which members were first resolved" do
      container.register_many(:handlers) do |handlers|
        handlers.add(:second, -> { "second" }, order: 20)
        handlers.add(:first, -> { "first" }, order: 10)
      end

      container.resolve_member(:handlers, :second)

      expect(container.resolve_many(:handlers)).to eq(%w[first second])
    end

    it "resolves the whole collection through #resolve as well" do
      container.register_many(:handlers) do |handlers|
        handlers.add(:one, -> { "one" }, order: 1)
        handlers.add(:two, -> { "two" }, order: 2)
      end

      expect(container.resolve(:handlers)).to eq(%w[one two])
      expect(container[:handlers]).to eq(%w[one two])
    end

    it "registers the multi-binding under its key" do
      container.register_many(:handlers) do |handlers|
        handlers.add(:one, "one")
      end

      expect(container.keys).to eq(["handlers"])
      expect(container.key?(:handlers)).to be true
    end

    it "supports a zero-arity block" do
      container.register_many(:handlers) do
        add(:one, "one")
      end

      expect(container.resolve_many(:handlers)).to eq(["one"])
    end

    it "returns self for chaining" do
      expect(container.register_many(:handlers) { |h| h.add(:one, "one") }).to be(container)
    end
  end

  describe "resolving by tags" do
    before do
      container.register_many(:handlers) do |handlers|
        handlers.add(:email, -> { "email" }, tags: %i[notifications], order: 1)
        handlers.add(:sms, -> { "sms" }, tags: %i[notifications urgent], order: 2)
        handlers.add(:audit, -> { "audit" }, tags: %i[audit], order: 3)
      end
    end

    it "resolves only members carrying the given tag" do
      expect(container.resolve_many(:handlers, tags: [:notifications])).to eq(%w[email sms])
    end

    it "accepts a single tag" do
      expect(container.resolve_many(:handlers, tags: :audit)).to eq(%w[audit])
    end

    it "requires members to carry all of the given tags" do
      expect(container.resolve_many(:handlers, tags: %i[notifications urgent])).to eq(%w[sms])
    end

    it "returns an empty collection when no member matches" do
      expect(container.resolve_many(:handlers, tags: [:unknown])).to eq([])
    end

    it "does not call factories of members filtered out" do
      called = []

      container = Dry::Container.new
      container.register_many(:handlers) do |handlers|
        handlers.add(:email, -> { called << :email }, tags: [:notifications])
        handlers.add(:audit, -> { called << :audit }, tags: [:audit])
      end

      container.resolve_many(:handlers, tags: [:notifications])

      expect(called).to eq([:email])
    end
  end

  describe "resolving a single member" do
    before do
      container.register_many(:handlers) do |handlers|
        handlers.add(:email, -> { "email" }, order: 1)
        handlers.add(:sms, -> { "sms" }, order: 2)
      end
    end

    it "resolves a member by name" do
      expect(container.resolve_member(:handlers, :email)).to eq("email")
      expect(container.resolve_member(:handlers, "sms")).to eq("sms")
    end

    it "raises an error pointing out the source when the member is unknown" do
      expect { container.resolve_member(:handlers, :unknown) }.to raise_error(
        Dry::Container::Error,
        /no member named "unknown".*"handlers"/
      )
    end
  end

  describe "per-member options" do
    it "honours call: false per member" do
      callable = -> { "called" }

      container.register_many(:handlers) do |handlers|
        handlers.add(:plain, callable, call: false)
        handlers.add(:called, -> { "called" })
      end

      expect(container.resolve_member(:handlers, :plain)).to be(callable)
      expect(container.resolve_many(:handlers)).to eq([callable, "called"])
    end

    it "honours memoize: true per member" do
      container.register_many(:handlers) do |handlers|
        handlers.add(:memoized, -> { Object.new }, memoize: true)
        handlers.add(:plain, -> { Object.new })
      end

      expect(container.resolve_member(:handlers, :memoized))
        .to be(container.resolve_member(:handlers, :memoized))
      expect(container.resolve_member(:handlers, :plain))
        .not_to be(container.resolve_member(:handlers, :plain))
    end

    it "shares the memoized member between collection and single-member resolution" do
      container.register_many(:handlers) do |handlers|
        handlers.add(:memoized, -> { Object.new }, memoize: true)
      end

      expect(container.resolve_many(:handlers).first)
        .to be(container.resolve_member(:handlers, :memoized))
    end
  end

  describe "name conflicts" do
    it "raises an error when the collection key is already registered" do
      container.register_many(:handlers) { |handlers| handlers.add(:email, "one") }

      expect {
        container.register_many(:handlers) { |handlers| handlers.add(:email, "two") }
      }.to raise_error(Dry::Container::Error, /already an item registered with the key "handlers"/)
    end

    it "raises an error naming the member and the collection on duplicate member names" do
      expect {
        container.register_many(:handlers) do |handlers|
          handlers.add(:email, "one")
          handlers.add(:email, "two")
        end
      }.to raise_error(Dry::Container::Error, /member named "email".*"handlers" multi-binding/)
    end
  end

  describe "error handling" do
    it "raises KeyError when resolving an unregistered key" do
      expect { container.resolve_many(:missing) }.to raise_error(KeyError)
      expect { container.resolve_member(:missing, :one) }.to raise_error(KeyError)
    end

    it "raises an error when the key is registered as a single item" do
      container.register(:item, "item")

      expect { container.resolve_many(:item) }.to raise_error(
        Dry::Container::Error, /single item/
      )
      expect { container.resolve_member(:item, :one) }.to raise_error(
        Dry::Container::Error, /single item/
      )
    end

    it "raises an error when no block is given" do
      expect { container.register_many(:handlers) }.to raise_error(
        Dry::Container::Error, /requires a block/
      )
    end
  end

  describe "namespaces and merging" do
    let(:source) { Dry::Container.new }

    before do
      source.namespace(:admin) do
        register_many(:handlers) do |handlers|
          handlers.add(:email, -> { Object.new }, tags: [:notifications], memoize: true)
          handlers.add(:sms, -> { Object.new }, tags: [:notifications], memoize: true)
        end
      end
    end

    it "resolves namespaced collections and members" do
      expect(source.resolve_many("admin.handlers", tags: [:notifications]).length).to eq(2)
      expect(source.resolve_member("admin.handlers", :email))
        .to be(source.resolve_many("admin.handlers").first)
    end

    it "shares the cached member between collection and single-item resolution after merging" do
      target = Dry::Container.new
      target.merge(source)

      expect(target.resolve_many("admin.handlers").first)
        .to be(target.resolve_member("admin.handlers", :email))
    end

    it "shares the cached member when merged under another namespace" do
      target = Dry::Container.new
      target.merge(source, namespace: :backoffice)

      expect(target.resolve_many("backoffice.admin.handlers").first)
        .to be(target.resolve_member("backoffice.admin.handlers", :email))
    end
  end
end
