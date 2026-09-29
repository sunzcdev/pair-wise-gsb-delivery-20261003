# frozen_string_literal: true

require "dry/validation/contract"

RSpec.describe Dry::Validation::Contract, ".mount" do
  let(:address_contract_class) do
    Class.new(Dry::Validation::Contract) do
      params do
        required(:street).filled(:string)
        required(:zipcode).filled(:integer)
      end

      rule(:zipcode) do
        key.failure("must be greater than 0") if values[:zipcode] <= 0
      end
    end
  end

  context "mounted at a single nested hash" do
    let(:contract_class) do
      address_contract = address_contract_class

      Class.new(Dry::Validation::Contract) do
        params do
          required(:email).filled(:string)
          required(:shipping_address).hash
        end

        mount address_contract, at: :shipping_address
      end
    end

    it "writes the transformed output back to the parent result" do
      result = contract_class.new.(
        email: "jane@doe.org",
        shipping_address: {street: "Main St", zipcode: "12345"}
      )

      expect(result).to be_success
      expect(result.to_h).to eql(
        email: "jane@doe.org",
        shipping_address: {street: "Main St", zipcode: 12_345}
      )
    end

    it "moves schema errors to the mount path" do
      result = contract_class.new.(
        email: "jane@doe.org",
        shipping_address: {street: "Main St", zipcode: "oops"}
      )

      expect(result.errors.to_h).to eql(shipping_address: {zipcode: ["must be an integer"]})
    end

    it "moves rule errors to the mount path" do
      result = contract_class.new.(
        email: "jane@doe.org",
        shipping_address: {street: "Main St", zipcode: -1}
      )

      expect(result.errors.to_h).to eql(shipping_address: {zipcode: ["must be greater than 0"]})
    end

    it "skips the mounted contract when the parent schema failed at the mount path" do
      result = contract_class.new.(email: "jane@doe.org", shipping_address: "not a hash")

      expect(result.errors.to_h).to eql(shipping_address: ["must be a hash"])
    end

    it "skips the mounted contract when the value is nil" do
      contract_class = Class.new(Dry::Validation::Contract) do
        params do
          required(:email).filled(:string)
          optional(:shipping_address).maybe(:hash)
        end
      end

      contract_class.mount(address_contract_class, at: :shipping_address)

      result = contract_class.new.(email: "jane@doe.org")

      expect(result).to be_success
      expect(result.to_h).to eql(email: "jane@doe.org")
    end

    it "accepts a contract instance" do
      address_contract = address_contract_class.new

      contract_class = Class.new(Dry::Validation::Contract) do
        params do
          required(:shipping_address).hash
        end

        mount address_contract, at: :shipping_address
      end

      result = contract_class.new.(shipping_address: {street: "Main St", zipcode: "oops"})

      expect(result.errors.to_h).to eql(shipping_address: {zipcode: ["must be an integer"]})
    end

    it "is inherited by subclasses" do
      subclass = Class.new(contract_class) do
        params do
          required(:name).filled(:string)
        end
      end

      result = subclass.new.(
        email: "jane@doe.org",
        name: "jane",
        shipping_address: {street: "Main St", zipcode: "oops"}
      )

      expect(result.errors.to_h).to eql(shipping_address: {zipcode: ["must be an integer"]})
    end

    it "raises when the mount path is not defined by the schema" do
      address_contract = address_contract_class

      expect {
        Class.new(Dry::Validation::Contract) do
          params do
            required(:email).filled(:string)
          end

          mount address_contract, at: :address
        end
      }.to raise_error(
        Dry::Validation::InvalidKeysError,
        /mount specifies keys that are not defined by the schema: \[:address\]/
      )
    end
  end

  context "mounted at an array of hashes" do
    let(:contract_class) do
      address_contract = address_contract_class

      Class.new(Dry::Validation::Contract) do
        params do
          required(:contacts).array(:hash)
        end

        mount address_contract, at: :contacts, each: true
      end
    end

    it "writes the transformed output of every element back to the parent result" do
      result = contract_class.new.(
        contacts: [
          {street: "Main St", zipcode: "12345"},
          {street: "Oak St", zipcode: "67890"}
        ]
      )

      expect(result).to be_success
      expect(result.to_h).to eql(
        contacts: [
          {street: "Main St", zipcode: 12_345},
          {street: "Oak St", zipcode: 67_890}
        ]
      )
    end

    it "moves errors to the mount path keeping element indexes" do
      result = contract_class.new.(
        contacts: [
          {street: "Main St", zipcode: 12_345},
          {street: "Oak St", zipcode: "oops"},
          {street: "", zipcode: 67_890}
        ]
      )

      expect(result.errors.to_h).to eql(
        contacts: {
          1 => {zipcode: ["must be an integer"]},
          2 => {street: ["must be filled"]}
        }
      )
    end

    it "keeps failure indexes stable regardless of preceding elements" do
      result = contract_class.new.(
        contacts: [
          {street: "", zipcode: "oops"},
          {street: "Oak St", zipcode: "oops"},
          {street: "Pine St", zipcode: 67_890}
        ]
      )

      expect(result.errors.to_h).to eql(
        contacts: {
          0 => {street: ["must be filled"], zipcode: ["must be an integer"]},
          1 => {zipcode: ["must be an integer"]}
        }
      )
    end

    it "skips elements rejected by the parent schema and keeps the indexes of the rest" do
      result = contract_class.new.(
        contacts: [
          {street: "Main St", zipcode: 12_345},
          "not a hash",
          {street: "Pine St", zipcode: "oops"}
        ]
      )

      expect(result.errors.to_h).to eql(
        contacts: {
          1 => ["must be a hash"],
          2 => {zipcode: ["must be an integer"]}
        }
      )
    end
  end

  context "with base errors and meta in the mounted contract" do
    let(:address_contract_class) do
      Class.new(Dry::Validation::Contract) do
        params do
          required(:street).filled(:string)
        end

        rule(:street) do
          key.failure(text: "is reserved", code: 1001) if values[:street] == "Main St"
        end

        rule do
          base.failure("is not deliverable") if values[:street] == "Nowhere"
        end
      end
    end

    let(:contract_class) do
      address_contract = address_contract_class

      Class.new(Dry::Validation::Contract) do
        params do
          required(:shipping_address).hash
        end

        mount address_contract, at: :shipping_address
      end
    end

    it "moves base errors to the mount path" do
      result = contract_class.new.(shipping_address: {street: "Nowhere"})

      expect(result.errors.to_h).to eql(shipping_address: ["is not deliverable"])
    end

    it "preserves error meta at the mount path" do
      result = contract_class.new.(shipping_address: {street: "Main St"})

      error = result.errors.find { |err| err.path == %i[shipping_address street] }

      expect(error.text).to eql("is reserved")
      expect(error.meta).to eql(code: 1001)
    end
  end

  context "with context passing" do
    let(:address_contract_class) do
      Class.new(Dry::Validation::Contract) do
        params do
          required(:street).filled(:string)
        end

        rule(:street) do |context:|
          if context[:restricted]&.include?(values[:street])
            key.failure("is not allowed here")
          end

          context[:seen] = Array(context[:seen]) + [values[:street]]
        end
      end
    end

    def build_contract(**mount_options)
      address_contract = address_contract_class

      Class.new(Dry::Validation::Contract) do
        params do
          required(:shipping_address).hash
          required(:billing_address).hash
        end

        mount address_contract, at: :shipping_address, **mount_options
        mount address_contract, at: :billing_address, **mount_options
      end
    end

    it "does not pass the parent context by default" do
      contract_class = build_contract

      result = contract_class.new.(
        {shipping_address: {street: "Main St"}, billing_address: {street: "Main St"}},
        {restricted: ["Main St"]}
      )

      expect(result).to be_success
    end

    it "passes a copy of the parent context when context: true" do
      contract_class = build_contract(context: true)

      result = contract_class.new.(
        {shipping_address: {street: "Main St"}, billing_address: {street: "Oak St"}},
        {restricted: ["Main St"]}
      )

      expect(result.errors.to_h).to eql(shipping_address: {street: ["is not allowed here"]})
    end

    it "passes an explicit context hash" do
      contract_class = build_contract(context: {restricted: ["Main St"]})

      result = contract_class.new.(
        shipping_address: {street: "Main St"},
        billing_address: {street: "Oak St"}
      )

      expect(result.errors.to_h).to eql(shipping_address: {street: ["is not allowed here"]})
    end

    it "isolates context writes between mount points and the parent" do
      contract_class = build_contract(context: true)

      result = contract_class.new.(
        {shipping_address: {street: "Main St"}, billing_address: {street: "Main St"}},
        {restricted: []}
      )

      expect(result).to be_success
      expect(result.context[:seen]).to be_nil
    end
  end

  context "with the same contract mounted multiple times" do
    let(:contract_class) do
      address_contract = address_contract_class

      Class.new(Dry::Validation::Contract) do
        params do
          required(:shipping_address).hash
          required(:billing_address).hash
          required(:contacts).array(:hash)
        end

        mount address_contract, at: :shipping_address
        mount address_contract, at: :billing_address
        mount address_contract, at: :contacts, each: true
      end
    end

    it "moves errors to every mount path independently" do
      result = contract_class.new.(
        shipping_address: {street: "Main St", zipcode: -1},
        billing_address: {street: "", zipcode: 2},
        contacts: [
          {street: "Oak St", zipcode: 3},
          {street: "Pine St", zipcode: "oops"},
          {street: "", zipcode: 5}
        ]
      )

      expect(result.errors.to_h).to eql(
        shipping_address: {zipcode: ["must be greater than 0"]},
        billing_address: {street: ["must be filled"]},
        contacts: {
          1 => {zipcode: ["must be an integer"]},
          2 => {street: ["must be filled"]}
        }
      )
    end

    it "writes the transformed output back for every mount" do
      result = contract_class.new.(
        shipping_address: {street: "Main St", zipcode: "1"},
        billing_address: {street: "Oak St", zipcode: "2"},
        contacts: [{street: "Pine St", zipcode: "3"}]
      )

      expect(result).to be_success
      expect(result.to_h).to eql(
        shipping_address: {street: "Main St", zipcode: 1},
        billing_address: {street: "Oak St", zipcode: 2},
        contacts: [{street: "Pine St", zipcode: 3}]
      )
    end
  end
end
