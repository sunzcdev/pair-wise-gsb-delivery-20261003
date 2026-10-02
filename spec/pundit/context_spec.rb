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

    it "does not share policies between namespace chains" do
      store = {}
      plugin_context = described_class.new(
        user: user,
        policy_cache: Pundit::CacheStore::LegacyStore.new(store),
        policy_namespaces: [PolicyNamespaceSpec::Plugin, PolicyNamespaceSpec::Host]
      )
      host_context = described_class.new(
        user: user,
        policy_cache: Pundit::CacheStore::LegacyStore.new(store),
        policy_namespaces: [PolicyNamespaceSpec::Host]
      )

      plugin_policy = plugin_context.policy(article)

      host_policy = host_context.policy(article)

      expect(host_policy).not_to be(plugin_policy)
      expect(host_policy).to be_a(PolicyNamespaceSpec::Host::ArticlePolicy)
      expect(plugin_policy).to be_a(PolicyNamespaceSpec::Host::ArticlePolicy)
    end
  end

  describe "policy namespaces" do
    let(:namespaced_context) do
      described_class.new(user: user, policy_namespaces: [PolicyNamespaceSpec::Plugin, PolicyNamespaceSpec::Host])
    end

    it "uses the first namespace with a policy" do
      expect(namespaced_context.policy(post)).to be_a(PolicyNamespaceSpec::Plugin::PostPolicy)
    end

    it "falls back to the next namespace when no policy exists" do
      expect(namespaced_context.policy(article)).to be_a(PolicyNamespaceSpec::Host::ArticlePolicy)
    end

    it "uses the record namespace before the configured namespace chain" do
      expect(namespaced_context.policy([:project, Post])).to be_a(PolicyNamespaceSpec::Host::Project::PostPolicy)
    end

    it "uses an anonymous record policy without falling back" do
      anonymous_policy = Class.new(BasePolicy)
      record_class = Class.new do
        define_singleton_method(:policy_class) { anonymous_policy }
      end

      expect(namespaced_context.policy(record_class.new)).to be_a(anonymous_policy)
    end

    it "does not call later policies when the selected policy denies access" do
      expect(PolicyNamespaceSpec::Host::PostPolicy).not_to receive(:new)

      expect do
        namespaced_context.authorize(post, query: :destroy?, policy_class: nil)
      end.to raise_error(Pundit::NotAuthorizedError)
    end

    it "does not call later policies when the selected policy constructor fails" do
      record = :invalid_constructor_record

      expect(PolicyNamespaceSpec::Host::InvalidConstructorRecordPolicy).not_to receive(:new)

      expect do
        namespaced_context.policy!(record)
      end.to raise_error(Pundit::InvalidConstructorError)
    end

    it "does not call later policies when the selected policy lacks the query method" do
      record = :missing_query_record

      expect(PolicyNamespaceSpec::Host::MissingQueryRecordPolicy).not_to receive(:new)

      expect do
        namespaced_context.authorize(record, query: :show?, policy_class: nil)
      end.to raise_error(NoMethodError)
    end

    it "keeps a record policy class ahead of the configured namespace chain" do
      record = PolicyNamespaceSpec::OverrideRecord.new

      expect(namespaced_context.policy(record)).to be_an_instance_of(PolicyNamespaceSpec::ExplicitRecordPolicy)
    end

    it "keeps an explicit policy class ahead of the configured namespace chain" do
      policy = namespaced_context.policy!(post)

      expect(policy).to be_a(PolicyNamespaceSpec::Plugin::PostPolicy)
      expect do
        namespaced_context.authorize(post, query: :create?, policy_class: PublicationPolicy)
      end.not_to raise_error
    end

    it "uses the scope from the selected policy" do
      expect(namespaced_context.policy_scope(post)).to eq(:plugin_posts)
    end

    it "does not assemble a scope from a policy in another namespace" do
      expect(namespaced_context.policy_scope(comment)).to be_nil
    end

    it "raises without checking another namespace when the selected policy has no scope" do
      expect do
        namespaced_context.policy_scope!(comment)
      end.to raise_error(Pundit::NotDefinedError)
    end
  end
end

module PolicyNamespaceSpec
  module Plugin
    class ExplicitRecordPolicy < BasePolicy
    end

    class PostPolicy < BasePolicy
      class Scope < BaseScope
        def resolve
          :plugin_posts
        end
      end

      def destroy?
        false
      end
    end

    class InvalidConstructorRecordPolicy
      def initialize(user)
        @user = user
      end
    end

    class MissingQueryRecordPolicy < BasePolicy
    end

    class CommentPolicy < BasePolicy
    end
  end

  module Host
    module Project
      class PostPolicy < BasePolicy
      end
    end

    class PostPolicy < BasePolicy
      class Scope < BaseScope
        def resolve
          :host_posts
        end
      end
    end

    class ArticlePolicy < BasePolicy
    end

    class CommentPolicy < BasePolicy
      class Scope < BaseScope
        def resolve
          :host_comments
        end
      end
    end

    class InvalidConstructorRecordPolicy < BasePolicy
    end

    class MissingQueryRecordPolicy < BasePolicy
      def show?
        true
      end
    end
  end

  class ExplicitRecordPolicy < BasePolicy
  end

  class OverrideRecord
    def policy_class
      ExplicitRecordPolicy
    end
  end
end
