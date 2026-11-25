# frozen_string_literal: true

RSpec.describe ROM::SQL::Schema, :postgres do
  describe '#primary_key' do
    it 'returns primary key attributes' do
      schema_proc = Class.new(ROM::Relation[:sql]).schema do
        attribute :id, ROM::SQL::Types::Serial
      end

      schema = schema_proc.call
      schema.finalize_attributes!.finalize!

      expect(schema.primary_key).to eql([schema[:id]])
    end

    it 'returns empty array when there is no PK defined' do
      schema_proc = Class.new(ROM::Relation[:sql]).schema do
        attribute :id, ROM::SQL::Types::Integer
      end

      schema = schema_proc.call
      schema.finalize_attributes!.finalize!

      expect(schema.primary_key).to eql([])
    end
  end

  describe '#with_attributes' do
    it 'returns a new schema preserving every attribute and options' do
      schema_proc = Class.new(ROM::Relation[:sql]).schema do
        attribute :id, ROM::SQL::Types::Serial
        attribute :name, Types::String
      end

      schema = schema_proc.call
      schema.finalize_attributes!.finalize!

      new_schema = schema.with_attributes { |attr|
        attr.name.equal?(:name) ? attr.optional : attr
      }

      expect(new_schema).not_to be(schema)
      expect(new_schema.map(&:name)).to eql(%i[id name])
      expect(new_schema[:id].optional?).to be(false)
      expect(new_schema[:name].optional?).to be(true)
      expect(schema[:name].optional?).to be(false)
      expect(new_schema.name).to eql(schema.name)
    end
  end
end
