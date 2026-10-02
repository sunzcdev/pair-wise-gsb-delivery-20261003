# frozen_string_literal: true

require "spec_helper"

RSpec.describe Pundit::Context do
  let(:user) { double }
  let(:post) { Post.new(user) }
  let(:comment) { Comment.new }
  let(:article) { Article.new }
  let(:context) { described_class.new(user: user) }

  def authorize(record, query:, policy_class: nil)
    context.authorize(record, query: query, policy_class: policy_class)
  end

  describe "#authorize" do
    it "returns the record when authorized" do
      expect(authorize(post, query: :update?)).to be(post)
    end

    it "raises when not authorized" do
      expect { authorize(Post.new, query: :update?) }.to raise_error(Pundit::NotAuthorizedError)
    end

    it "returns the record when given a namespaced record" do
      expect(authorize([:project, comment], query: :update?)).to be(comment)
    end

    it "uses an explicit policy class" do
      expect(authorize(post, query: :create?, policy_class: PublicationPolicy)).to be(post)
    end

    it "raises when the policy cannot be found" do
      expect { authorize(article, query: :update?) }.to raise_error(Pundit::NotDefinedError)
    end
  end

  describe "#policy" do
    it "returns an instantiated policy" do
      policy = context.policy(post)

      expect(policy).to be_a(PostPolicy)
      expect(policy.user).to be(user)
      expect(policy.record).to be(post)
    end

    it "returns nil when the policy cannot be found" do
      expect(context.policy(article)).to be_nil
    end

    it "raises when the policy constructor is invalid" do
      expect { context.policy(Wiki.new) }.to raise_error(Pundit::InvalidConstructorError)
    end
  end

  describe "#policy!" do
    it "raises when the policy cannot be found" do
      expect { context.policy!(article) }.to raise_error(Pundit::NotDefinedError)
    end
  end

  describe "#policy_scope" do
    it "resolves the scope" do
      expect(context.policy_scope(Post)).to eq(:published)
    end

    it "resolves a namespaced scope" do
      expect(context.policy_scope([:project, Post])).to eq(:read)
    end

    it "returns nil when the scope cannot be found" do
      expect(context.policy_scope(Article)).to be_nil
    end
  end

  describe "#policy_scope!" do
    it "raises when the scope cannot be found" do
      expect { context.policy_scope!(Article) }.to raise_error(Pundit::NotDefinedError)
    end
  end

  describe "policy cache" do
    it "fetches policies through it" do
      store = {}
      context = described_class.new(user: user, policy_cache: Pundit::CacheStore::LegacyStore.new(store))

      policy = context.policy(post)

      expect(context.policy(post)).to be(policy)
      expect(store).to eq({post => policy})
    end
  end

  describe "#policy_permissions" do
    it "returns an ordered matrix of booleans" do
      post = Post.new
      archived_post = Post.new(user)
      archived_post.archived = true

      result = context.policy_permissions([post, archived_post], %i[show? destroy? archive?])

      expect(result).to eq([[true, false, true], [true, false, false]])
    end

    it "preserves duplicate records and queries while evaluating them once" do
      post = Post.new(user)
      record_class = Class.new do
        attr_reader :id

        def initialize(id, allowed)
          @id = id
          @allowed = allowed
        end

        def allowed?
          @allowed
        end

        def ==(other)
          other.is_a?(self.class) && other.id == id
        end
        alias_method :eql?, :==

        def hash
          id.hash
        end
      end
      first_record = record_class.new(1, true)
      second_record = record_class.new(1, false)
      policy_class = Class.new(BasePolicy) do
        class << self
          attr_accessor :initializations, :show_checks
        end

        def initialize(user, record)
          self.class.initializations += 1
          super
        end

        def show?
          self.class.show_checks += 1
          record.allowed?
        end
      end
      policy_class.initializations = 0
      policy_class.show_checks = 0

      result = context.policy_permissions(
        [first_record, second_record, first_record],
        [:show?, "show?"],
        policy_class: policy_class
      )

      expect(result).to eq([[true, true], [false, false], [true, true]])
      expect(policy_class.initializations).to eq(2)
      expect(policy_class.show_checks).to eq(2)
    end

    it "uses namespaces as part of record identity" do
      Project::CommentPolicy.instances = 0
      Project::Admin::CommentPolicy.instances = 0

      namespaced_comment = [:project, comment]
      result = context.policy_permissions(
        [namespaced_comment, [:project, :admin, comment], namespaced_comment],
        [:show?]
      )

      expect(result).to eq([[true], [false], [true]])
      expect(Project::CommentPolicy.instances).to eq(1)
      expect(Project::Admin::CommentPolicy.instances).to eq(1)
    end

    it "uses explicit policy classes as part of record identity" do
      explicit_policy_class = Class.new(BasePolicy) do
        class << self
          attr_accessor :initializations
        end

        def initialize(user, record)
          self.class.initializations += 1
          super
        end

        def show?
          false
        end
      end
      explicit_policy_class.initializations = 0

      result = context.policy_permissions([post, post], %i[show?], policy_class: explicit_policy_class)

      expect(result).to eq([[false], [false]])
      expect(explicit_policy_class.initializations).to eq(1)
    end

    it "does not reuse policies for value-equal records that are different objects" do
      policy_class = Class.new(BasePolicy) do
        class << self
          attr_accessor :initializations
        end

        def initialize(user, record)
          self.class.initializations += 1
          super
        end

        def show?
          record.allowed?
        end
      end
      policy_class.initializations = 0
      record_class = Class.new do
        attr_reader :id

        def initialize(id, allowed)
          @id = id
          @allowed = allowed
        end

        def allowed?
          @allowed
        end

        def ==(other)
          other.is_a?(self.class) && other.id == id
        end
        alias_method :eql?, :==

        def hash
          id.hash
        end
      end
      first_record = record_class.new(1, true)
      second_record = record_class.new(1, false)
      record_class.define_method(:policy_class) { policy_class }
      store = {}
      context = described_class.new(user: user, policy_cache: Pundit::CacheStore::LegacyStore.new(store))

      result = context.policy_permissions([first_record, second_record], %i[show?])

      expect(result).to eq([[true], [false]])
      expect(policy_class.initializations).to eq(2)
    end

    it "does not cache query results across calls when record state changes" do
      post = Post.new(user)
      store = {}
      context = described_class.new(user: user, policy_cache: Pundit::CacheStore::LegacyStore.new(store))

      expect(context.policy_permissions([post], %i[edit? archive?])).to eq([[true, true]])

      post.archived = true

      expect(context.policy_permissions([post], %i[edit? archive?])).to eq([[true, false]])
    end

    it "reuses policy instances across calls without caching query results" do
      policy_class = Class.new(BasePolicy) do
        class << self
          attr_accessor :initializations, :show_checks
        end

        def initialize(user, record)
          self.class.initializations += 1
          super
        end

        def show?
          self.class.show_checks += 1
          true
        end
      end
      policy_class.initializations = 0
      policy_class.show_checks = 0

      expect(context.policy_permissions([post], %i[show?], policy_class: policy_class))
        .to eq([[true]])
      expect(context.policy_permissions([post], %i[show?], policy_class: policy_class))
        .to eq([[true]])

      expect(policy_class.initializations).to eq(1)
      expect(policy_class.show_checks).to eq(2)
    end

    it "supports empty batches" do
      expect(context.policy_permissions([], %i[show?])).to eq([])
      expect(context.policy_permissions([post], [])).to eq([[]])
    end

    it "reports a missing policy with the record index and query" do
      expect do
        context.policy_permissions([post, article], %i[show?])
      end.to raise_error(
        Pundit::BatchAuthorizationError,
        "unable to evaluate query `show?` for record at index 1"
      ) do |error|
        expect(error.record_index).to eq(1)
        expect(error.query).to eq(:show?)
        expect(error.record).to be(article)
      end
    end

    it "reports a missing query method without treating it as false" do
      expect do
        context.policy_permissions([post], %i[missing?])
      end.to raise_error(
        Pundit::BatchAuthorizationError,
        "unable to evaluate query `missing?` for record at index 0"
      ) do |error|
        expect(error.record_index).to eq(0)
        expect(error.query).to eq(:missing?)
      end
    end

    it "reports an invalid policy constructor with the record index and query" do
      invalid_policy_class = Class.new do
        def initialize; end
      end

      expect do
        context.policy_permissions([post], %i[show?], policy_class: invalid_policy_class)
      end.to raise_error(Pundit::BatchAuthorizationError) do |error|
        expect(error.record_index).to eq(0)
        expect(error.query).to eq(:show?)
        expect(error.cause).to be_a(Pundit::InvalidConstructorError)
      end
    end

    it "reports an error raised inside a policy and allows a successful retry" do
      policy_class = Class.new(BasePolicy) do
        class << self
          attr_accessor :fails
        end

        def edit?
          false
        end

        def show?
          raise "temporary policy failure" if self.class.fails
          true
        end
      end
      policy_class.fails = true

      expect do
        context.policy_permissions([post], %i[edit? show?], policy_class: policy_class)
      end.to raise_error(Pundit::BatchAuthorizationError) do |error|
        expect(error.record_index).to eq(0)
        expect(error.query).to eq(:show?)
      end

      policy_class.fails = false

      expect(context.policy_permissions([post], %i[edit? show?], policy_class: policy_class))
        .to eq([[false, true]])
    end
  end
end
