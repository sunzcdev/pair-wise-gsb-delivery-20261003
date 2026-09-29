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
  end

  context "with external operations" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        map :process, with: :process
        try :validate, with: :validate, catch: Test::NotValidError
        tee :persist, with: :persist
      end.new
    }

    before do
      Test.const_set(:Container, {
        process: -> input { {name: input["name"], email: input["email"]} },
        validate: -> input, prefix, allowed: {
          input[:email] == "#{prefix}@#{allowed}" ? input : raise(Test::NotValidError, "email not allowed")
        },
        persist: -> input, tag { Test::DB << [tag, input] and true }
      }.freeze)
    end

    it "passes positional and keyword arguments to the operations" do
      result = transaction
        .with_step_args(
          validate: {args: ["jane"], kwargs: {allowed: "doe.com"}},
          persist: {args: ["user"]}
        )
        .call(input)

      expect(result).to be_a Dry::Monads::Result::Success
      expect(Test::DB).to eql [["user", {name: "Jane", email: "jane@doe.com"}]]
    end

    context "with keyword arguments only" do
      before do
        Test.send(:remove_const, :Container)
        Test.const_set(:Container, {
          process: -> input { {name: input["name"], email: input["email"]} },
          validate: -> input, allowed: {
            input[:email].end_with?(allowed) ? input : raise(Test::NotValidError, "email not allowed")
          },
          persist: -> input { Test::DB << input and true }
        }.freeze)
      end

      it "passes keyword arguments only" do
        result = transaction
          .with_step_args(validate: {kwargs: {allowed: "doe.com"}})
          .call(input)

        expect(result).to be_a Dry::Monads::Result::Success
      end
    end

    it "preserves arguments from a previous with_step_args call" do
      result = transaction
        .with_step_args(validate: {args: ["jane"], kwargs: {allowed: "doe.com"}})
        .with_step_args(persist: {args: ["user"]})
        .call(input)

      expect(result).to be_a Dry::Monads::Result::Success
      expect(Test::DB).to eql [["user", {name: "Jane", email: "jane@doe.com"}]]
    end

    context "with a trailing hash in the legacy array form" do
      before do
        Test.send(:remove_const, :Container)
        Test.const_set(:Container, {
          process: -> input { {name: input["name"], email: input["email"]} },
          validate: -> input, allowed: {
            input[:email].end_with?(allowed) ? input : raise(Test::NotValidError, "email not allowed")
          },
          persist: -> input { Test::DB << input and true }
        }.freeze)
      end

      it "keeps treating the trailing hash as keyword arguments" do
        result = transaction
          .with_step_args(validate: [{allowed: "doe.com"}])
          .call(input)

        expect(result).to be_a Dry::Monads::Result::Success
      end
    end

    context "with an explicit hash positional argument and a keyword of the same name" do
      before do
        Test.send(:remove_const, :Container)
        Test.const_set(:Container, {
          process: -> input { {name: input["name"], email: input["email"]} },
          validate: -> input, options, name: { [options[:name], name] },
          persist: -> input { Test::DB << input and true }
        }.freeze)
      end

      it "passes the hash positionally and the keyword separately" do
        result = transaction
          .with_step_args(validate: {args: [{name: "positional"}], kwargs: {name: "keyword"}})
          .call(input)

        expect(result).to be_a Dry::Monads::Result::Success
        expect(result.value!).to eql ["positional", "keyword"]
      end
    end
  end

  context "with internal methods" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction

        step :validate

        def validate(input, allowed:)
          input["email"].end_with?(allowed) ? Success(input) : Failure(:not_allowed)
        end
      end.new
    }

    it "passes keyword arguments to the method" do
      result = transaction
        .with_step_args(validate: {kwargs: {allowed: "doe.com"}})
        .call(input)

      expect(result).to be_a Dry::Monads::Result::Success
    end
  end

  context "with around steps" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction

        around :wrap, with: :wrapper
        map :work

        def work(input, upcase: false)
          upcase ? input.upcase : input
        end
      end.new(wrap: -> input, tag:, &block { block.(Dry::Monads::Success("[#{tag}] #{input}")) })
    }

    it "passes keyword arguments through the around adapter and nested steps" do
      result = transaction
        .with_step_args(
          wrap: {kwargs: {tag: "wrapped"}},
          work: {kwargs: {upcase: true}}
        )
        .call("jane")

      expect(result.value!).to eql "[WRAPPED] JANE"
    end
  end

  context "with invalid step argument descriptors" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction

        map :process

        def process(input)
          input
        end
      end.new
    }

    it "raises an ArgumentError for unknown keys in the hash form" do
      expect {
        transaction.with_step_args(process: {arguments: ["x"]})
      }.to raise_error(ArgumentError, /invalid keys :arguments in arguments for step \+process\+/)
    end

    it "raises an ArgumentError for non-array, non-hash arguments" do
      expect {
        transaction.with_step_args(process: "x")
      }.to raise_error(ArgumentError, /invalid arguments for step \+process\+/)
    end
  end

  context "validating step arguments against operation signatures" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        map :process, with: :process
        try :validate, with: :validate, catch: Test::NotValidError
        tee :persist, with: :persist
      end.new
    }

    before do
      Test.const_set(:Container, {
        process: -> input { Test::DB << :processed and {name: input["name"], email: input["email"]} },
        validate: -> input, allowed:, strict: false {
          input[:email].end_with?(allowed) ? input : raise(Test::NotValidError, "email not allowed")
        },
        persist: -> input { Test::DB << input and true }
      }.freeze)
    end

    it "raises an ArgumentError naming the step when a required keyword is missing" do
      expect {
        transaction.with_step_args(validate: {kwargs: {strict: true}}).call(input)
      }.to raise_error(ArgumentError, "missing keyword arguments :allowed for step +validate+")
    end

    it "raises an ArgumentError naming the step when unknown keywords are given" do
      expect {
        transaction.with_step_args(validate: {kwargs: {allowed: "doe.com", bogus: 1}}).call(input)
      }.to raise_error(ArgumentError, "unknown keyword arguments :bogus for step +validate+")
    end

    it "raises an ArgumentError naming the step when too many positional arguments are given" do
      expect {
        transaction.with_step_args(validate: {args: ["a", "b"], kwargs: {allowed: "doe.com"}}).call(input)
      }.to raise_error(ArgumentError, "too many arguments supplied for step +validate+")
    end

    it "raises an ArgumentError naming the step when required positional arguments are missing" do
      expect {
        transaction.with_step_args(validate: {kwargs: {allowed: "doe.com"}}).call(input)
      }.not_to raise_error

      expect {
        transaction.call(input)
      }.to raise_error(ArgumentError, "not enough arguments supplied for step +validate+")
    end

    it "raises before executing any step" do
      expect {
        transaction.with_step_args(validate: {kwargs: {strict: true}}).call(input)
      }.to raise_error(ArgumentError, /step \+validate\+/)

      expect(Test::DB).to be_empty
    end

    context "with a keyrest parameter" do
      before do
        Test.send(:remove_const, :Container)
        Test.const_set(:Container, {
          process: -> input { {name: input["name"], email: input["email"]} },
          validate: -> input, allowed:, **rest {
            rest[:strict] && !input[:email].end_with?(allowed) ? raise(Test::NotValidError, "email not allowed") : input
          },
          persist: -> input { Test::DB << input and true }
        }.freeze)
      end

      it "accepts additional keyword arguments" do
        result = transaction
          .with_step_args(validate: {kwargs: {allowed: "doe.com", strict: true, extra: 1}})
          .call(input)

        expect(result).to be_a Dry::Monads::Result::Success
      end
    end
  end
end
