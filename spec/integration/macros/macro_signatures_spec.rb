# frozen_string_literal: true

RSpec.describe "Macro signatures" do
  subject(:contract) { contract_class.new }

  let(:contract_class) do
    Class.new(Dry::Validation::Contract) do
      schema do
        required(:numbers).array(:integer)
      end
    end
  end

  context "legacy macro without a signature" do
    before do
      contract_class.register_macro(:legacy_even) do
        key.failure("all numbers must be even") unless values[key_name].all?(&:even?)
      end

      contract_class.rule(:numbers).validate(legacy_even: [1, 2, {anything: :goes}])
    end

    it "is not checked and keeps working" do
      expect(contract.(numbers: [2, 4])).to be_success
      expect(contract.(numbers: [1, 2]).errors.to_h).to eql(numbers: ["all numbers must be even"])
    end
  end

  context "macro with a signature" do
    before do
      contract_class.register_macro(
        :size_between,
        signature: {required: [:min], optional: [:max], keywords: [:message]}
      ) do |macro:|
        min, max, opts = macro.args

        message = (opts && opts[:message]) || "must have at least #{min} items"
        key.failure(message) if values[key_name].size < min
        key.failure("must have at most #{max} items") if max && values[key_name].size > max
      end
    end

    it "accepts a call matching the signature" do
      contract_class.rule(:numbers).validate(size_between: [2, 3, {message: "too few"}])

      expect(contract.(numbers: [1, 2, 3])).to be_success
      expect(contract.(numbers: [1]).errors.to_h).to eql(numbers: ["too few"])
    end

    it "raises when a required argument is missing" do
      contract_class.rule(:numbers).validate(:size_between)

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /calls macro :size_between with invalid arguments: wrong number of arguments \(given 0, expected 1\.\.2\)/
      )
    end

    it "raises when too many arguments are given" do
      contract_class.rule(:numbers).validate(size_between: [1, 2, 3])

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /wrong number of arguments \(given 3, expected 1\.\.2\)/
      )
    end

    it "raises when an unknown keyword is used" do
      contract_class.rule(:numbers).validate(size_between: [1, {mesage: "typo"}])

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /unknown keywords: :mesage/
      )
    end

    it "points to the contract, rule and macro in the error message" do
      stub_const("Test::NumbersContract", contract_class)
      contract_class.rule(:numbers).validate(:size_between)

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /Test::NumbersContract\.rule\(:numbers\) calls macro :size_between/
      )
    end
  end

  context "macro with required keywords" do
    before do
      contract_class.register_macro(
        :size_at_least,
        signature: {required: [:num], required_keywords: [:message]}
      ) do |macro:|
        key.failure(macro.args.last[:message]) if values[key_name].size < macro.args[0]
      end
    end

    it "raises when a required keyword is missing" do
      contract_class.rule(:numbers).validate(size_at_least: 2)

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /missing required keywords: :message/
      )
    end
  end

  context "macro that is not registered" do
    it "raises an error pointing to the contract, rule and macro" do
      stub_const("Test::NumbersContract", contract_class)
      contract_class.rule(:numbers).validate(:nope)

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /Test::NumbersContract\.rule\(:numbers\) uses macro :nope which is not registered/
      )
    end
  end

  context "global macro with a signature" do
    before do
      Dry::Validation.register_macro(:global_min, signature: {required: 1}) do |macro:|
        key.failure("too few") if values[key_name].size < macro.args[0]
      end
    end

    after do
      Dry::Validation::Macros.container._container.delete("global_min")
    end

    it "is checked against its signature" do
      contract_class.rule(:numbers).validate(:global_min)

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /calls macro :global_min with invalid arguments/
      )
    end

    it "works when the call is valid" do
      contract_class.rule(:numbers).validate(global_min: 2)

      expect(contract.(numbers: [1, 2])).to be_success
    end
  end

  context "with inheritance" do
    let(:parent_class) do
      Class.new(Dry::Validation::Contract) do
        schema do
          required(:numbers).array(:integer)
        end

        register_macro(:min_items, signature: {required: [:num]}) do |macro:|
          key.failure("too few") if values[key_name].size < macro.args[0]
        end

        rule(:numbers).validate(min_items: 2)
      end
    end

    it "checks rules inherited from the parent against inherited macros" do
      child_class = Class.new(parent_class) do
        schema do
          required(:numbers).array(:integer)
        end
      end

      expect(child_class.new.(numbers: [1, 2])).to be_success
      expect(child_class.new.(numbers: [1]).errors.to_h).to eql(numbers: ["too few"])
    end

    it "resolves a same-name macro override in the child scope" do
      child_class = Class.new(parent_class) do
        schema do
          required(:numbers).array(:integer)
        end

        register_macro(:min_items, signature: {required: [:num, :other]}) do |macro:|
          key.failure("too few") if values[key_name].size < macro.args[0]
        end
      end

      expect(parent_class.new.(numbers: [1, 2])).to be_success
      expect { child_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /calls macro :min_items with invalid arguments: wrong number of arguments \(given 1, expected 2\)/
      )
    end
  end

  context "macros calling other macros" do
    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        schema do
          required(:numbers).array(:integer)
        end

        register_macro(:min_items, signature: {required: [:num]}) do |macro:|
          key.failure("must have at least #{macro.args[0]} items") if values[key_name].size < macro.args[0]
        end

        register_macro(:even_items, signature: {macros: [:min_items]}) do
          run_macro(:min_items, 2)
          key.failure("must contain only even numbers") unless values[key_name].all?(&:even?)
        end

        register_macro(:valid_numbers, signature: {required: [:num], macros: %i[min_items even_items]}) do |macro:|
          run_macro(:min_items, macro.args[0])
          run_macro(:even_items)
        end

        rule(:numbers).validate(valid_numbers: 3)
      end
    end

    it "expands nested macros at runtime" do
      expect(contract.(numbers: [2, 4, 6])).to be_success
      expect(contract.(numbers: [1]).errors.to_h).to eql(
        numbers: ["must have at least 3 items", "must have at least 2 items", "must contain only even numbers"]
      )
    end

    it "raises when an expanded macro is not registered" do
      contract_class.register_macro(:broken, signature: {macros: [:missing_macro]}) {}
      contract_class.rule(:numbers).validate(:broken)

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /uses macro :missing_macro which is not registered/
      )
    end
  end

  context "circular macro expansion" do
    it "is rejected when the contract is validated" do
      stub_const("Test::NumbersContract", contract_class)

      contract_class.register_macro(:macro_a, signature: {macros: [:macro_b]}) { run_macro(:macro_b) }
      contract_class.register_macro(:macro_b, signature: {macros: [:macro_a]}) { run_macro(:macro_a) }
      contract_class.rule(:numbers).validate(:macro_a)

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /Test::NumbersContract\.rule\(:numbers\) expands macro :macro_a into a circular chain: :macro_a -> :macro_b -> :macro_a/
      )
    end

    it "rejects a macro expanding into itself" do
      contract_class.register_macro(:loop, signature: {macros: [:loop]}) { run_macro(:loop) }
      contract_class.rule(:numbers).validate(:loop)

      expect { contract_class.new }.to raise_error(
        Dry::Validation::InvalidMacroCallError,
        /circular chain: :loop -> :loop/
      )
    end
  end
end
