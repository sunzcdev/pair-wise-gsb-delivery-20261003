# frozen_string_literal: true

require "dry/validation/contract"

RSpec.describe Dry::Validation::Contract, ".mount" do
  let(:address_contract_class) do
    Class.new(Dry::Validation::Contract) do
      params do
        required(:street).filled(:string)
        required(:zip).filled(:integer)
      end

      rule do
        base.failure("not a deliverable address") if values[:street] == "Nowhere"
      end

      rule(:zip) do
        key.failure(text: "is not routable", code: :unroutable) if value == 0
      end
    end
  end

  let(:address_contract) { address_contract_class.new }

  describe "single object" do
    subject(:contract) do
      mounted_contract = address_contract

      Class.new(Dry::Validation::Contract) do
        params do
          required(:email).filled(:string)
          required(:address).filled(:hash)
        end

        mount :address, mounted_contract
      end.new
    end

    it "puts the sub-contract's output into the parent result" do
      result = contract.(email: "jane@doe.org", address: {street: "Main St", zip: "12345"})

      expect(result).to be_success
      expect(result.to_h).to eql(email: "jane@doe.org", address: {street: "Main St", zip: 12_345})
    end

    it "moves field errors to the mount path" do
      result = contract.(email: "jane@doe.org", address: {street: "", zip: "nope"})

      expect(result.errors.to_h).to eql(
        address: {street: ["must be filled"], zip: ["must be an integer"]}
      )
    end

    it "moves base errors to the mount path" do
      result = contract.(email: "jane@doe.org", address: {street: "Nowhere", zip: 12_345})

      expect(result.errors.to_h).to eql(address: ["not a deliverable address"])
    end

    it "preserves error meta data" do
      result = contract.(email: "jane@doe.org", address: {street: "Main St", zip: 0})

      message = result.errors.find { |error| error.path == %i[address zip] }

      expect(message.text).to eql("is not routable")
      expect(message.meta).to eql(code: :unroutable)
    end

    it "does not apply the sub-contract when the key is missing" do
      contract_class = Class.new(Dry::Validation::Contract) do
        params do
          required(:email).filled(:string)
          optional(:address).maybe(:hash)
        end
      end
      contract_class.mount(:address, address_contract)

      result = contract_class.new.(email: "jane@doe.org")

      expect(result).to be_success
      expect(result.to_h).to eql(email: "jane@doe.org")
    end

    it "does not apply the sub-contract when the value has schema errors" do
      result = contract.(email: "jane@doe.org", address: "not a hash")

      expect(result.errors.to_h).to eql(address: ["must be a hash"])
    end
  end

  describe "array of objects" do
    subject(:contract) do
      mounted_contract = address_contract

      Class.new(Dry::Validation::Contract) do
        params do
          required(:contacts).filled(:array)
        end

        mount :contacts, mounted_contract, each: true
      end.new
    end

    it "puts each sub-contract output into the parent result" do
      result = contract.(contacts: [{street: "Main St", zip: "12345"}, {street: "Oak St", zip: "678"}])

      expect(result).to be_success
      expect(result.to_h).to eql(
        contacts: [{street: "Main St", zip: 12_345}, {street: "Oak St", zip: 678}]
      )
    end

    it "moves errors to the mount path including the element index" do
      result = contract.(contacts: [{street: "", zip: "12345"}, {street: "Oak St", zip: 678}])

      expect(result.errors.to_h).to eql(contacts: {0 => {street: ["must be filled"]}})
    end

    it "moves base errors to the mount path including the element index" do
      result = contract.(contacts: [{street: "Nowhere", zip: 12_345}])

      expect(result.errors.to_h).to eql(contacts: {0 => ["not a deliverable address"]})
    end

    it "keeps failure indexes stable regardless of preceding successes and failures" do
      result = contract.(contacts: [
        {street: "Main St", zip: 12_345},
        {street: "", zip: 12_345},
        {street: "Oak St", zip: 678},
        {street: "", zip: 678},
        {street: "Pine St", zip: 999}
      ])

      expect(result.errors.to_h).to eql(
        contacts: {
          1 => {street: ["must be filled"]},
          3 => {street: ["must be filled"]}
        }
      )

      expect(result.to_h[:contacts]).to eql([
        {street: "Main St", zip: 12_345},
        {street: "", zip: 12_345},
        {street: "Oak St", zip: 678},
        {street: "", zip: 678},
        {street: "Pine St", zip: 999}
      ])
    end
  end

  describe "context handling" do
    let(:tracking_contract_class) do
      Class.new(Dry::Validation::Contract) do
        params do
          required(:street).filled(:string)
        end

        rule do |context:|
          base.failure("context leaked between mounts") if context[:touched]
          context[:touched] = true
        end
      end
    end

    it "does not share context state between two mount points" do
      tracking_contract = tracking_contract_class.new

      contract = Class.new(Dry::Validation::Contract) do
        params do
          required(:shipping_address).filled(:hash)
          required(:billing_address).filled(:hash)
        end

        mount :shipping_address, tracking_contract
        mount :billing_address, tracking_contract
      end.new

      result = contract.(
        shipping_address: {street: "Main St"},
        billing_address: {street: "Oak St"}
      )

      expect(result).to be_success
      expect(result.context.each.to_h).to eql({})
    end

    it "does not leak sub-contract context writes into the parent context" do
      contract = Class.new(Dry::Validation::Contract) do
        params do
          required(:address).filled(:hash)
        end
      end
      contract.mount(:address, tracking_contract_class.new)

      result = contract.new.(address: {street: "Main St"})

      expect(result).to be_success
      expect(result.context.each.to_h).to eql({})
    end

    it "passes a snapshot of the parent context when declared" do
      guard_contract_class = Class.new(Dry::Validation::Contract) do
        params do
          required(:street).filled(:string)
        end

        rule(:street) do |context:|
          key.failure("is not available in this tenant") if context[:tenant] != "acme"
          context[:tenant] = "changed"
        end
      end

      contract = Class.new(Dry::Validation::Contract) do
        params do
          required(:address).filled(:hash)
        end
      end
      contract.mount(:address, guard_contract_class.new, context: true)

      failure = contract.new.(address: {street: "Main St"})
      expect(failure.errors.to_h).to eql(address: {street: ["is not available in this tenant"]})

      success = contract.new.({address: {street: "Main St"}}, {tenant: "acme"})
      expect(success).to be_success
      expect(success.context.each.to_h).to eql(tenant: "acme")
    end

    it "passes only the declared context keys" do
      guard_contract_class = Class.new(Dry::Validation::Contract) do
        params do
          required(:street).filled(:string)
        end

        rule do |context:|
          base.failure("tenant missing") unless context[:tenant]
          base.failure("secret leaked") if context[:secret]
        end
      end

      contract = Class.new(Dry::Validation::Contract) do
        params do
          required(:address).filled(:hash)
        end
      end
      contract.mount(:address, guard_contract_class.new, context: [:tenant])

      result = contract.new.({address: {street: "Main St"}}, {tenant: "acme", secret: "s3cret"})

      expect(result).to be_success
    end
  end

  describe "class interface" do
    it "accepts a contract class and instantiates it" do
      contract_class = Class.new(Dry::Validation::Contract) do
        params do
          required(:address).filled(:hash)
        end
      end
      mount = contract_class.mount(:address, address_contract_class)

      expect(mount.contract).to be_a(address_contract_class)
      expect(contract_class.new.(address: {street: "Main St", zip: 12_345})).to be_success
    end

    it "raises a meaningful error when contract is not a contract" do
      contract_class = Class.new(Dry::Validation::Contract) do
        params do
          required(:address).filled(:hash)
        end
      end

      expect { contract_class.mount(:address, "nope") }.to raise_error(
        ArgumentError, /must be a Dry::Validation::Contract/
      )
    end

    it "inherits mounts" do
      mounted_contract = address_contract

      parent_class = Class.new(Dry::Validation::Contract) do
        params do
          required(:address).filled(:hash)
        end

        mount :address, mounted_contract
      end

      child_class = Class.new(parent_class) do
        params do
          optional(:email).filled(:string)
        end
      end

      result = child_class.new.(address: {street: "", zip: "12345"})

      expect(result.errors.to_h).to eql(address: {street: ["must be filled"]})
    end
  end
end
