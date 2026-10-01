# frozen_string_literal: true

require 'ostruct'

describe Grape::Entity::Exposure::PolymorphicRepresentExposure do
  let(:mapping) do
    dog_entity = Class.new(Grape::Entity) { expose(:kind) { |_| 'dog' } }
    Grape::Entity::Exposure::PolymorphicMapping.new(String => dog_entity)
  end

  let(:subexposure) do
    Grape::Entity::Exposure::DelegatorExposure.new(:foo, {}, [])
  end

  subject(:exposure) { described_class.new(:foo, {}, [], mapping, subexposure) }

  describe '#setup' do
    it 'exposes the mapping and the delegating subexposure' do
      expect(exposure.mapping).to eq(mapping)
      expect(exposure.subexposure).to eq(subexposure)
      expect(exposure).to be_polymorphic
    end
  end

  describe '#dup' do
    it 'preserves the mapping and subexposure' do
      copy = exposure.dup
      expect(copy).to eq(exposure)
      expect(copy.mapping).to eq(mapping)
    end
  end

  describe '#with_extra_rules' do
    it 'returns an exposure with an extended mapping' do
      cat_entity = Class.new(Grape::Entity)
      copy = exposure.with_extra_rules(Integer => cat_entity)
      expect(copy.mapping.rules).to include(String, Integer)
    end

    it 'does not mutate the original exposure or its mapping' do
      exposure.with_extra_rules(Integer => Class.new(Grape::Entity))
      expect(exposure.mapping.rules.keys).to eq([String])
    end
  end

  describe '#value' do
    it 'delegates nested rendering to the mapping' do
      model = OpenStruct.new(foo: 'hello')
      entity = Grape::Entity.new(model, Grape::Entity::Options.new)
      result = exposure.value(entity, Grape::Entity::Options.new)
      expect(result).to be_a(Grape::Entity)
      expect(result.serializable_hash).to eq(kind: 'dog')
    end
  end

  describe '#valid?' do
    it 'delegates to the subexposure' do
      model = OpenStruct.new(foo: 'hello')
      entity = Grape::Entity.new(model, Grape::Entity::Options.new)
      expect(exposure.valid?(entity)).to be(true)
    end
  end
end
