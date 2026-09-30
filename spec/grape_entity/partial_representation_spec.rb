# frozen_string_literal: true

describe Grape::Entity, 'partial representation' do
  let(:fresh_class) { Class.new(Grape::Entity) }

  let(:model_class) do
    Class.new do
      attr_reader :id

      def initialize(id = 1)
        @id = id
        @getter_calls = Hash.new(0)
      end

      def name
        "name-#{id}"
      end

      def broken
        @getter_calls[:broken] += 1
        raise StandardError, 'broken formatter boom'
      end

      def getter_calls
        @getter_calls
      end
    end
  end

  describe '.represent_partial' do
    before do
      fresh_class.expose :id
      fresh_class.expose :name
      fresh_class.expose :broken, as: :broken_value
    end

    it 'returns a PartialResult with value and structured errors' do
      result = fresh_class.represent_partial(model_class.new)

      expect(result).to be_a(Grape::Entity::PartialResult)
      expect(result.value).to eq(id: 1, name: 'name-1', broken_value: nil)
      expect(result.success?).to be(false)
      expect(result.errors.size).to eq(1)

      error = result.errors.first
      expect(error).to be_a(Grape::Entity::PartialError)
      expect(error.path).to eq([:broken_value])
      expect(error.attribute).to eq(:broken)
      expect(error.message).to eq('broken formatter boom')
      expect(error.exception).to be_a(StandardError)
      expect(error.cause).to equal(error.exception)
    end

    it 'does not change the default raising behavior of serializable_hash' do
      expect { fresh_class.represent(model_class.new).serializable_hash }
        .to raise_error(StandardError, 'broken formatter boom')
    end

    it 'does not report legitimately nil values as errors' do
      model = Class.new do
        def id; 1; end
        def maybe; nil; end
      end

      entity = Class.new(Grape::Entity) do
        expose :id
        expose :maybe
      end

      result = entity.represent_partial(model.new)

      expect(result.value).to eq(id: 1, maybe: nil)
      expect(result.errors).to be_empty
      expect(result.success?).to be(true)
    end

    it 'does not report fields hidden by conditions as errors' do
      entity = Class.new(Grape::Entity) do
        expose :id
        expose :hidden_field, if: ->(_m, _o) { false }
        expose :other_field, unless: ->(_m, _o) { true }
      end

      model = Object.new
      def model.id; 1; end
      def model.hidden_field; 'x'; end
      def model.other_field; 'y'; end

      result = entity.represent_partial(model)

      expect(result.value).to eq(id: 1)
      expect(result.errors).to be_empty
    end
  end

  describe 'on_error' do
    it 'omits the field when configured with :omit' do
      fresh_class.expose :id
      fresh_class.expose :broken, as: :broken_value, on_error: :omit

      result = fresh_class.represent_partial(model_class.new)

      expect(result.value).to eq(id: 1)
      expect(result.errors.map(&:path)).to eq([[:broken_value]])
    end

    it 'renders the field as null when configured with :null' do
      fresh_class.expose :id
      fresh_class.expose :broken, as: :broken_value, on_error: :null

      result = fresh_class.represent_partial(model_class.new)

      expect(result.value).to eq(id: 1, broken_value: nil)
    end

    it 'uses the runtime on_error option as a global fallback' do
      fresh_class.expose :id
      fresh_class.expose :broken, as: :broken_value

      result = fresh_class.represent_partial(model_class.new, on_error: :omit)

      expect(result.value).to eq(id: 1)
    end

    it 'lets the field-level setting win over the runtime fallback' do
      fresh_class.expose :id
      fresh_class.expose :broken, as: :broken_value, on_error: :null

      result = fresh_class.represent_partial(model_class.new, on_error: :omit)

      expect(result.value).to eq(id: 1, broken_value: nil)
    end

    it 'rejects invalid values at exposure declaration time' do
      expect { fresh_class.expose :id, on_error: :bogus }
        .to raise_error(ArgumentError, ':on_error must be :null or :omit')
    end
  end

  describe 'instance entry point' do
    it 'supports #represent_partial on an entity instance' do
      fresh_class.expose :id
      fresh_class.expose :broken, as: :broken_value

      result = fresh_class.new(model_class.new).represent_partial

      expect(result).to be_a(Grape::Entity::PartialResult)
      expect(result.value).to eq(id: 1, broken_value: nil)
      expect(result.errors.map(&:path)).to eq([[:broken_value]])
    end

    it 'returns nil value and no errors for a nil object' do
      fresh_class.expose :id

      result = fresh_class.new(nil).represent_partial

      expect(result.value).to be_nil
      expect(result.errors).to be_empty
    end
  end

  describe 'collections' do
    let(:item_entity) do
      Class.new(Grape::Entity) do
        expose :id
        expose :label do |item|
          raise StandardError, 'middle boom' if item.id == 2

          "label-#{item.id}"
        end
      end
    end

    let(:items) { [model_class.new(1), model_class.new(2), model_class.new(3)] }

    it 'keeps representing the other elements and reports the index based path' do
      result = item_entity.represent_partial(items)

      expect(result.value).to eq(
        [
          { id: 1, label: 'label-1' },
          { id: 2, label: nil },
          { id: 3, label: 'label-3' }
        ]
      )
      expect(result.errors.map(&:path)).to eq([[1, :label]])
      expect(result.errors.first.exception.message).to eq('middle boom')
    end

    it 'omits a failing element with the global :omit strategy' do
      result = item_entity.represent_partial(items, on_error: :omit)

      expect(result.value).to eq(
        [
          { id: 1, label: 'label-1' },
          { id: 2 },
          { id: 3, label: 'label-3' }
        ]
      )
    end

    it 'omits a whole element with the index path when presenting it raises outside field handling' do
      entity = Class.new(Grape::Entity) do
        expose :id
      end
      objects = [model_class.new(1), model_class.new(2)]
      options = Grape::Entity::Options.new(
        partial: true,
        partial_errors: [],
        collection: true,
        on_error: :omit
      )

      allow(entity).to receive(:new).and_wrap_original do |original, object, opts|
        presenter = original.call(object, opts)
        allow(presenter).to receive(:presented).and_raise(StandardError, 'element boom') if object.id == 2
        presenter
      end

      values = entity.send(:represent_collection, objects, options)

      expect(values).to eq([{ id: 1 }])
      expect(options[:partial_errors].map(&:path)).to eq([[1]])
    end

    it 'renders other fields when a nested collection fails mid-way' do
      container_model = Class.new do
        attr_reader :items

        def initialize(items)
          @items = items
          @items_calls = 0
        end

        def items
          @items_calls += 1
          @items
        end

        def items_calls
          @items_calls
        end

        def title
          'container'
        end
      end

      nested_item_entity = item_entity
      container_entity = Class.new(Grape::Entity) do
        expose :title
        expose :items, using: nested_item_entity
      end

      container = container_model.new(items)
      result = container_entity.represent_partial(container)

      expect(result.value).to eq(
        title: 'container',
        items: [
          { id: 1, label: 'label-1' },
          { id: 2, label: nil },
          { id: 3, label: 'label-3' }
        ]
      )
      expect(result.errors.map(&:path)).to eq([[:items, 1, :label]])
      expect(container.items_calls).to eq(1)
    end
  end

  describe 'getter evaluation' do
    it 'evaluates a failing getter only once per representation round' do
      fresh_class.expose :id
      fresh_class.expose :broken, format_with: :timestamp
      fresh_class.format_with(:timestamp) { |value| value.to_s }

      model = model_class.new
      result = fresh_class.represent_partial(model)

      expect(result.value).to eq(id: 1, broken: nil)
      expect(model.getter_calls[:broken]).to eq(1)
    end

    it 'evaluates a plain delegated getter only once even with expose_nil: false' do
      fresh_class.expose :id
      fresh_class.expose :name, expose_nil: false

      model = model_class.new
      allow(model).to receive(:name).and_call_original

      result = fresh_class.represent_partial(model)

      expect(result.value).to eq(id: 1, name: 'name-1')
      expect(model).to have_received(:name).once
    end
  end

  describe 'error reporting failures' do
    it 'propagates the original exception when the :as proc fails' do
      fresh_class.expose :id
      fresh_class.expose :name, as: ->(_m, _o) { raise StandardError, 'as boom' }

      expect { fresh_class.represent_partial(model_class.new) }
        .to raise_error(StandardError, 'as boom')
    end

    it 'propagates the original exception when the :attr_path proc fails' do
      fresh_class.expose :id
      fresh_class.expose :name, attr_path: ->(_m, _o) { raise StandardError, 'path boom' }

      expect { fresh_class.represent_partial(model_class.new) }
        .to raise_error(StandardError, 'path boom')
    end
  end

  describe 'nested entities' do
    it 'collects errors of nested entities under their external alias' do
      nested = Class.new(Grape::Entity) do
        expose :id
        expose :broken, as: :oops
      end

      outer = Class.new(Grape::Entity) do
        expose :id
        expose :child, as: :kid, using: nested
      end

      child_model = model_class.new(2)
      parent = Object.new
      def parent.id; 1; end
      parent.define_singleton_method(:child) { child_model }

      result = outer.represent_partial(parent)

      expect(result.value).to eq(id: 1, kid: { id: 2, oops: nil })
      expect(result.errors.map(&:path)).to eq([[:kid, :oops]])
    end
  end
end
