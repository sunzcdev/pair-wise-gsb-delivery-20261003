# frozen_string_literal: true

RSpec.describe "Macro signatures" do
  before do
    class Test::BaseContract < Dry::Validation::Contract; end
  end

  def build_contract(&block)
    Class.new(Test::BaseContract) do
      schema do
        required(:numbers).array(:integer)
        optional(:number).maybe(:integer)
      end

      class_exec(&block)
    end
  end

  context "with a legacy macro without a signature" do
    before do
      Test::BaseContract.register_macro(:legacy_even) do
        key.failure("must be even") unless values[key_name].all?(&:even?)
      end
    end

    after do
      Test::BaseContract.macros._container.delete("legacy_even")
    end

    it "is not checked and keeps working" do
      contract_class = build_contract do
        rule(:numbers).validate(legacy_even: :anything_goes)
      end

      expect { contract_class.new }.not_to raise_error

      contract = contract_class.new
      expect(contract.(numbers: [2, 4])).to be_success
      expect(contract.(numbers: [1, 2]).errors.to_h).to eql(numbers: ["must be even"])
    end
  end

  context "with a macro declaring required and optional arguments" do
    before do
      Test::BaseContract.register_macro(:size_between, signature: {required: 1, optional: 1}) do |macro:|
        min = macro.args[0]
        max = macro.args[1]

        key.failure("must have at least #{min} items") if values[key_name].size < min
        key.failure("must have at most #{max} items") if max && values[key_name].size > max
      end
    end

    after do
      Test::BaseContract.macros._container.delete("size_between")
    end

    it "accepts calls matching the signature" do
      contract_class = build_contract do
        rule(:numbers).validate(size_between: [2, 3])
      end

      contract = contract_class.new
      expect(contract.(numbers: [1, 2])).to be_success
      expect(contract.(numbers: [1]).errors.to_h).to eql(numbers: ["must have at least 2 items"])
      expect(contract.(numbers: [1, 2, 3, 4]).errors.to_h).to eql(numbers: ["must have at most 3 items"])
    end

    it "raises when a required argument is missing" do
      contract_class = build_contract do
        rule(:numbers).validate(:size_between)
      end

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /:size_between with invalid arguments: wrong number of arguments \(expected 1\.\.2, got 0\)/
      )
    end

    it "raises when too many arguments are given" do
      contract_class = build_contract do
        rule(:numbers).validate(size_between: [1, 2, 3])
      end

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /wrong number of arguments \(expected 1\.\.2, got 3\)/
      )
    end

    it "points out the contract, the rule and the macro" do
      class Test::SizedContract < Test::BaseContract
        schema do
          required(:numbers).array(:integer)
        end

        rule(:numbers).validate(size_between: [1, 2, 3])
      end

      expect { Test::SizedContract.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /Test::SizedContract\.rule\(:numbers\) calls macro :size_between/
      )
    end

    it "raises as soon as the contract definition is completed by inheritance" do
      contract_class = build_contract do
        rule(:numbers).validate(size_between: [1, 2, 3])
      end

      expect { Class.new(contract_class) }.to raise_error(Dry::Validation::InvalidMacroCallError)
    end

    it "checks macros used via each" do
      contract_class = build_contract do
        rule(:numbers).each(size_between: [1, 2, 3])
      end

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /:size_between with invalid arguments: wrong number of arguments \(expected 1\.\.2, got 3\)/
      )
    end
  end

  context "with a macro declaring keyword arguments" do
    before do
      Test::BaseContract.register_macro(:limited, signature: {required: 1, keywords: [:inclusive]}) do |macro:|
        limit = macro.args[0]
        inclusive = macro.args.last.is_a?(Hash) ? macro.args.last[:inclusive] : true

        too_big = inclusive ? values[key_name].size > limit : values[key_name].size >= limit
        key.failure("must have at most #{limit} items") if too_big
      end
    end

    after do
      Test::BaseContract.macros._container.delete("limited")
    end

    it "accepts declared keywords" do
      contract_class = build_contract do
        rule(:numbers).validate(limited: [2, {inclusive: false}])
      end

      contract = contract_class.new
      expect(contract.(numbers: [1])).to be_success
      expect(contract.(numbers: [1, 2]).errors.to_h).to eql(numbers: ["must have at most 2 items"])
    end

    it "raises on unknown keywords" do
      contract_class = build_contract do
        rule(:numbers).validate(limited: [2, {strict: true}])
      end

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /:limited with invalid arguments: unknown keyword arguments: :strict/
      )
    end
  end

  context "with inheritance and local macro overrides" do
    before do
      Dry::Validation.register_macro(:limited, signature: {required: 1}) do |macro:|
        limit = macro.args[0]
        key.failure("must have at most #{limit} items") if values[key_name].size > limit
      end
    end

    after do
      Dry::Validation::Macros.container._container.delete("limited")
    end

    it "checks calls against a local macro that overrides a global one" do
      contract_class = build_contract do
        register_macro(:limited, signature: {required: 2}) do |macro:|
          min, max = macro.args
          unless (min..max).cover?(values[key_name].size)
            key.failure("must have between #{min} and #{max} items")
          end
        end

        rule(:numbers).validate(limited: [1, 2])
      end

      contract = contract_class.new
      expect(contract.(numbers: [1, 2])).to be_success
      expect(contract.(numbers: [1, 2, 3]).errors.to_h).to eql(
        numbers: ["must have between 1 and 2 items"]
      )
    end

    it "rejects calls that only match the overridden global macro" do
      contract_class = build_contract do
        register_macro(:limited, signature: {required: 2}) do |macro:|
          min, max = macro.args
          unless (min..max).cover?(values[key_name].size)
            key.failure("must have between #{min} and #{max} items")
          end
        end

        rule(:numbers).validate(limited: 2)
      end

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /:limited with invalid arguments: wrong number of arguments \(expected 2, got 1\)/
      )
    end

    it "checks inherited rules against the subclass' own macro scope" do
      parent_class = build_contract do
        rule(:numbers).validate(limited: 2)
      end

      subclass = Class.new(parent_class) do
        register_macro(:limited, signature: {required: 2}) do |macro:|
          min, max = macro.args
          unless (min..max).cover?(values[key_name].size)
            key.failure("must have between #{min} and #{max} items")
          end
        end
      end

      expect { subclass.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /:limited with invalid arguments: wrong number of arguments \(expected 2, got 1\)/
      )
    end

    it "accepts inherited rules when the override stays compatible" do
      parent_class = build_contract do
        rule(:numbers).validate(limited: 2)
      end

      subclass = Class.new(parent_class) do
        schema do
          required(:numbers).array(:integer)
        end

        register_macro(:limited, signature: {required: 1, optional: 1}) do |macro:|
          max = macro.args[0]
          key.failure("must have at most #{max} items") if values[key_name].size > max
        end
      end

      contract = subclass.new
      expect(contract.(numbers: [1, 2])).to be_success
      expect(contract.(numbers: [1, 2, 3]).errors.to_h).to eql(numbers: ["must have at most 2 items"])
    end
  end

  context "with macros calling other macros" do
    before do
      Test::BaseContract.register_macro(:all_even, signature: {}) do
        key.failure("all numbers must be even") unless values[key_name].all?(&:even?)
      end

      Test::BaseContract.register_macro(:all_positive, signature: {}) do
        key.failure("all numbers must be positive") unless values[key_name].all?(&:positive?)
      end

      Test::BaseContract.register_macro(:all_positive_even, signature: {}) do
        macro(:all_positive)
        macro(:all_even)
      end

      Test::BaseContract.register_macro(:bounded_positive_even, signature: {required: 1}) do |macro:|
        macro(:all_positive_even)
        max = macro.args[0]
        key.failure("all numbers must be at most #{max}") unless values[key_name].all? { _1 <= max }
      end
    end

    after do
      container = Test::BaseContract.macros._container
      container.delete("all_even")
      container.delete("all_positive")
      container.delete("all_positive_even")
      container.delete("bounded_positive_even")
    end

    it "expands nested macro calls" do
      contract_class = build_contract do
        rule(:numbers).validate(bounded_positive_even: 10)
      end

      contract = contract_class.new
      expect(contract.(numbers: [2, 4, 6])).to be_success
      expect(contract.(numbers: [-2, 4]).errors.to_h).to eql(numbers: ["all numbers must be positive"])
      expect(contract.(numbers: [2, 3]).errors.to_h).to eql(numbers: ["all numbers must be even"])
      expect(contract.(numbers: [2, 12]).errors.to_h).to eql(numbers: ["all numbers must be at most 10"])
    end

    it "checks nested calls against signatures at expansion time" do
      Test::BaseContract.register_macro(:broken_caller, signature: {}) do
        macro(:bounded_positive_even)
      end

      contract_class = build_contract do
        rule(:numbers).validate(:broken_caller)
      end

      expect { contract_class.new.(numbers: [2]) }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /:bounded_positive_even with invalid arguments: wrong number of arguments \(expected 1, got 0\)/
      )
    ensure
      Test::BaseContract.macros._container.delete("broken_caller")
    end

    it "rejects cyclic expansion" do
      Test::BaseContract.register_macro(:ping, signature: {}) do
        macro(:pong)
      end

      Test::BaseContract.register_macro(:pong, signature: {}) do
        macro(:ping)
      end

      contract_class = build_contract do
        rule(:numbers).validate(:ping)
      end

      expect { contract_class.new.(numbers: [2]) }.to raise_error(
        Dry::Validation::MacroCycleError,
        /expands macros cyclically: :ping -> :pong -> :ping/
      )
    ensure
      Test::BaseContract.macros._container.delete("ping")
      Test::BaseContract.macros._container.delete("pong")
    end

    it "rejects self-recursive macros" do
      Test::BaseContract.register_macro(:looping, signature: {}) do
        macro(:looping)
      end

      contract_class = build_contract do
        rule(:numbers).validate(:looping)
      end

      expect { contract_class.new.(numbers: [2]) }.to raise_error(
        Dry::Validation::MacroCycleError, /:looping -> :looping/
      )
    ensure
      Test::BaseContract.macros._container.delete("looping")
    end
  end
end
