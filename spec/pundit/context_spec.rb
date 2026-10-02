# frozen_string_literal: true

require "spec_helper"

RSpec.describe Pundit::Context do
  let(:user) { double }
  let(:post) { Post.new(user) }
  let(:comment) { Comment.new }
  let(:article) { Article.new }
  let(:decision_post) { DecisionPost.new(user) }
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

    context "when the policy returns a Decision" do
      it "returns the record when the decision is allowed" do
        expect(authorize(decision_post, query: :show?)).to be(decision_post)
      end

      it "does not allow a denied decision even though it is a truthy object" do
        expect { authorize(DecisionPost.new, query: :update?) }
          .to raise_error(Pundit::NotAuthorizedError)
      end

      it "exposes the decision with code and details on the error" do
        other_post = DecisionPost.new(double("other user"))

        expect do
          authorize(other_post, query: :update?)
        end.to raise_error(Pundit::NotAuthorizedError) do |error|
          expect(error.query).to eq(:update?)
          expect(error.record).to eq(other_post)
          expect(error.policy).to be_a(DecisionPostPolicy)
          expect(error.decision).to have_attributes(
            allowed: false,
            code: :not_owner
          )
          expect(error.decision.details).to eq(owner_id: other_post.user.object_id)
        end
      end

      it "does not put the details into the error message" do
        expect do
          authorize(decision_post, query: :destroy?)
        end.to raise_error(Pundit::NotAuthorizedError) do |error|
          expect(error.message).to eq(
            "not allowed to DecisionPostPolicy#destroy? this DecisionPost"
          )
          expect(error.message).not_to include("do not log me")
          expect(error.message).not_to include("secret")
        end
      end

      it "uses an explicit policy class returning a decision" do
        expect(authorize(post, query: :show?, policy_class: DecisionPostPolicy))
          .to be(post)
      end

      it "raises with the decision when an explicit policy class denies" do
        expect do
          authorize(post, query: :destroy?, policy_class: DecisionPostPolicy)
        end.to raise_error(Pundit::NotAuthorizedError) do |error|
          expect(error.policy).to be_a(DecisionPostPolicy)
          expect(error.decision).to have_attributes(allowed: false, code: :never_allowed)
        end
      end

      it "keeps raising for a policy method that returns plain false" do
        expect { authorize(decision_post, query: :archive?) }
          .to raise_error(Pundit::NotAuthorizedError) do |error|
            expect(error.decision).to be_nil
          end
      end

      it "calls the policy query exactly once" do
        policy = DecisionPostPolicy.new(user, decision_post)
        allow(context).to receive(:policy!).with(decision_post).and_return(policy)
        expect(policy).to receive(:show?).once.and_call_original

        authorize(decision_post, query: :show?)
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
