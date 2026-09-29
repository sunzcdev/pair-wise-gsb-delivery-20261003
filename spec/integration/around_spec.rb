# frozen_string_literal: true

RSpec.describe "around steps" do
  include_context "db transactions"

  include Dry::Monads[:result]

  before do
    container.instance_exec do
      register :validate, -> input {
        if input[:account][:balance] >= 0
          Success(input)
        else
          Failure(input)
        end
      }

      register :persist_user do |user:, **other|
        self[:database] << [:user, user]
        Success(other)
      end

      register :persist_account do |account:|
        self[:database] << [:account, account]
        Success(true)
      end
    end
  end

  let(:transaction) do
    Class.new do
      include Dry::Transaction(container: Test::Container)

      around :transaction, with: :transaction
      step :validate, with: :validate
      step :persist_user, with: :persist_user
      step :persist_account, with: :persist_account
      step :finalize, with: :finalize
    end
  end

  let(:input) { {user: {name: "Jane"}, account: {balance: 0}} }

  it "starts a transaction" do
    called = false

    finalize = -> x do
      called = true
      expect(database).to(be_in_transaction)
      Success(x)
    end

    result = transaction.new(finalize: finalize).call(input)
    expect(called).to be true
    expect(result).to eql(Success(true))
  end

  it "commits transactions" do
    transaction.new(finalize: -> x { Success(x) }).call(input)

    expect(database).to be_committed
    expect(database).not_to be_rolled_back
    expect(database).not_to be_in_transaction
    expect(database).to eql([[:user, name: "Jane"],
                             [:account, balance: 0]])
  end

  it "rolls back transactions on failure" do
    transaction.new(finalize: -> x { Failure(x) }).call(input)

    expect(database).to be_rolled_back
    expect(database).not_to be_in_transaction
    expect(database).not_to be_committed
    expect(database).to be_empty
  end

  it "rolls back transaction on exception" do
    uncaught = Class.new(StandardError)

    expect {
      transaction.new(finalize: -> x { raise uncaught }).call(input)
    }.to raise_error(uncaught)

    expect(database).to be_rolled_back
    expect(database).not_to be_in_transaction
    expect(database).not_to be_committed
    expect(database).to be_empty
  end

  it "supports matching on nested failures" do
    invalid_input = input.merge(account: {balance: -10})

    failed_input = nil

    transaction.new(finalize: -> x { Success(x) }).call(invalid_input) do |m|
      m.success { |v| v }
      m.failure(:validate) { |input| failed_input = input }
    end

    expect(failed_input).to eq invalid_input
  end

  describe "subscribing to events" do
    let(:subscriber) {
      Class.new do
        attr_reader :user_persisted

        def on_step_succeeded(event)
          @user_persisted = true if event[:step_name] == :persist_user
        end
      end.new
    }

    it "supports subscribing to events from nested steps" do
      trans = transaction.new(finalize: -> x { Success(x) })
      trans.subscribe(subscriber)
      trans.call(input)

      expect(subscriber.user_persisted).to be_truthy
    end
  end

  describe "strict continuations" do
    let(:transaction) do
      Class.new do
        include Dry::Transaction

        around :outer, strict: true
        around :inner, strict: true
        step :final
      end
    end

    let(:step_calls) { Hash.new(0) }

    let(:counter_mutex) { Mutex.new }

    let(:final_operation) do
      -> input {
        counter_mutex.synchronize { step_calls[:final] += 1 }
        Success(input)
      }
    end

    def operations(overrides = {})
      {
        outer: -> (input, &block) { block.(Success(input)) },
        inner: -> (input, &block) { block.(Success(input)) },
        final: final_operation
      }.merge(overrides)
    end

    it "allows nested around steps" do
      outer = -> (input, &block) {
        step_calls[:outer] += 1
        block.(Success(input))
      }
      inner = -> (input, &block) {
        step_calls[:inner] += 1
        block.(Success(input))
      }

      result = transaction.new(
        **operations(outer: outer, inner: inner)
      ).call(:ok)

      expect(result).to eql(Success(:ok))
      expect(step_calls).to eql(outer: 1, inner: 1, final: 1)
    end

    it "invalidates a continuation when an operation exits with an exception" do
      failure = Class.new(StandardError)
      stale_continuation = nil

      inner = -> (input, &block) {
        stale_continuation = block
        block.(Success(input))
        raise failure
      }

      expect {
        transaction.new(**operations(inner: inner)).call(:ok)
      }.to raise_error(failure)

      expect {
        stale_continuation.(Success(:after))
      }.to raise_error(
        Dry::Transaction::InvalidContinuationError,
        /\Astep \+inner\+ continuation is no longer available \(invocation #\d+\)\z/
      )
      expect(step_calls).to eql(final: 1)
    end

    it "rejects continuations reused by another concurrent call" do
      state_mutex = Mutex.new
      synchronize_mutex = Mutex.new
      ready = ConditionVariable.new
      rejected = ConditionVariable.new
      ready_count = 0
      rejected_count = 0
      continuations = {}
      events = Hash.new { |hash, key| hash[key] = Queue.new }

      around_operation = -> (input, &block) {
        name = Thread.current[:around_name]
        state_mutex.synchronize { continuations[name] = block }

        synchronize_mutex.synchronize do
          ready_count += 1
          ready.broadcast if ready_count == 2
          ready.wait(synchronize_mutex) while ready_count < 2
        end

        other_continuation = state_mutex.synchronize do
          continuations.fetch(other_name(name))
        end

        begin
          other_continuation.(Success(input))
        rescue Dry::Transaction::InvalidContinuationError => exception
          events[name] << [:rejected, exception.message]
        end

        synchronize_mutex.synchronize do
          rejected_count += 1
          rejected.broadcast if rejected_count == 2
          rejected.wait(synchronize_mutex) while rejected_count < 2
        end

        block.(Success(input))
      }

      concurrent_transaction = transaction.new(
        **operations(outer: around_operation)
      )

      first_thread = Thread.new do
        Thread.current[:around_name] = :first
        concurrent_transaction.call(:first)
      end

      second_thread = Thread.new do
        Thread.current[:around_name] = :second
        concurrent_transaction.call(:second)
      end

      first_result = first_thread.value
      second_result = second_thread.value
      first_error = events[:first].pop
      second_error = events[:second].pop

      expect(first_result).to eql(Success(:first))
      expect(second_result).to eql(Success(:second))
      expect(first_error[0]).to eql(:rejected)
      expect(second_error[0]).to eql(:rejected)
      expect(first_error[1]).to match(
        /\Astep \+outer\+ continuation belongs to invocation #\d+, not invocation #\d+\z/
      )
      expect(second_error[1]).to match(
        /\Astep \+outer\+ continuation belongs to invocation #\d+, not invocation #\d+\z/
      )
      expect(step_calls).to eql(final: 2)
    end

    def other_name(name)
      name == :first ? :second : :first
    end
  end
end
