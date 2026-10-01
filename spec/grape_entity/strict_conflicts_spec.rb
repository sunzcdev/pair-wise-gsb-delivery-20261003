# frozen_string_literal: true

require 'spec_helper'
require 'ostruct'

describe 'Strict conflict mode' do
  subject(:entity_class) do
    Class.new(Grape::Entity) do
      def self.name
        'ConflictEntity'
      end

      expose :name
      expose :title, as: :name
    end
  end

  let(:object) { OpenStruct.new(name: 'direct', title: 'aliased') }

  context 'when disabled (default)' do
    it 'silently keeps the last written value' do
      expect(entity_class.represent(object, serializable: true)).to eq(name: 'aliased')
    end
  end

  context 'when enabled' do
    it 'raises KeyConflictError naming both source paths' do
      expect { entity_class.represent(object, serializable: true, strict_conflicts: true) }
        .to raise_error(Grape::Entity::KeyConflictError, /:name.*ConflictEntity\.name.*ConflictEntity\.title \(as: name\)/m)
    end

    it 'does not treat conditionally skipped exposures as conflicts' do
      conditional_class = Class.new(Grape::Entity) do
        expose :name
        expose :nickname, as: :name, if: ->(_object, options) { options[:with_nickname] }
      end
      conditional_object = OpenStruct.new(name: 'direct', nickname: 'skipped')

      expect(conditional_class.represent(conditional_object, serializable: true, strict_conflicts: true))
        .to eq(name: 'direct')

      expect do
        conditional_class.represent(conditional_object, serializable: true, strict_conflicts: true, with_nickname: true)
      end.to raise_error(Grape::Entity::KeyConflictError, /:name/)
    end

    it 'includes the element index in source paths of collection elements' do
      objects = [OpenStruct.new(name: 'first', title: 'uno'), OpenStruct.new(name: 'second', title: 'dos')]

      expect { entity_class.represent(objects, serializable: true, strict_conflicts: true) }
        .to raise_error(Grape::Entity::KeyConflictError, /ConflictEntity\.name\[0\].*ConflictEntity\.title \(as: name\)\[0\]/m)
    end

    it 'rejects a blanket resolver that is not keyed per output key' do
      expect do
        entity_class.represent(object, serializable: true, strict_conflicts: true,
                                      conflict_resolvers: ->(_key, _old, new) { new })
      end.to raise_error(ArgumentError, /must be a Hash mapping output keys to callables/)
    end

    it 'ignores resolvers declared for other keys' do
      expect do
        entity_class.represent(object, serializable: true, strict_conflicts: true,
                                      conflict_resolvers: { other: ->(_key, _old, new, _sources) { new } })
      end.to raise_error(Grape::Entity::KeyConflictError, /:name/)
    end

    it 'uses the adjudicator declared for the conflicting key' do
      resolver = ->(_key, existing, incoming, _sources) { "#{existing}+#{incoming}" }

      expect(entity_class.represent(object, serializable: true, strict_conflicts: true,
                                            conflict_resolvers: { name: resolver }))
        .to eq(name: 'direct+aliased')
    end

    it 'passes both source paths to the adjudicator' do
      received_sources = nil
      resolver = ->(_key, _existing, incoming, sources) { received_sources = sources; incoming }

      entity_class.represent(object, serializable: true, strict_conflicts: true,
                                     conflict_resolvers: { 'name' => resolver })

      expect(received_sources).to eq(['ConflictEntity.name', 'ConflictEntity.title (as: name)'])
    end
  end

  context 'with a direct field, an aliased field and a merged field colliding on one key' do
    subject(:three_way_class) do
      Class.new(Grape::Entity) do
        def self.name
          'ThreeWayEntity'
        end

        expose :name
        expose :title, as: :name
        expose :extras, merge: true
      end
    end

    let(:object) { OpenStruct.new(name: 'direct', title: 'aliased', extras: { name: 'merged' }) }

    it 'without strict mode silently overwrites the direct field, then fails the merge cryptically' do
      # direct vs alias: last write wins silently; the merge collision then
      # surfaces as an opaque ArgumentError with no hint about the sources.
      expect { three_way_class.represent(object, serializable: true) }
        .to raise_error(ArgumentError, /wrong number of arguments/)
    end

    it 'reports a merge collision with both source paths instead of a cryptic error' do
      direct_and_merge_class = Class.new(Grape::Entity) do
        def self.name
          'MergeOnlyEntity'
        end

        expose :name
        expose :extras, merge: true
      end

      expect do
        direct_and_merge_class.represent(object, serializable: true, strict_conflicts: true)
      end.to raise_error(Grape::Entity::KeyConflictError) do |error|
        expect(error.message).to include('MergeOnlyEntity.name')
        expect(error.message).to include('MergeOnlyEntity.extras (merge)')
      end
    end

    it 'refuses to output the key until an adjudicator is declared' do
      expect { three_way_class.represent(object, serializable: true, strict_conflicts: true) }
        .to raise_error(Grape::Entity::KeyConflictError) do |error|
          expect(error.message).to include('ThreeWayEntity.name')
          expect(error.message).to include('ThreeWayEntity.title (as: name)')
        end
    end

    it 'reports the merge source once the first collision is adjudicated' do
      resolver = ->(_key, _existing, incoming, _sources) { incoming }

      expect do
        three_way_class.represent(object, serializable: true, strict_conflicts: true,
                                          conflict_resolvers: { name: resolver })
      end.not_to raise_error
    end

    it 'adjudicates every collision of the key and outputs only the final adjudicated value' do
      adjudications = []
      resolver = lambda do |key, existing, incoming, sources|
        adjudications << [key, existing, incoming, sources]
        incoming
      end

      result = three_way_class.represent(object, serializable: true, strict_conflicts: true,
                                                 conflict_resolvers: { name: resolver })

      expect(result).to eq(name: 'merged')
      expect(adjudications.map { |key, _existing, _incoming, _sources| key }).to eq(%i[name name])
      expect(adjudications[0][3]).to eq(['ThreeWayEntity.name', 'ThreeWayEntity.title (as: name)'])
      expect(adjudications[1][3]).to eq(['ThreeWayEntity.title (as: name) (adjudicated)', 'ThreeWayEntity.extras (merge)'])
    end

    it 'honors a merge callable as explicit adjudication for merged keys' do
      merge_class = Class.new(Grape::Entity) do
        expose :name
        expose :extras, merge: ->(_key, old_val, new_val) { "#{old_val}|#{new_val}" }
      end
      merge_object = OpenStruct.new(name: 'direct', extras: { name: 'merged' })

      expect(merge_class.represent(merge_object, serializable: true, strict_conflicts: true))
        .to eq(name: 'direct|merged')
    end
  end
end
