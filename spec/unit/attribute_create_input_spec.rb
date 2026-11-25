# frozen_string_literal: true

RSpec.describe ROM::SQL::Attribute, '#to_create_input_type' do
  def input_hash_for(type)
    attr = ROM::SQL::Attribute[type].with(name: :value)
    schema = ROM::SQL::Schema.define(
      :things,
      attributes: [ROM::SQL::Schema.build_attribute_info(attr.to_create_input_type, name: :value)],
      attr_class: ROM::SQL::Attribute
    )

    schema.to_create_input_hash
  end

  context 'with a plain default' do
    let(:type) { ROM::SQL::Types::Integer.default(42) }

    it 'does not resolve the default for an omitted key' do
      expect(input_hash_for(type).call({})).to eql({})
    end

    it 'keeps an explicitly provided value' do
      expect(input_hash_for(type).call(value: 3)).to eql(value: 3)
    end
  end

  context 'with an optional default' do
    let(:type) { ROM::SQL::Types::Integer.optional.default(42) }

    it 'does not resolve the default for an omitted key' do
      expect(input_hash_for(type).call({})).to eql({})
    end

    it 'keeps an explicitly provided nil' do
      expect(input_hash_for(type).call(value: nil)).to eql(value: nil)
    end
  end

  context 'with a default on the non-nil branch of an optional type' do
    let(:type) { ROM::SQL::Types::Integer.default(42).optional }

    it 'does not resolve the default for an omitted key' do
      expect(input_hash_for(type).call({})).to eql({})
    end
  end

  context 'with a constrained default' do
    let(:type) { ROM::SQL::Types::Strict::String.default('default') }

    it 'keeps validation of provided values' do
      input = input_hash_for(type)

      expect { input.call(value: nil) }.to raise_error(Dry::Types::SchemaError)
      expect(input.call(value: 'Jane')).to eql(value: 'Jane')
    end
  end

  it 'does not apply to primary key types' do
    expect(input_hash_for(ROM::SQL::Types::Serial).call({})).to eql({})
  end
end
