# frozen_string_literal: true

RSpec.describe "Nested transaction failure addresses" do
  include Dry::Monads[:result]

  let(:error_class) { Class.new(StandardError) }

  # Two subflows, each defining their own +validate+ step
  let(:create_user_flow) do
    Class.new do
      include Dry::Transaction

      step :validate

      def validate(input)
        input[:email].to_s.empty? ? Failure(:email_required) : Success(input)
      end
    end.new
  end

  let(:update_order_flow) do
    Class.new do
      include Dry::Transaction

      step :validate

      def validate(input)
        input[:total].to_i <= 0 ? Failure(:invalid_total) : Success(input)
      end
    end.new
  end

  let(:transaction) do
    Class.new do
      include Dry::Transaction

      step :create_user
      step :update_order
    end.new(create_user: create_user_flow, update_order: update_order_flow)
  end

  context "with two subflows failing at steps with the same name" do
    it "matches a plain step name against the leaf step of either subflow" do
      matched = []

      transaction.call(email: "", total: 100) do |m|
        m.success {}
        m.failure(:validate) { |value| matched << value }
      end

      transaction.call(email: "jane@doe.com", total: 0) do |m|
        m.success {}
        m.failure(:validate) { |value| matched << value }
      end

      # The leaf step name alone cannot tell the two subflows apart
      expect(matched).to eq %i[email_required invalid_total]
    end

    it "matches the full failure address to tell the subflows apart" do
      matched = []

      transaction.call(email: "", total: 100) do |m|
        m.success {}
        m.failure(%i[create_user validate]) { |value| matched << [:create_user, value] }
        m.failure(%i[update_order validate]) { |value| matched << [:update_order, value] }
        m.failure {}
      end

      expect(matched).to eq [[:create_user, :email_required]]
    end

    it "matches a parent-level address prefix against any failure beneath it" do
      matched = nil

      transaction.call(email: "jane@doe.com", total: 0) do |m|
        m.success {}
        m.failure([:update_order]) { |value| matched = value }
        m.failure {}
      end

      expect(matched).to eq :invalid_total
    end

    it "evaluates overlapping cases in definition order, first match wins" do
      matched = []

      transaction.call(email: "", total: 100) do |m|
        m.success {}
        m.failure([:create_user]) { matched << :parent_scope }
        m.failure(%i[create_user validate]) { matched << :full_address }
        m.failure {}
      end

      expect(matched).to eq [:parent_scope]
    end

    it "prefers the full address when it is defined before the broader scope" do
      matched = []

      transaction.call(email: "", total: 100) do |m|
        m.success {}
        m.failure(%i[create_user validate]) { matched << :full_address }
        m.failure([:create_user]) { matched << :parent_scope }
        m.failure {}
      end

      expect(matched).to eq [:full_address]
    end

    it "still returns the original failure value when called without a block" do
      expect(transaction.call(email: "", total: 100).failure).to eq :email_required
    end
  end

  context "with multiple levels of wrapping" do
    let(:innermost) do
      error = error_class

      Class.new do
        include Dry::Transaction

        try :validate, catch: error

        define_method(:validate) do |_input|
          raise error, "email required"
        end
      end.new
    end

    let(:middle) do
      Class.new do
        include Dry::Transaction

        step :payment
      end.new(payment: innermost)
    end

    let(:outer) do
      Class.new do
        include Dry::Transaction

        step :checkout
      end.new(checkout: middle)
    end

    it "composes the full address across every level of nesting" do
      failure = outer.stack.(Success({})).failure

      expect(failure.path).to eq %i[checkout payment validate]
      expect(failure.step.name).to eq :validate
    end

    it "preserves the original exception as the failure value" do
      failure = outer.stack.(Success({})).failure

      expect(failure.value).to be_a(error_class)
      expect(failure.value.message).to eq "email required"
    end

    it "matches the full address through the result matcher" do
      matched = nil

      outer.call({}) do |m|
        m.success {}
        m.failure(%i[checkout payment validate]) { |value| matched = value }
        m.failure {}
      end

      expect(matched).to be_a(error_class)
    end

    it "matches a mid-level parent scope through the result matcher" do
      matched = nil

      outer.call({}) do |m|
        m.success {}
        m.failure(%i[checkout payment]) { |value| matched = value }
        m.failure {}
      end

      expect(matched).to be_a(error_class)
    end

    it "still unwraps the original exception when called without a block" do
      expect(outer.call({}).failure).to be_a(error_class)
    end
  end

  context "when calling a wrapped transaction directly" do
    let(:innermost) do
      error = error_class

      Class.new do
        include Dry::Transaction

        try :validate, catch: error

        define_method(:validate) do |_input|
          raise error, "email required"
        end
      end.new
    end

    let(:middle) do
      Class.new do
        include Dry::Transaction

        step :payment
      end.new(payment: innermost)
    end

    it "keeps the leaf address available to the matcher" do
      matched = nil

      innermost.call({}) do |m|
        m.success {}
        m.failure([:validate]) { |value| matched = value }
        m.failure {}
      end

      expect(matched).to be_a(error_class)
    end

    it "keeps the composed address when a wrapped transaction is called directly" do
      failure = middle.stack.(Success({})).failure

      expect(failure.path).to eq %i[payment validate]
      expect(failure.value).to be_a(error_class)
    end

    it "still unwraps the failure value when called without a block" do
      expect(middle.call({}).failure).to be_a(error_class)
    end
  end
end
