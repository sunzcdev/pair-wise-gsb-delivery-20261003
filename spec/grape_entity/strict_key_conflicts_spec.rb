# frozen_string_literal: true

describe Grape::Entity do
  subject(:fresh_class) { Class.new(Grape::Entity) }

  describe 'strict key conflict mode' do
    let(:model) { { name: 'direct', full_name: 'aliased', extras: { name: 'merged', other: 1 } } }

    it 'is disabled by default and keeps last-write-wins behavior' do
      fresh_class.expose :name
      fresh_class.expose :full_name, as: :name

      expect(fresh_class.represent(model, serializable: true)).to eq(name: 'aliased')
    end

    context 'with a direct field and an aliased field landing on the same key' do
      subject(:entity_class) do
        Class.new(Grape::Entity) do
          self.strict_key_conflicts = true

          expose :name
          expose :full_name, as: :name
        end
      end

      it 'raises a KeyConflictError exposing both source paths' do
        expect { entity_class.represent(model, serializable: true) }
          .to raise_error(Grape::Entity::KeyConflictError) do |error|
            expect(error.key).to eq(:name)
            expect(error.first_source).to eq('name')
            expect(error.second_source).to eq('name (from attribute :full_name)')
            expect(error.message).to include('name', 'name (from attribute :full_name)')
          end
      end

      it 'outputs the key once a resolver is declared for it' do
        entity_class.resolve_key_conflict(:name) { |existing, incoming, _entity| "#{existing}/#{incoming}" }

        expect(entity_class.represent(model, serializable: true)).to eq(name: 'direct/aliased')
      end

      it 'passes the entity to the resolver' do
        entity_class.resolve_key_conflict(:name) { |_existing, _incoming, entity| entity.object[:full_name] }

        expect(fresh_class).not_to be_nil
        expect(entity_class.represent(model, serializable: true)).to eq(name: 'aliased')
      end

      it 'requires a block when declaring a resolver' do
        expect { entity_class.resolve_key_conflict(:name) }.to raise_error(ArgumentError)
      end
    end

    context 'when a direct field, an aliased field and a merged field all collide' do
      subject(:entity_class) do
        Class.new(Grape::Entity) do
          self.strict_key_conflicts = true

          expose :name
          expose :full_name, as: :name
          expose :extras, merge: true
        end
      end

      it 'does not output the key until the conflict is explicitly adjudicated' do
        expect { entity_class.represent(model, serializable: true) }
          .to raise_error(Grape::Entity::KeyConflictError)

        entity_class.resolve_key_conflict(:name) { |_existing, incoming, _entity| incoming }

        expect(entity_class.represent(model, serializable: true)).to eq(name: 'merged', other: 1)
      end

      it 'adjudicates every collision in exposure order' do
        entity_class.resolve_key_conflict(:name) { |existing, incoming, _entity| [existing, incoming].join('+') }

        expect(entity_class.represent(model, serializable: true))
          .to eq(name: 'direct+aliased+merged', other: 1)
      end
    end

    context 'with a direct field and a merged field landing on the same key' do
      subject(:entity_class) do
        Class.new(Grape::Entity) do
          self.strict_key_conflicts = true

          expose :name
          expose :extras, merge: true
        end
      end

      it 'raises a KeyConflictError' do
        expect { entity_class.represent(model, serializable: true) }
          .to raise_error(Grape::Entity::KeyConflictError) do |error|
            expect(error.key).to eq(:name)
            expect(error.second_source).to eq('extras.name (from attribute :extras)')
          end
      end

      it 'detects collisions on the JSON key level across strings and symbols' do
        expect { entity_class.represent({ name: 'direct', extras: { 'name' => 'merged' } }, serializable: true) }
          .to raise_error(Grape::Entity::KeyConflictError)
      end

      it 'still allows an explicit callable :merge strategy' do
        entity_class = Class.new(Grape::Entity) do
          self.strict_key_conflicts = true

          expose :name
          expose :extras, merge: ->(_key, old_value, _new_value) { old_value }
        end

        expect(entity_class.represent(model, serializable: true)).to eq(name: 'direct', other: 1)
      end

      it 'detects collisions between two merged hashes' do
        entity_class = Class.new(Grape::Entity) do
          self.strict_key_conflicts = true

          expose :first, merge: true
          expose :second, merge: true
        end

        expect do
          entity_class.represent({ first: { name: 'a' }, second: { name: 'b' } }, serializable: true)
        end.to raise_error(Grape::Entity::KeyConflictError) do |error|
          expect(error.first_source).to eq('first.name (from attribute :first)')
          expect(error.second_source).to eq('second.name (from attribute :second)')
        end
      end
    end

    context 'with conditional exposures' do
      subject(:entity_class) do
        Class.new(Grape::Entity) do
          self.strict_key_conflicts = true

          expose :name
          expose :full_name, as: :name, if: ->(_object, options) { options[:include_alias] }
        end
      end

      it 'does not treat fields whose conditions fail as conflicts' do
        expect(entity_class.represent(model, serializable: true)).to eq(name: 'direct')
      end

      it 'treats fields as conflicts once their conditions hold' do
        expect { entity_class.represent(model, serializable: true, include_alias: true) }
          .to raise_error(Grape::Entity::KeyConflictError)
      end
    end

    context 'when representing a collection' do
      subject(:entity_class) do
        Class.new(Grape::Entity) do
          self.strict_key_conflicts = true

          expose :name
          expose :full_name, as: :name, if: ->(object, _options) { object[:conflict] }
        end
      end

      it 'includes the element index in the conflict source path' do
        collection = [
          { name: 'first', full_name: 'ok', conflict: false },
          { name: 'second', full_name: 'clashing', conflict: true }
        ]

        expect { entity_class.represent(collection, serializable: true) }
          .to raise_error(Grape::Entity::KeyConflictError) do |error|
            expect(error.first_source).to eq('[1].name')
            expect(error.second_source).to eq('[1].name (from attribute :full_name)')
          end
      end
    end

    context 'with nested exposures' do
      subject(:entity_class) do
        Class.new(Grape::Entity) do
          self.strict_key_conflicts = true

          expose :name
          expose :details do
            expose :full_name, as: :name
            expose :label, as: :name, if: ->(_object, options) { options[:with_label] }
          end
        end
      end

      let(:model) { { name: 'outer', full_name: 'inner', label: 'clash' } }

      it 'scopes conflicts to the nesting level they occur in' do
        expect(entity_class.represent(model, serializable: true))
          .to eq(name: 'outer', details: { name: 'inner' })
      end

      it 'reports nested source paths' do
        expect { entity_class.represent(model, serializable: true, with_label: true) }
          .to raise_error(Grape::Entity::KeyConflictError) do |error|
            expect(error.first_source).to eq('details.name (from attribute :full_name)')
            expect(error.second_source).to eq('details.name (from attribute :label)')
          end
      end
    end

    describe 'runtime option' do
      it 'can enable strict mode per representation' do
        fresh_class.expose :name
        fresh_class.expose :full_name, as: :name

        expect { fresh_class.represent(model, serializable: true, strict_key_conflicts: true) }
          .to raise_error(Grape::Entity::KeyConflictError)
      end

      it 'can disable strict mode per representation' do
        fresh_class.strict_key_conflicts = true
        fresh_class.expose :name
        fresh_class.expose :full_name, as: :name

        expect(fresh_class.represent(model, serializable: true, strict_key_conflicts: false))
          .to eq(name: 'aliased')
      end
    end

    describe 'inheritance' do
      it 'passes the setting and the resolvers down to subclasses' do
        fresh_class.strict_key_conflicts = true
        fresh_class.expose :name
        fresh_class.expose :full_name, as: :name
        fresh_class.resolve_key_conflict(:name) { |existing, _incoming, _entity| existing }

        subclass = Class.new(fresh_class)

        expect(subclass.strict_key_conflicts?).to be(true)
        expect(subclass.represent(model, serializable: true)).to eq(name: 'direct')
      end

      it 'does not leak subclass resolvers back into the parent' do
        subclass = Class.new(fresh_class)
        subclass.resolve_key_conflict(:name) { |_existing, incoming, _entity| incoming }

        expect(fresh_class.key_conflict_resolvers).to be_empty
      end
    end
  end
end
