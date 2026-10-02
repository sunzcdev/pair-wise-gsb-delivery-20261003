# frozen_string_literal: true

require "spec_helper"

RSpec.describe "Pundit batch permissions" do
  let(:user) { double("user") }
  let(:context) { Pundit::Context.new(user: user) }

  let(:mine) { Document.new(id: 1, owner: user) }
  let(:other) { Document.new(id: 2, owner: :someone_else) }
  let(:comment) { Comment.new }

  before { DocumentPolicy.reset_calls! }

  describe Pundit::Context, "#permissions" do
    it "returns a records x queries matrix keeping the given order" do
      result = context.permissions([mine, other], %i[show? edit? archive?])

      expect(result).to eq([
        [true, true, true],
        [true, false, false]
      ])
    end

    it "always returns real booleans" do
      truthy_policy = Class.new(BasePolicy) do
        def show?
          "yes"
        end
      end
      falsy_policy = Class.new(BasePolicy) do
        def show?
          nil
        end
      end

      truthy = context.permissions([mine], [:show?], policy_class: truthy_policy)
      falsy = context.permissions([other], [:show?], policy_class: falsy_policy)

      expect(truthy).to eq([[true]])
      expect(falsy).to eq([[false]])
      expect(truthy.dig(0, 0)).to be(true)
      expect(falsy.dig(0, 0)).to be(false)
    end

    it "keeps ordinary denials as false without interrupting the other checks" do
      denied = Document.new(id: 9, owner: :nobody, published: false)

      result = context.permissions([mine, denied, other], %i[show? edit? archive?])

      expect(result).to eq([
        [true, true, true],
        [false, false, false],
        [true, false, false]
      ])
    end

    context "with duplicated input" do
      it "preserves the position of repeated records" do
        result = context.permissions([mine, other, mine], %i[show? edit?])

        expect(result).to eq([
          [true, true],
          [true, false],
          [true, true]
        ])
      end

      it "evaluates the same object and query only once" do
        context.permissions([mine, other, mine, mine], %i[show? edit? show?])

        expect(DocumentPolicy.call_count(:show?, mine)).to eq(1)
        expect(DocumentPolicy.call_count(:edit?, mine)).to eq(1)
        expect(DocumentPolicy.call_count(:show?, other)).to eq(1)
        expect(DocumentPolicy.call_count(:edit?, other)).to eq(1)
      end

      it "normalizes string and symbol queries into the same identity" do
        result = context.permissions([mine], ["show?", :show?])

        expect(result).to eq([[true, true]])
        expect(DocumentPolicy.call_count(:show?, mine)).to eq(1)
      end

      it "reuses one policy instance for the same object" do
        expect do
          context.permissions([mine, mine], %i[show? edit? archive?])
        end.to change { DocumentPolicy.instances }.by(1)
      end
    end

    context "with records that share an id but are different objects" do
      it "judges each object separately and keeps both positions" do
        first = Document.new(id: 7, owner: user)
        second = Document.new(id: 7, owner: :someone_else)

        expect(first).to eq(second)

        result = context.permissions([first, second], %i[show? edit? archive?])

        expect(result).to eq([
          [true, true, true],
          [true, false, false]
        ])
        expect(DocumentPolicy.call_count(:edit?, first)).to eq(1)
        expect(DocumentPolicy.call_count(:edit?, second)).to eq(1)
      end

      it "builds a separate policy instance per object" do
        first = Document.new(id: 7, owner: user)
        second = Document.new(id: 7, owner: user)

        expect do
          context.permissions([first, second], [:show?])
        end.to change { DocumentPolicy.instances }.by(2)
      end
    end

    context "with namespaced records" do
      it "checks the namespaced policy" do
        result = context.permissions(
          [[:project, comment], [:project, :admin, comment]],
          %i[update?]
        )

        expect(result).to eq([[true], [true]])
      end

      it "uses one policy instance for a repeated namespaced record" do
        records = [[:project, :admin, comment], [:project, :admin, comment]]

        expect do
          context.permissions(records, %i[update? destroy?])
        end.to change { Project::Admin::CommentPolicy.instances }.by(1)
      end

      it "treats different namespaces around the same object as distinct" do
        expect do
          context.permissions(
            [[:project, comment], [:project, :admin, comment]],
            %i[update?]
          )
        end.to change {
          Project::CommentPolicy.instances + Project::Admin::CommentPolicy.instances
        }.by(2)
      end

      it "reports the model record and its index when a query is missing" do
        expect do
          context.permissions([mine, [:project, comment]], %i[archive?])
        end.to raise_error(Pundit::BatchAuthorizationError) do |error|
          expect(error.index).to eq(1)
          expect(error.query).to eq(:archive?)
          expect(error.record).to equal(comment)
          expect(error.cause).to be_a(NoMethodError)
        end
      end
    end

    context "with an explicit policy class" do
      it "uses the given policy class for every record" do
        result = context.permissions([comment], %i[create?], policy_class: PublicationPolicy)

        expect(result).to eq([[true]])
      end

      it "deduplicates repeated records under the same policy class" do
        expect do
          context.permissions([comment, comment], %i[create?], policy_class: PublicationPolicy)
        end.to change { PublicationPolicy.instances }.by(1)
      end

      it "instantiates the policy with the model of a namespaced record" do
        expect(PublicationPolicy).to receive(:new).with(user, comment).and_call_original

        context.permissions([[:project, comment]], %i[create?], policy_class: PublicationPolicy)
      end
    end

    context "with empty batches" do
      it "returns an empty matrix for no records" do
        expect(context.permissions([], %i[show?])).to eq([])
      end

      it "returns one empty row per record for no queries" do
        expect(context.permissions([mine], [])).to eq([[]])
      end

      it "handles an entirely empty batch" do
        expect(context.permissions([], [])).to eq([])
      end
    end

    context "across separate calls" do
      it "does not reuse results after the object's state changes" do
        document = Document.new(id: 1, owner: user, published: true)

        expect(context.permissions([document], %i[show?])).to eq([[true]])

        document.published = false

        expect(context.permissions([document], %i[show?])).to eq([[false]])
        expect(DocumentPolicy.call_count(:show?, document)).to eq(2)
      end

      it "builds fresh policy instances for every batch" do
        document = Document.new(id: 1, owner: user)

        expect do
          context.permissions([document], %i[show?])
          context.permissions([document], %i[show?])
        end.to change { DocumentPolicy.instances }.by(2)
      end

      it "can be called again after a previous batch failed midway" do
        documents = [mine, Article.new]

        expect do
          context.permissions(documents, %i[show?])
        end.to raise_error(Pundit::BatchAuthorizationError) do |error|
          expect(error.index).to eq(1)
          expect(error.query).to eq(:show?)
          expect(error.record).to be_a(Article)
        end

        documents[1] = other

        expect(context.permissions(documents, %i[show? edit?])).to eq([
          [true, true],
          [true, false]
        ])
        expect(DocumentPolicy.call_count(:show?, mine)).to eq(2)
      end
    end

    context "on programmer errors" do
      it "wraps a missing policy with the record index and query name" do
        expect do
          context.permissions([mine, Article.new, other], %i[edit?])
        end.to raise_error(Pundit::BatchAuthorizationError) do |error|
          expect(error.index).to eq(1)
          expect(error.query).to eq(:edit?)
          expect(error.record).to be_a(Article)
          expect(error.cause).to be_a(Pundit::NotDefinedError)
          expect(error.message).to include("index 1").and include("edit?")
        end
      end

      it "wraps a missing query method instead of treating it as false" do
        expect do
          context.permissions([Blog.new], %i[show?])
        end.to raise_error(Pundit::BatchAuthorizationError) do |error|
          expect(error.index).to eq(0)
          expect(error.query).to eq(:show?)
          expect(error.cause).to be_a(NoMethodError)
        end
      end

      it "wraps an error raised from inside the policy" do
        expect do
          context.permissions([Explosion.new], %i[show?])
        end.to raise_error(Pundit::BatchAuthorizationError) do |error|
          expect(error.index).to eq(0)
          expect(error.query).to eq(:show?)
          expect(error.record).to be_a(Explosion)
          expect(error.cause).to be_a(RuntimeError)
          expect(error.cause.message).to eq("boom")
        end
      end

      it "wraps an invalid policy constructor" do
        expect do
          context.permissions([Wiki.new], %i[show?])
        end.to raise_error(Pundit::BatchAuthorizationError) do |error|
          expect(error.cause).to be_a(Pundit::InvalidConstructorError)
        end
      end
    end
  end

  describe Pundit, ".permissions" do
    it "delegates to a context and returns the matrix" do
      result = Pundit.permissions(user, [mine, other], %i[show? edit? archive?])

      expect(result).to eq([
        [true, true, true],
        [true, false, false]
      ])
    end

    it "accepts an explicit policy class" do
      expect(Pundit.permissions(user, [comment], %i[create?], policy_class: PublicationPolicy))
        .to eq([[true]])
    end

    it "raises batch errors with their index and query" do
      expect do
        Pundit.permissions(user, [Article.new], %i[show?])
      end.to raise_error(Pundit::BatchAuthorizationError) do |error|
        expect(error.index).to eq(0)
        expect(error.query).to eq(:show?)
      end
    end
  end

  describe Pundit::Authorization, "#pundit_permissions" do
    let(:controller) { BatchController.new(user) }

    it "returns the matrix for the current user" do
      expect(controller.pundit_permissions([mine, other], %i[show? edit? archive?]))
        .to eq([
          [true, true, true],
          [true, false, false]
        ])
    end

    it "accepts an explicit policy class" do
      expect(controller.pundit_permissions([comment], %i[create?], policy_class: PublicationPolicy))
        .to eq([[true]])
    end
  end

  # Minimal controller-like host, without pulling in Action Controller.
  class BatchController
    include Pundit::Authorization

    attr_reader :current_user

    def initialize(current_user)
      @current_user = current_user
    end

    public(*Pundit::Authorization.protected_instance_methods)
  end
end
