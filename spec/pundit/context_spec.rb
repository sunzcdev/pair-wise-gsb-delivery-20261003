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

    it "raises an invalid constructor error for an incompatible explicit policy class" do
      expect do
        authorize(post, query: :show?, policy_class: BadArityPolicy)
      end.to raise_error(Pundit::InvalidConstructorError, /#<BadArityPolicy>/)
    end

    it "lets an ArgumentError from an explicit policy class constructor bubble up" do
      expect do
        authorize(RaisesArgumentError.new, query: :show?, policy_class: RaisesArgumentErrorPolicy)
      end.to raise_error(ArgumentError, /invalid value for Integer/)
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

    it "lets an ArgumentError raised inside a valid constructor bubble up" do
      expect do
        context.policy(RaisesArgumentError.new)
      end.to raise_error(ArgumentError, /invalid value for Integer/) { |error|
        expect(error).not_to be_a(Pundit::InvalidConstructorError)
        expect(error.backtrace.join("\n")).to include("raises_argument_error_policy.rb")
      }
    end
  end

  describe "#policy!" do
    it "raises when the policy cannot be found" do
      expect { context.policy!(article) }.to raise_error(Pundit::NotDefinedError)
    end

    it "lets an ArgumentError raised inside a valid constructor bubble up" do
      expect { context.policy!(RaisesArgumentError.new) }.to raise_error(ArgumentError, /invalid value for Integer/)
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

    it "lets an ArgumentError raised inside a valid scope constructor bubble up" do
      expect do
        context.policy_scope(RaisesArgumentError)
      end.to raise_error(ArgumentError, /invalid value for Integer/)
    end
  end

  describe "#policy_scope!" do
    it "raises when the scope cannot be found" do
      expect { context.policy_scope!(Article) }.to raise_error(Pundit::NotDefinedError)
    end

    it "lets an ArgumentError raised inside a valid scope constructor bubble up" do
      expect do
        context.policy_scope!(RaisesArgumentError)
      end.to raise_error(ArgumentError, /invalid value for Integer/)
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

    it "does not cache a policy with an invalid constructor" do
      store = {}
      context = described_class.new(user: user, policy_cache: Pundit::CacheStore::LegacyStore.new(store))

      expect do
        context.policy(BadArity.new)
      end.to raise_error(Pundit::InvalidConstructorError)

      expect(store).to be_empty
    end

    it "does not cache a policy whose constructor raised an ArgumentError" do
      store = {}
      context = described_class.new(user: user, policy_cache: Pundit::CacheStore::LegacyStore.new(store))

      expect do
        context.policy(RaisesArgumentError.new)
      end.to raise_error(ArgumentError, /invalid value for Integer/)

      expect(store).to be_empty
    end
  end
end
