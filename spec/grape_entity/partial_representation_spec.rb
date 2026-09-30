# frozen_string_literal: true

require 'ostruct'

describe Grape::Entity do
  describe '.represent_partial' do
    let(:item_class) do
      Class.new do
        attr_accessor :id, :name, :nickname, :visible_name

        def initialize(id:, name: nil, nickname: nil, visible_name: nil)
          @id = id
          @name = name
          @nickname = nickname
          @visible_name = visible_name
          @name_gets = 0
          @visible_name_gets = 0
        end

        def name
          @name_gets += 1
          @name
        end

        def visible_name
          @visible_name_gets += 1
          @visible_name
        end

        def name_gets
          @name_gets
        end

        def visible_name_gets
          @visible_name_gets
        end
      end
    end

    let(:item_entity) do
      item_class = self.item_class
      entity_class = Class.new(Grape::Entity)

      entity_class.class_eval do
        expose :id
        expose :name,
               as: :display_name,
               format_with: :omit_value,
               on_format_error: :omit
        expose :visible_name,
               if: ->(_, options) { options[:show_optional] },
               format_with: ->(value) { value.to_s },
               on_format_error: :null
        expose :nickname,
               as: :nick,
               format_with: :null_value,
               on_format_error: :null
      end

      entity_class.define_method(:omit_value) do |value|
        raise StandardError, 'cannot format value' if value == 'broken'

        value&.upcase
      end
      entity_class.define_method(:null_value) do |value|
        raise StandardError, 'cannot format value' if value == 'broken-nick'

        value
      end
      entity_class
    end

    let(:parent_entity) do
      item_entity = self.item_entity
      Class.new(Grape::Entity) do
        expose :title
        expose :items, using: item_entity
        expose :after
      end
    end

    let(:items) do
      [
        item_class.new(id: 1, name: 'good', nickname: 'first'),
        item_class.new(id: 2, name: 'broken', nickname: 'broken-nick'),
        item_class.new(id: 3)
      ]
    end
    let(:model) { OpenStruct.new(title: 'parent', items: items, after: 'done') }

    it 'omits or nulls failed fields and keeps sibling fields and elements' do
      result = parent_entity.represent_partial(model)
      representation, errors = result

      expect(representation).to eq(
        title: 'parent',
        items: [
          { id: 1, display_name: 'GOOD', nick: 'first' },
          { id: 2, nick: nil },
          { id: 3, display_name: nil, nick: nil }
        ],
        after: 'done'
      )
      expect(errors.map { |error| error[:path] }).to eq([[:items, 1, :display_name], [:items, 1, :nick]])
      expect(errors.map { |error| error[:cause] }).to all(be_a(StandardError))
      expect(errors.map { |error| error[:cause].message }).to eq(Array.new(2, 'cannot format value'))
      expect(result.representation).to be_a(Hash)
      expect(result.representation[:items]).to all(be_a(Hash))
    end

    it 'does not call getters for fields hidden by conditions' do
      parent_entity.represent_partial(model, show_optional: false)

      expect(items.map(&:visible_name_gets)).to eq([0, 0, 0])
      expect(items.map(&:name_gets)).to eq([1, 1, 1])
    end

    it 'keeps normal nil values separate from failed and conditionally hidden fields' do
      entity = Class.new(Grape::Entity) do
        expose :name, format_with: ->(value) { value }, on_format_error: :omit
        expose :visible_name, if: ->(_, options) { options[:show] }, format_with: ->(value) { value }, on_format_error: :null
        expose :nickname,
               format_with: ->(value) { raise StandardError, 'cannot format value' if value == 'bad'; value },
               on_format_error: :null
      end

      result = entity.represent_partial(OpenStruct.new(nickname: 'bad'), show: false)

      expect(result.representation).to eq(name: nil, nickname: nil)
      expect(result.errors.map { |current_error| current_error[:path] }).to eq([[:nickname]])
    end

    it 'returns null instead of the default value when a null formatter fails' do
      entity = Class.new(Grape::Entity) do
        expose :name,
               default: 'default name',
               format_with: ->(value) { raise StandardError, 'cannot format value' if value == 'bad'; value },
               on_format_error: :null
      end

      result = entity.represent_partial(OpenStruct.new(name: 'bad'))

      expect(result.representation).to eq(name: nil)
      expect(result.errors.map { |error| error[:cause].message }).to eq(['cannot format value'])
    end

    it 'does not change the default raising behavior of serializable_hash' do
      expect { parent_entity.represent(model, serializable: true) }.to raise_error(StandardError, 'cannot format value')
    end

    it 'raises failures for fields without an explicit format error action' do
      entity = Class.new(Grape::Entity) do
        expose :name, format_with: ->(value) { raise StandardError, 'cannot format value' }
      end

      expect { entity.represent_partial(OpenStruct.new(name: 'broken')) }.to raise_error(StandardError, 'cannot format value')
    end

    it 'does not swallow exceptions raised while building the error path' do
      entity = Class.new(Grape::Entity) do
        expose :name,
               as: ->(object) { raise 'path failure' if object.name == 'broken'; :display_name },
               format_with: ->(value) { raise StandardError, 'cannot format value' },
               on_format_error: :omit
      end

      expect { entity.represent_partial(OpenStruct.new(name: 'broken')) }.to raise_error(RuntimeError, 'path failure')
    end

    it 'supports instance-level partial serialization' do
      result = item_entity.new(items[1]).partial_serializable_hash

      expect(result.representation).to eq(id: 2, nick: nil)
      expect(result.errors.map { |error| error[:path] }).to eq([[:display_name], [:nick]])
      expect(result.errors.map { |error| error[:cause] }).to all(be_a(StandardError))
    end

    it 'supports block formatters and present collections with collection-scoped paths' do
      detail_entity = Class.new(Grape::Entity) do
        expose :id
        expose :name, format_with: ->(value) { raise StandardError, 'block failure' if value == 'bad'; value }, on_format_error: :null
      end
      collection_entity = Class.new(Grape::Entity) do
        expose :things, using: detail_entity
      end
      collection_entity.present_collection true, :things

      result = collection_entity.represent_partial(
        [OpenStruct.new(id: 1, name: 'good'), OpenStruct.new(id: 2, name: 'bad')]
      )

      expect(result.representation).to eq(things: [{ id: 1, name: 'good' }, { id: 2, name: nil }])
      expect(result.errors.map { |error| error[:path] }).to eq([[:things, 1, :name]])
    end

    it 'preserves the original exception object as the error cause' do
      formatter_error = StandardError.new('original failure')
      detail_entity = Class.new(Grape::Entity) do
        expose :name,
               format_with: ->(value) { raise formatter_error if value == 'bad'; value },
               on_format_error: :null
      end
      nested_entity = Class.new(Grape::Entity) do
        expose :items, using: detail_entity
      end

      result = nested_entity.represent_partial(OpenStruct.new(items: [OpenStruct.new(name: 'bad')]))

      expect(result.errors).to eq([{ path: [:items, 0, :name], cause: formatter_error }])
      expect(result.errors.first[:cause]).to equal(formatter_error)
    end

    it 'rejects unknown format error actions' do
      expect do
        Class.new(Grape::Entity) do
          expose :name, format_with: ->(value) { value }, on_format_error: :ignore
        end
      end.to raise_error(ArgumentError, ':on_format_error must be :omit or :null')
    end

    it 'requires a formatter for format error actions' do
      expect do
        Class.new(Grape::Entity) do
          expose :name, on_format_error: :omit
        end
      end.to raise_error(ArgumentError, ':on_format_error requires :format_with')
    end
  end
end
