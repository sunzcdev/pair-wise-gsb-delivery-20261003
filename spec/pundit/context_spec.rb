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

    it "raises InvalidConstructorError when an explicit policy class has an incompatible constructor" do
      expect { authorize(post, query: :create?, policy_class: InvalidConstructorPolicy) }.to raise_error(
        Pundit::InvalidConstructorError,
        "Invalid #<InvalidConstructorPolicy> constructor is called. " \
        "Pundit calls it with (user, record), but its signature is (user, record, extra, optional = ?, *rest, keyword:)"
      )
    end

    it "propagates ArgumentError raised inside an explicit policy class constructor" do
      expect do
        authorize(post, query: :create?, policy_class: ConstructorContainsArgumentErrorPolicy)
      end.to raise_error(ArgumentError, 'invalid value for Integer(): "not a number"')
    end

    it "propagates ArgumentError raised inside an inferred policy constructor" do
      expect do
        authorize(ConstructorContainsArgumentError.new, query: :show?)
      end.to raise_error(ArgumentError, 'invalid value for Integer(): "not a number"')
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

    it "raises InvalidConstructorError with a diagnostic covering all parameter kinds" do
      expect { context.policy(InvalidConstructor.new) }.to raise_error(
        Pundit::InvalidConstructorError,
        "Invalid #<InvalidConstructorPolicy> constructor is called. " \
        "Pundit calls it with (user, record), but its signature is (user, record, extra, optional = ?, *rest, keyword:)"
      )
    end

    it "propagates ArgumentError raised inside the policy constructor with its original backtrace" do
      expect { context.policy(ConstructorContainsArgumentError.new) }
        .to raise_error(ArgumentError, 'invalid value for Integer(): "not a number"') { |error|
          expect(error.backtrace).to include(a_string_including("constructor_contains_argument_error_policy"))
        }
    end
  end

  describe "#policy!" do
    it "raises when the policy cannot be found" do
      expect { context.policy!(article) }.to raise_error(Pundit::NotDefinedError)
    end

    it "propagates ArgumentError raised inside the policy constructor" do
      expect { context.policy!(ConstructorContainsArgumentError.new) }
        .to raise_error(ArgumentError, 'invalid value for Integer(): "not a number"')
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

    it "raises InvalidConstructorError with a diagnostic when the scope constructor is invalid" do
      expect { context.policy_scope(InvalidConstructor) }.to raise_error(
        Pundit::InvalidConstructorError,
        "Invalid #<InvalidConstructorPolicy::Scope> constructor is called. " \
        "Pundit calls it with (user, record), but its signature is (user, scope, extra, optional = ?, *rest, keyword:)"
      )
    end

    it "propagates ArgumentError raised inside the scope constructor" do
      expect { context.policy_scope(ConstructorContainsArgumentError) }
        .to raise_error(ArgumentError, 'invalid value for Integer(): "not a number"')
    end
  end

  describe "#policy_scope!" do
    it "raises when the scope cannot be found" do
      expect { context.policy_scope!(Article) }.to raise_error(Pundit::NotDefinedError)
    end

    it "raises InvalidConstructorError when the scope constructor is invalid" do
      expect { context.policy_scope!(InvalidConstructor) }.to raise_error(Pundit::InvalidConstructorError)
    end

    it "propagates ArgumentError raised inside the scope constructor" do
      expect { context.policy_scope!(ConstructorContainsArgumentError) }
        .to raise_error(ArgumentError, 'invalid value for Integer(): "not a number"')
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

    it "does not cache failed instantiations with an invalid constructor" do
      store = {}
      context = described_class.new(user: user, policy_cache: Pundit::CacheStore::LegacyStore.new(store))
      record = Wiki.new

      expect { context.policy(record) }.to raise_error(Pundit::InvalidConstructorError)
      expect { context.policy(record) }.to raise_error(Pundit::InvalidConstructorError)
      expect(store).to be_empty
    end

    it "does not cache failed instantiations raising ArgumentError inside the constructor" do
      store = {}
      context = described_class.new(user: user, policy_cache: Pundit::CacheStore::LegacyStore.new(store))
      record = ConstructorContainsArgumentError.new

      expect { context.policy(record) }.to raise_error(ArgumentError)
      expect { context.policy(record) }.to raise_error(ArgumentError)
      expect(store).to be_empty
    end
  end
end
