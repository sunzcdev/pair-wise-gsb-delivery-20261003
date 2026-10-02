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

    context "when the policy returns a Pundit::Decision" do
      it "returns the record when the decision is allowed" do
        expect(authorize(post, query: :show?, policy_class: DecisionPolicy)).to be(post)
      end

      it "raises when the decision is denied, even though it is a non-nil object" do
        expect do
          authorize(post, query: :update?, policy_class: DecisionPolicy)
        end.to raise_error(Pundit::NotAuthorizedError)
      end

      it "exposes the decision on the error without leaking details into the message" do
        expect do
          authorize(post, query: :update?, policy_class: DecisionPolicy)
        end.to raise_error(Pundit::NotAuthorizedError) do |error|
          expect(error.query).to eq(:update?)
          expect(error.record).to eq(post)
          expect(error.policy).to be_a(DecisionPolicy)
          expect(error.decision).to be_a(Pundit::Decision).and be_denied
          expect(error.decision.code).to eq(:not_owner)
          expect(error.message).not_to include(error.decision.details.to_s)
          expect(error.message).to eq("not allowed to DecisionPolicy#update? this Post")
        end
      end

      it "calls the policy query method only once" do
        policy_class = Class.new(BasePolicy) do
          @calls = 0

          class << self
            attr_accessor :calls
          end

          def update?
            self.class.calls += 1
            Pundit::Decision.deny
          end
        end

        expect do
          authorize(post, query: :update?, policy_class: policy_class)
        end.to raise_error(Pundit::NotAuthorizedError)
        expect(policy_class.calls).to eq(1)
      end

      it "treats a plain false from the same policy like before" do
        expect do
          authorize(post, query: :destroy?, policy_class: DecisionPolicy)
        end.to raise_error(Pundit::NotAuthorizedError) do |error|
          expect(error.decision).to be_nil
          expect(error.message).to eq("not allowed to DecisionPolicy#destroy? this Post")
        end
      end
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
end
