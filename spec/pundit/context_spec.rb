# frozen_string_literal: true

require "spec_helper"

module PluginPolicies
  class PostPolicy < BasePolicy
    class Scope < BasePolicy::BaseScope
      def resolve
        :plugin
      end
    end

    def update?
      false
    end

    def show?
      true
    end
  end

  class ArticlePolicy < BasePolicy
    def show?
      true
    end
  end

  class WikiPolicy < BasePolicy
    def initialize(user, record, extra)
      super(user, record)
    end
  end

  module Project
    class PostPolicy < BasePolicy
      class Scope < BasePolicy::BaseScope
        def resolve
          :plugin_project
        end
      end

      def show?
        true
      end
    end
  end
end

module HostPolicies
  class PostPolicy < BasePolicy
    class Scope < BasePolicy::BaseScope
      def resolve
        :host
      end
    end

    def update?
      true
    end

    def show?
      true
    end
  end

  class ArticlePolicy < BasePolicy
    class Scope < BasePolicy::BaseScope
      def resolve
        :host
      end
    end

    def show?
      true
    end
  end

  class WikiPolicy < BasePolicy
    def show?
      true
    end
  end
end

module EmptyPolicies
end

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

  describe "policy namespaces" do
    let(:plugin_context) { described_class.new(user: user, policy_namespaces: [PluginPolicies, HostPolicies]) }

    describe "#policy" do
      it "prefers the first namespace that defines the policy" do
        expect(plugin_context.policy(post)).to be_an_instance_of(PluginPolicies::PostPolicy)
      end

      it "falls back to the next namespace when the policy is not defined" do
        context = described_class.new(user: user, policy_namespaces: [EmptyPolicies, HostPolicies])

        expect(context.policy(post)).to be_an_instance_of(HostPolicies::PostPolicy)
      end

      it "does not resolve top-level policies through a namespace" do
        context = described_class.new(user: user, policy_namespaces: [EmptyPolicies, HostPolicies])

        expect(context.policy(post)).not_to be_an_instance_of(::PostPolicy)
      end

      it "falls back to the top-level policy when no namespace defines it" do
        expect(plugin_context.policy(comment)).to be_an_instance_of(::CommentPolicy)
      end

      it "prefers the policy_class of the record over the namespaces" do
        customer_post = Customer::Post.new(user)

        expect(plugin_context.policy(customer_post)).to be_an_instance_of(::PostPolicy)
      end

      it "finds namespaced policies for namespaced records" do
        context = described_class.new(user: user, policy_namespaces: [PluginPolicies])

        expect(context.policy([:project, post])).to be_an_instance_of(PluginPolicies::Project::PostPolicy)
      end

      it "falls back to the top-level namespace for namespaced records" do
        context = described_class.new(user: user, policy_namespaces: [HostPolicies])

        expect(context.policy([:project, post])).to be_an_instance_of(Project::PostPolicy)
      end

      it "returns nil for anonymous record types" do
        expect(plugin_context.policy(Class.new.new)).to be_nil
      end

      it "still finds the NilClassPolicy" do
        expect(plugin_context.policy(nil)).to be_an_instance_of(NilClassPolicy)
      end
    end

    describe "#policy!" do
      it "raises when no namespace defines the policy" do
        expect { plugin_context.policy!(Foo) }.to raise_error(Pundit::NotDefinedError)
      end

      it "raises for anonymous record types" do
        expect { plugin_context.policy!(Class.new.new) }.to raise_error(Pundit::NotDefinedError)
      end
    end

    describe "#authorize" do
      it "authorizes with the policy from the namespace" do
        expect(plugin_context.authorize(post, query: :show?, policy_class: nil)).to be(post)
      end

      it "authorizes namespaced records with the policy from the namespace" do
        context = described_class.new(user: user, policy_namespaces: [PluginPolicies])

        expect(context.authorize([:project, post], query: :show?, policy_class: nil)).to be(post)
      end

      it "prefers an explicitly passed policy class over the namespaces" do
        expect(plugin_context.authorize(post, query: :create?, policy_class: PublicationPolicy)).to be(post)
      end

      it "does not fall through to the next namespace when authorization is denied" do
        expect do
          expect do
            plugin_context.authorize(post, query: :update?, policy_class: nil)
          end.to raise_error(Pundit::NotAuthorizedError)
        end.to change { PluginPolicies::PostPolicy.instances }.by(1)
          .and not_change { HostPolicies::PostPolicy.instances }
      end

      it "does not fall through to the next namespace when the policy constructor is invalid" do
        expect do
          expect do
            plugin_context.authorize(Wiki.new, query: :show?, policy_class: nil)
          end.to raise_error(Pundit::InvalidConstructorError)
        end.to not_change { HostPolicies::WikiPolicy.instances }
      end

      it "does not fall through to the next namespace when the query is missing" do
        expect do
          expect do
            plugin_context.authorize(post, query: :destroy?, policy_class: nil)
          end.to raise_error(NoMethodError)
        end.to not_change { HostPolicies::PostPolicy.instances }
      end
    end

    describe "#policy_scope" do
      it "resolves the scope from the policy in the namespace" do
        expect(plugin_context.policy_scope(Post)).to eq(:plugin)
      end

      it "resolves namespaced scopes from the namespace" do
        context = described_class.new(user: user, policy_namespaces: [PluginPolicies])

        expect(context.policy_scope([:project, Post])).to eq(:plugin_project)
      end

      it "does not assemble a scope from another namespace when the selected policy has none" do
        expect(plugin_context.policy_scope(Article)).to be_nil
      end

      it "resolves the scope from the next namespace when it defines the policy" do
        context = described_class.new(user: user, policy_namespaces: [HostPolicies, PluginPolicies])

        expect(context.policy_scope(Article)).to eq(:host)
      end
    end

    describe "#policy_scope!" do
      it "raises when the selected policy has no scope, even if another namespace has one" do
        expect { plugin_context.policy_scope!(Article) }.to raise_error(Pundit::NotDefinedError)
      end
    end

    describe "policy cache" do
      it "caches policies per namespace chain" do
        store = {}
        cache = Pundit::CacheStore::LegacyStore.new(store)
        plugin_context = described_class.new(user: user, policy_cache: cache, policy_namespaces: [PluginPolicies])
        host_context = described_class.new(user: user, policy_cache: cache, policy_namespaces: [HostPolicies])
        default_context = described_class.new(user: user, policy_cache: cache)

        plugin_policy = plugin_context.policy(post)
        host_policy = host_context.policy(post)
        default_policy = default_context.policy(post)

        expect(plugin_policy).to be_an_instance_of(PluginPolicies::PostPolicy)
        expect(host_policy).to be_an_instance_of(HostPolicies::PostPolicy)
        expect(default_policy).to be_an_instance_of(::PostPolicy)
        expect(plugin_context.policy(post)).to be(plugin_policy)
        expect(host_context.policy(post)).to be(host_policy)
        expect(default_context.policy(post)).to be(default_policy)
        expect(store.keys).to contain_exactly(post, [post, [PluginPolicies]], [post, [HostPolicies]])
      end
    end
  end
end
