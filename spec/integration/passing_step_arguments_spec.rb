# frozen_string_literal: true

RSpec.describe "Passing additional arguments to step operations" do
  let(:call_transaction) { transaction.with_step_args(**step_options).call(input) }

  let(:transaction) {
    Class.new do
      include Dry::Transaction(container: Test::Container)

      map :process, with: :process
      try :validate, with: :validate, catch: Test::NotValidError
      tee :persist, with: :persist
    end.new
  }

  let(:input) { {"name" => "Jane", "email" => "jane@doe.com"} }

  before do
    Test::NotValidError = Class.new(StandardError)
    Test::DB = Array.new

    module Test
      Container = {
        process: -> input { {name: input["name"], email: input["email"]} },
        validate: -> input, allowed { input[:email].include?(allowed) ? input : raise(Test::NotValidError, "email not allowed") },
        persist: -> input { Test::DB << input and true }
      }.freeze
    end
  end

  context "required arguments provided" do
    let(:step_options) { {validate: ["doe.com"]} }

    it "passes the arguments and calls the operations successfully" do
      expect(call_transaction).to be_a Dry::Monads::Result::Success
    end
  end

  context "required arguments not provided" do
    let(:step_options) { {} }

    it "raises an ArgumentError" do
      expect { call_transaction }.to raise_error(ArgumentError)
    end
  end

  context "spurious arguments provided" do
    let(:step_options) { {validate: ["doe.com"], bogus: ["not matching any step"]} }

    it "raises an ArgumentError" do
      expect { call_transaction }.to raise_error(ArgumentError)
    end
  end
end

RSpec.describe "Passing keyword arguments to step operations" do
  let(:input) { {"name" => "Jane", "email" => "jane@doe.com"} }

  before do
    Test::NotValidError = Class.new(StandardError)
    Test::DB = Array.new
    Test::CALLED = Array.new
  end

  context "external callables" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        map :process, with: :process
        try :validate, with: :validate, catch: Test::NotValidError
        tee :persist, with: :persist
      end.new
    }

    before do
      module Test
        Container = {
          process: -> input {
            Test::CALLED << :process
            {name: input["name"], email: input["email"]}
          },
          validate: -> input, allowed: {
            Test::CALLED << :validate
            input[:email].include?(allowed) ? input : raise(Test::NotValidError, "email not allowed")
          },
          persist: -> input, suffix: nil {
            Test::CALLED << :persist
            Test::DB << (suffix ? input.merge(suffix: suffix) : input) and true
          }
        }.freeze
      end
    end

    it "passes keyword arguments to the operations" do
      result = transaction
        .with_step_args(validate: {allowed: "doe.com"}, persist: {suffix: "!"})
        .call(input)

      expect(result).to be_a Dry::Monads::Result::Success
      expect(Test::DB.last).to eq(name: "Jane", email: "jane@doe.com", suffix: "!")
    end

    it "preserves step arguments when with_step_args is chained" do
      result = transaction
        .with_step_args(validate: {allowed: "doe.com"})
        .with_step_args(persist: {suffix: "!"})
        .call(input)

      expect(result).to be_a Dry::Monads::Result::Success
      expect(Test::DB.last).to eq(name: "Jane", email: "jane@doe.com", suffix: "!")
    end

    context "required keyword argument not provided" do
      it "raises an ArgumentError naming the step before running any step" do
        expect {
          transaction.with_step_args(persist: {suffix: "!"}).call(input)
        }.to raise_error(ArgumentError, /missing keyword arguments :allowed for step \+validate\+/)

        expect(Test::CALLED).to be_empty
      end
    end

    context "unknown keyword arguments provided" do
      it "raises an ArgumentError naming the step before running any step" do
        expect {
          transaction.with_step_args(validate: {allowed: "doe.com", bogus: "x"}).call(input)
        }.to raise_error(ArgumentError, /unknown keyword arguments :bogus for step \+validate\+/)

        expect(Test::CALLED).to be_empty
      end
    end

    context "operation accepting a keyrest parameter" do
      let(:transaction) {
        Class.new do
          include Dry::Transaction(container: Test::KeyrestContainer)

          map :process, with: :process
          try :validate, with: :validate, catch: Test::NotValidError
          tee :persist, with: :persist
        end.new
      }

      before do
        module Test
          KeyrestContainer = {
            process: -> input {
              Test::CALLED << :process
              {name: input["name"], email: input["email"]}
            },
            validate: -> input, allowed:, **rest {
              Test::CALLED << :validate
              Test::DB << rest
              input[:email].include?(allowed) ? input : raise(Test::NotValidError, "email not allowed")
            },
            persist: -> input { Test::DB << input and true }
          }.freeze
        end
      end

      it "passes any keywords through" do
        result = transaction
          .with_step_args(validate: {allowed: "doe.com", extra: "yes"})
          .call(input)

        expect(result).to be_a Dry::Monads::Result::Success
        expect(Test::DB).to include({extra: "yes"})
      end
    end
  end

  context "internal methods" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction

        map :process
        tee :persist

        def process(input, prefix:)
          Test::CALLED << :process
          {name: "#{prefix} #{input["name"]}", email: input["email"]}
        end

        def persist(input, db:)
          Test::CALLED << :persist
          db << input
        end
      end.new
    }

    it "passes keyword arguments to instance methods" do
      db = []

      result = transaction
        .with_step_args(process: {prefix: "Ms."}, persist: {db: db})
        .call(input)

      expect(result).to be_a Dry::Monads::Result::Success
      expect(db).to eq([{name: "Ms. Jane", email: "jane@doe.com"}])
    end

    it "raises an ArgumentError naming the step when a required keyword is missing" do
      expect {
        transaction.with_step_args(persist: {db: []}).call(input)
      }.to raise_error(ArgumentError, /missing keyword arguments :prefix for step \+process\+/)

      expect(Test::CALLED).to be_empty
    end
  end

  context "around steps" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        around :transaction, with: :transaction
        step :work, with: :work
      end.new
    }

    before do
      module Test
        Container = {
          transaction: -> input, label:, &block {
            Test::CALLED << "before_#{label}"
            result = block.(Dry::Monads.Success(input))
            Test::CALLED << "after_#{label}"
            result
          },
          work: -> input, flag: {
            Test::CALLED << "work_#{flag}"
            Dry::Monads.Success(input)
          }
        }.freeze
      end
    end

    it "passes keyword arguments through the around adapter" do
      result = transaction
        .with_step_args(transaction: {label: "tx"}, work: {flag: "on"})
        .call(input)

      expect(result).to be_a Dry::Monads::Result::Success
      expect(Test::CALLED).to eq ["before_tx", "work_on", "after_tx"]
    end
  end

  context "positional and keyword arguments combined" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        map :greet, with: :greet
      end.new
    }

    before do
      module Test
        Container = {
          greet: -> input, greeting, punctuation: "." {
            "#{greeting}, #{input["name"]}#{punctuation}"
          }
        }.freeze
      end
    end

    it "passes both positional and keyword arguments" do
      result = transaction
        .with_step_args(greet: [["Hello"], {punctuation: "!"}])
        .call(input)

      expect(result.value!).to eq "Hello, Jane!"
    end

    it "applies keyword defaults when only positional arguments are given" do
      result = transaction
        .with_step_args(greet: ["Hi"])
        .call(input)

      expect(result.value!).to eq "Hi, Jane."
    end
  end

  context "explicit hash positional argument alongside a same-named keyword" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        map :configure, with: :configure
      end.new
    }

    before do
      module Test
        Container = {
          configure: -> input, options, mode: {
            {input: input, positional: options, keyword: mode}
          }
        }.freeze
      end
    end

    it "keeps the positional hash and the keyword separate" do
      result = transaction
        .with_step_args(configure: [[{mode: "positional"}], {mode: "keyword"}])
        .call("input")

      expect(result.value!).to eq(
        input: "input",
        positional: {mode: "positional"},
        keyword: "keyword"
      )
    end
  end

  context "invalid step arguments" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        map :process, with: :process
        map :validate, with: :validate
      end.new
    }

    before do
      module Test
        Container = {
          process: -> input { Test::CALLED << :process and input },
          validate: -> input, allowed { Test::CALLED << :validate and input }
        }.freeze
      end
    end

    it "raises an ArgumentError naming the step before running any step" do
      expect { transaction.call(input) }.to raise_error(
        ArgumentError, /not enough arguments supplied for step \+validate\+/
      )

      expect(Test::CALLED).to be_empty
    end

    it "rejects step arguments that are neither an Array nor a Hash" do
      expect {
        transaction.with_step_args(validate: "doe.com")
      }.to raise_error(ArgumentError, /step arguments for \+validate\+/)
    end
  end
end
