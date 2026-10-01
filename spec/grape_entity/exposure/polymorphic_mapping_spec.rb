# frozen_string_literal: true

require 'ostruct'

describe Grape::Entity::Exposure::PolymorphicMapping do
  def entity_class(*exposed, &block)
    klass = Class.new(Grape::Entity)
    klass.class_exec { expose(*exposed) } if exposed.any?
    klass.class_exec(&block) if block
    klass
  end

  let(:dog_entity)    { entity_class(:name) { expose(:kind) { |_| 'dog' } } }
  let(:cat_entity)    { entity_class(:name) { expose(:kind) { |_| 'cat' } } }
  let(:animal_entity) { entity_class(:name) { expose(:kind) { |_| 'animal' } } }

  module TestPolymorphic
    class Animal
      attr_accessor :name
      def initialize(name = nil) = @name = name
    end
    class Dog < Animal; end
    class Cat < Animal; end
    class WildDog < Dog; end
    module Swimmer; end
    module Tagged
      include Swimmer
    end
    class Fish < Animal
      include Swimmer
    end
    # Pure-module hierarchy (no registered class keys): Tagged is more
    # specific than Swimmer because it includes it.
    class TaggedFish
      include Tagged
    end
  end

  let(:animal_class) { TestPolymorphic::Animal }
  let(:dog_class)    { TestPolymorphic::Dog }
  let(:cat_class)    { TestPolymorphic::Cat }
  let(:wild_class)   { TestPolymorphic::WildDog }
  let(:fish_class)   { TestPolymorphic::Fish }
  let(:swimmer_mod)  { TestPolymorphic::Swimmer }
  let(:tagged_mod)   { TestPolymorphic::Tagged }
  let(:tagged_fish_class) { TestPolymorphic::TaggedFish }

  let(:mapping) { described_class.new(dog_class => dog_entity, cat_class => cat_entity) }

  describe '#initialize' do
    it 'accepts a class => Entity map' do
      expect { mapping }.not_to raise_error
    end

    it 'rejects a non-hash declaration' do
      expect { described_class.new([[dog_class, dog_entity]]) }
        .to raise_error(ArgumentError, /must be a Hash/)
    end

    it 'rejects an empty declaration' do
      expect { described_class.new({}) }.to raise_error(ArgumentError, /cannot be empty/)
    end

    it 'rejects keys that are neither classes nor modules' do
      expect { described_class.new('Dog' => dog_entity) }
        .to raise_error(ArgumentError, /invalid polymorphic mapping key/)
    end

    it 'accepts module keys' do
      expect { described_class.new(swimmer_mod => dog_entity) }.not_to raise_error
    end

    it 'rejects presenters that are not Entity subclasses or Strings' do
      expect { described_class.new(dog_class => Object) }
        .to raise_error(ArgumentError, /invalid polymorphic presenter/)
      expect { described_class.new(dog_class => :dog_entity) }
        .to raise_error(ArgumentError)
    end

    it 'freezes the rules' do
      expect(mapping.rules).to be_frozen
    end
  end

  describe '.coerce' do
    it 'returns an existing mapping unchanged' do
      expect(described_class.coerce(mapping)).to be(mapping)
    end

    it 'compiles a hash into a mapping' do
      expect(described_class.coerce(dog_class => dog_entity)).to be_a(described_class)
    end
  end

  describe '#entity_for' do
    it 'matches an exact class' do
      expect(mapping.entity_for(dog_class.new)).to eq(dog_entity)
    end

    it 'matches subclasses against a registered parent class' do
      expect(mapping.entity_for(wild_class.new)).to eq(dog_entity)
    end

    it 'prefers the more specific class rule' do
      specific = described_class.new(animal_class => animal_entity, dog_class => dog_entity)
      expect(specific.entity_for(dog_class.new)).to eq(dog_entity)
      expect(specific.entity_for(cat_class.new)).to eq(animal_entity)
    end

    it 'does not depend on registration order for class hierarchy resolution' do
      reversed = described_class.new(dog_class => dog_entity, animal_class => animal_entity)
      expect(reversed.entity_for(wild_class.new)).to eq(dog_entity)
    end

    it 'matches a module via ===' do
      mod_mapping = described_class.new(swimmer_mod => animal_entity)
      expect(mod_mapping.entity_for(fish_class.new)).to eq(animal_entity)
    end

    it 'prefers a matching class over a matching module' do
      mixed = described_class.new(swimmer_mod => animal_entity, fish_class => cat_entity)
      expect(mixed.entity_for(fish_class.new)).to eq(cat_entity)
    end

    it 'prefers the more specific (including) module over a general one' do
      ordered = described_class.new(swimmer_mod => dog_entity, tagged_mod => cat_entity)
      expect(ordered.entity_for(tagged_fish_class.new)).to eq(cat_entity)
    end

    it 'raises when two unrelated module rules both match' do
      unrelated = Module.new
      ambiguous = described_class.new(swimmer_mod => dog_entity, unrelated => cat_entity)
      object = fish_class.new
      object.singleton_class.include(unrelated)
      expect { ambiguous.entity_for(object) }
        .to raise_error(described_class::AmbiguousPolymorphicMatch, /ambiguous polymorphic mapping/)
    end

    it 'does not resolve ambiguity via registration order' do
      unrelated = Module.new
      reversed = described_class.new(unrelated => cat_entity, swimmer_mod => dog_entity)
      object = fish_class.new
      object.singleton_class.include(unrelated)
      expect { reversed.entity_for(object) }
        .to raise_error(described_class::AmbiguousPolymorphicMatch)
    end

    it 'uses the :default rule for unmatched types' do
      with_default = described_class.new(dog_class => dog_entity, described_class::DEFAULT_KEY => animal_entity)
      expect(with_default.entity_for(OpenStruct.new)).to eq(animal_entity)
    end

    it 'raises for an unmatched type without a :default rule' do
      expect { mapping.entity_for(OpenStruct.new) }
        .to raise_error(described_class::UnmappedPolymorphicType, /no polymorphic presenter/)
    end

    it 'returns nil for a nil object' do
      expect(mapping.entity_for(nil)).to be_nil
    end

    it 'treats a nil key like any other invalid key' do
      expect { described_class.new(nil => dog_entity) }
        .to raise_error(ArgumentError, /invalid polymorphic mapping key/)
    end
  end

  describe '#represent' do
    let(:full_mapping) do
      described_class.new(
        dog_class => dog_entity,
        cat_class => cat_entity,
        described_class::DEFAULT_KEY => animal_entity
      )
    end

    it 'represents a single object with the matching entity' do
      result = full_mapping.represent(dog_class.new)
      expect(result).to be_a(Grape::Entity)
      expect(result.serializable_hash).to eq(name: nil, kind: 'dog')
    end

    it 'dispatches every element of a collection individually' do
      result = full_mapping.represent(
        [dog_class.new, cat_class.new, OpenStruct.new(name: 'str')], serializable: true
      )
      expect(result).to eq(
        [{ name: nil, kind: 'dog' }, { name: nil, kind: 'cat' }, { name: 'str', kind: 'animal' }]
      )
    end

    it 'represents nil elements in a collection as nil' do
      result = full_mapping.represent([dog_class.new, nil], serializable: true)
      expect(result).to eq([{ name: nil, kind: 'dog' }, nil])
    end

    it 'represents nil itself as nil' do
      expect(full_mapping.represent(nil, serializable: true)).to be_nil
    end

    it 'constantizes string presenter names lazily' do
      stub_const('PolymorphicSpecDogEntity', dog_entity)
      string_mapping = described_class.new(dog_class => 'PolymorphicSpecDogEntity')
      expect(string_mapping.entity_for(dog_class.new)).to eq(dog_entity)
    end
  end

  describe '#merge' do
    it 'returns a new mapping with the extra rules' do
      extended = mapping.merge(cat_class => animal_entity)
      expect(extended.entity_for(cat_class.new)).to eq(animal_entity)
    end

    it 'does not mutate the original mapping' do
      mapping.merge(animal_class => animal_entity)
      expect(mapping.rules.keys).to eq([dog_class, cat_class])
    end

    it 'lets new rules override existing keys only on the new mapping' do
      extended = mapping.merge(dog_class => animal_entity)
      expect(extended.entity_for(dog_class.new)).to eq(animal_entity)
      expect(mapping.entity_for(dog_class.new)).to eq(dog_entity)
    end
  end

  describe 'entities using a polymorphic :using option' do
    it 'builds a PolymorphicRepresentExposure' do
      klass = Class.new(Grape::Entity)
      klass.expose :items, using: { dog_class => dog_entity, cat_class => cat_entity }
      exposure = klass.find_exposure(:items)
      expect(exposure).to be_a(Grape::Entity::Exposure::PolymorphicRepresentExposure)
      expect(exposure).to be_polymorphic
      expect(exposure.mapping).to be_a(described_class)
    end

    it 'renders a nested collection with per-type entities' do
      fish_entity = entity_class(:name) { expose(:kind) { |_| 'fish' } }
      klass = Class.new(Grape::Entity)
      klass.expose :items, using: {
        dog_class => dog_entity,
        cat_class => cat_entity,
        swimmer_mod => fish_entity,
        described_class::DEFAULT_KEY => animal_entity
      }

      feed = OpenStruct.new(items: [dog_class.new, cat_class.new, fish_class.new, OpenStruct.new(name: 'other')])
      expect(klass.represent(feed, serializable: true)).to eq(
        items: [
          { name: nil, kind: 'dog' },
          { name: nil, kind: 'cat' },
          { name: nil, kind: 'fish' },
          { name: 'other', kind: 'animal' }
        ]
      )
    end

    it 'renders nil as nil for a single nested object' do
      klass = Class.new(Grape::Entity)
      klass.expose :pet, using: { dog_class => dog_entity, described_class::DEFAULT_KEY => animal_entity }
      expect(klass.represent(OpenStruct.new(pet: nil), serializable: true)).to eq(pet: nil)
    end

    it 'supports a proc providing the nested value' do
      klass = Class.new(Grape::Entity)
      klass.expose :best, using: { dog_class => dog_entity, described_class::DEFAULT_KEY => animal_entity } do |object|
        object.pets.first
      end
      model = OpenStruct.new(pets: [dog_class.new])
      expect(klass.represent(model, serializable: true)[:best][:kind]).to eq('dog')
    end
  end

  describe 'inheritance' do
    def parent_entity
      Class.new(Grape::Entity) do
        expose :items, using: {
          TestPolymorphic::Dog => Class.new(Grape::Entity) { expose(:kind) { |_| 'dog' } },
          TestPolymorphic::Cat => Class.new(Grape::Entity) { expose(:kind) { |_| 'cat' } }
        }
      end
    end

    it 'lets a subclass extend the inherited polymorphic mapping' do
      parent = parent_entity
      animal_entity = Class.new(Grape::Entity) { expose(:kind) { |_| 'animal' } }
      child = Class.new(parent) { expose :items, using: { TestPolymorphic::Animal => animal_entity } }

      result = child.represent(OpenStruct.new(items: [TestPolymorphic::Dog.new, TestPolymorphic::Animal.new]),
                               serializable: true)
      expect(result[:items].map { |item| item[:kind] }).to eq(%w[dog animal])
    end

    it 'does not let the subclass rules pollute the parent mapping' do
      parent = parent_entity
      animal_entity = Class.new(Grape::Entity) { expose(:kind) { |_| 'animal' } }
      Class.new(parent) { expose :items, using: { TestPolymorphic::Animal => animal_entity } }

      parent_mapping = parent.find_exposure(:items).mapping
      expect(parent_mapping.rules).not_to have_key(TestPolymorphic::Animal)
      expect {
        parent.represent(OpenStruct.new(items: [TestPolymorphic::Animal.new]), serializable: true)
      }.to raise_error(described_class::UnmappedPolymorphicType)
    end

    it 'does not pollute representation configurations already created' do
      parent = parent_entity
      first_child = Class.new(parent)
      fish_entity = Class.new(Grape::Entity) { expose(:kind) { |_| 'fish' } }
      Class.new(parent) { expose :items, using: { TestPolymorphic::Fish => fish_entity } }

      expect(first_child.find_exposure(:items).mapping.rules).not_to have_key(TestPolymorphic::Fish)
      expect {
        first_child.represent(OpenStruct.new(items: [TestPolymorphic::Fish.new]), serializable: true)
      }.to raise_error(described_class::UnmappedPolymorphicType)
    end

    it 'keeps the inherited rules when the subclass adds unrelated exposures' do
      parent = parent_entity
      child = Class.new(parent) { expose :meta }
      feed = OpenStruct.new(items: [TestPolymorphic::Dog.new], meta: 'ok')
      result = child.represent(feed, serializable: true)
      expect(result[:items].first[:kind]).to eq('dog')
      expect(result[:meta]).to eq('ok')
    end
  end
end
