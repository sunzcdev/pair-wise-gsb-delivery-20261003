# frozen_string_literal: true

require 'ostruct'
require 'thread'

describe 'render snapshot mode' do
  # A controllable synchronization point: the first object of a render blocks in its field
  # extractor until the test thread signals it to continue. The second object passes straight
  # through, so the collection keeps being output object by object.
  class SyncPoint
    def initialize
      @mutex = Mutex.new
      @arrived = ConditionVariable.new
      @proceed = ConditionVariable.new
      @arrived_flag = false
    end

    def wait_at_point(id)
      @mutex.synchronize do
        unless @arrived_flag
          @arrived_flag = true
          @arrived.signal
          @proceed.wait(@mutex)
        end
      end
      id
    end

    def wait_until_arrived
      @mutex.synchronize do
        @arrived.wait(@mutex) until @arrived_flag
      end
    end

    def release
      @mutex.synchronize { @proceed.broadcast }
    end

    def join_thread(thread)
      thread.join(5) or raise 'render thread did not finish within 5 seconds'
    end
  end

  def build_item(id:, label:, category_name:)
    { id: id, label: label, category: { name: category_name, code: 'old-code' } }
  end

  # Builds a pair of blueprints with an association view that can be mutated mid-render.
  def build_blueprints
    category_blueprint = Class.new(Blueprinter::Base) do
      identifier :id
      field :name

      view :extended do
        field :code
      end
    end

    root_blueprint = Class.new(Blueprinter::Base) do
      identifier :id
      field :label
      association :category, blueprint: category_blueprint, view: :extended
      view(:extended) { field :extended_flag }
      field :_sync_point, name: :sync, exclude_if_nil: true do |item, **opts|
        point = opts[:sync_point]
        point&.wait_at_point(item[:id])
        nil
      end
    end

    [root_blueprint, category_blueprint]
  end

  around do |example|
    previous_sort = Blueprinter.configuration.sort_fields_by
    Blueprinter.configuration.sort_fields_by = :definition
    example.run
    Blueprinter.configuration.sort_fields_by = previous_sort
  end

  describe '::render_as_hash with snapshot: true' do
    let(:items) do
      [
        build_item(id: 1, label: 'one', category_name: 'Books'),
        build_item(id: 2, label: 'two', category_name: 'Games')
      ]
    end

    it 'pins fields and the association view for the whole render' do
      sync_point = SyncPoint.new
      root_blueprint, category_blueprint = build_blueprints

      rendered = nil
      renderer = Thread.new do
        rendered = root_blueprint.render_as_hash(items, snapshot: true, sync_point: sync_point)
      end

      sync_point.wait_until_arrived

      # Redefine a field and the associated view while the first object's extractor is paused.
      root_blueprint.field :label, name: :renamed_label
      root_blueprint.field :added_mid_render
      category_blueprint.view(:extended) { category_blueprint.field :added_to_association }

      sync_point.release
      sync_point.join_thread(renderer)

      expect(rendered).to eq(
        [
          { id: 1, label: 'one', category: { id: nil, name: 'Books', code: 'old-code' } },
          { id: 2, label: 'two', category: { id: nil, name: 'Games', code: 'old-code' } }
        ]
      )

      # The definition updates are visible to the next top-level render.
      next_render = root_blueprint.render_as_hash(items)
      expect(next_render.map(&:keys)).to all(include(:renamed_label, :added_mid_render))
      expect(next_render.first[:category]).to include(added_to_association: nil)
    end

    it 'renders a collection against two field sets when snapshot mode is off' do
      sync_point = SyncPoint.new
      root_blueprint, _category_blueprint = build_blueprints

      rendered = nil
      renderer = Thread.new do
        rendered = root_blueprint.render_as_hash(items, view: :extended, sync_point: sync_point)
      end

      sync_point.wait_until_arrived
      root_blueprint.view(:extended) { root_blueprint.field :added_mid_render }
      sync_point.release
      sync_point.join_thread(renderer)

      expect(rendered.first).not_to include(:added_mid_render)
      expect(rendered.last).to include(:added_mid_render)
      expect(rendered.first).to include(extended_flag: nil)
    end
  end

  describe 'pinning' do
    it 'pins a nested or dynamically selected Blueprint when first encountered' do
      nested = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        field :mutation_trigger, exclude_if_nil: true do |child, **_options|
          nested.field(:added_after_first_encounter) if child[:id] == 10
          nil
        end
      end
      root = Class.new(Blueprinter::Base) do
        identifier :id
        association :child, blueprint: nested
      end

      parent = { id: 1, child: [{ id: 10, name: 'first' }, { id: 11, name: 'second' }] }

      rendered = root.render_as_hash(parent, snapshot: true)

      expect(rendered[:child]).to eq(
        [
          { id: 10, name: 'first' },
          { id: 11, name: 'second' }
        ]
      )
      expect(root.render_as_hash(parent)[:child].first).to include(added_after_first_encounter: nil)
    end

    it 'pins field exclusions and included views, not just the final field names' do
      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        field :secret

        view :extras do
          field :extra
        end

        view :public do
          include_view :extras
          exclude :secret
        end
      end

      object = { id: 1, name: 'thing', secret: 'shh', extra: 'bonus' }
      sync_point = SyncPoint.new
      blueprint.field :_sync_point, exclude_if_nil: true do |item, **_opts|
        sync_point.wait_at_point(item[:id])
        nil
      end

      rendered = nil
      renderer = Thread.new do
        rendered = blueprint.render_as_hash([object, object], view: :public, snapshot: true,
                                             sync_point: sync_point)
      end

      sync_point.wait_until_arrived
      blueprint.view(:public) { blueprint.field :late_field }
      blueprint.view(:extras) { blueprint.field :late_extra }
      sync_point.release
      sync_point.join_thread(renderer)

      expect(rendered).to eq(
        [
          { id: 1, name: 'thing', extra: 'bonus' },
          { id: 1, name: 'thing', extra: 'bonus' }
        ]
      )

      next_render = blueprint.render_as_hash(object, view: :public)
      expect(next_render).to include(late_field: nil, late_extra: nil)
      expect(next_render).not_to include(:secret)
    end

    it 'pins transformer order' do
      calls = []
      first_transformer = Class.new(Blueprinter::Transformer) do
        define_singleton_method(:transform) do |hash, _object, _options|
          calls << :first
          hash[:first] = true
        end
      end
      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        transform first_transformer
        field :_sync_point, exclude_if_nil: true do |object, **options|
          options[:sync_point]&.wait_at_point(object[:id])
          nil
        end
      end
      late_transformer = Class.new(Blueprinter::Transformer) do
        define_singleton_method(:transform) do |hash, _object, _options|
          calls << :late
          hash[:late] = true
        end
      end

      sync_point = SyncPoint.new
      object = { id: 1 }
      rendered = nil
      renderer = Thread.new do
        rendered = blueprint.render_as_hash([object, object], snapshot: true, sync_point: sync_point)
      end

      sync_point.wait_until_arrived
      blueprint.transform(late_transformer)
      sync_point.release
      sync_point.join_thread(renderer)

      expect(rendered).to eq([{ id: 1, first: true }, { id: 1, first: true }])
      expect(calls).to eq(%i[first first])

      expect(blueprint.render_as_hash(object)).to include(first: true, late: true)
    end
  end

  describe 'state lifecycle' do
    it 'releases the pinned state when the render raises' do
      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
      end
      object = OpenStruct.new(id: 1)

      calls = 0
      original = blueprint.method(:hashify)
      blueprint.singleton_class.send(:define_method, :hashify) do |object, **kwargs|
        calls += 1
        raise StandardError, 'boom' if calls == 1

        original.call(object, **kwargs)
      end

      expect do
        blueprint.render_as_hash(object, snapshot: true)
      end.to raise_error(StandardError, 'boom')

      expect(Blueprinter::RenderSnapshot.current).to be_nil

      blueprint.field :after_the_failure
      expect(blueprint.render_as_hash(object, snapshot: true)).to eq(id: 1, after_the_failure: nil)
    end

    it 'does not let two concurrent renders overwrite one another' do
      sync_point = SyncPoint.new
      shared_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        field :_sync_point, exclude_if_nil: true do |object, **options|
          options[:sync_point]&.wait_at_point(object[:id])
          nil
        end
      end

      items = [{ id: 1, name: 'one' }, { id: 2, name: 'two' }]

      results = {}
      threads = []

      threads << Thread.new do
        results[:snapshot] = shared_blueprint.render_as_hash(items, snapshot: true, sync_point: sync_point)
      end
      threads << Thread.new do
        # The live render starts only once the snapshot render has pinned its first object, so the
        # added field must never leak into the snapshot result.
        sync_point.wait_until_arrived
        shared_blueprint.field(:from_concurrent_render)
        results[:live] = shared_blueprint.render_as_hash(items)
        sync_point.release
      end

      threads.each { |thread| sync_point.join_thread(thread) }

      expect(results[:snapshot]).to eq(
        [{ id: 1, name: 'one' }, { id: 2, name: 'two' }]
      )
      expect(results[:live].first).to include(from_concurrent_render: nil)
    end

    it 'does not freeze or mutate the caller objects and options' do
      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
      end
      object = OpenStruct.new(id: 1, name: 'thing')
      options = { snapshot: true, view: :default, custom: 'passthrough' }

      rendered = blueprint.render_as_hash(object, options)

      expect(rendered).to eq(id: 1, name: 'thing')
      expect(object).not_to be_frozen
      expect(options).to eq(snapshot: true, view: :default, custom: 'passthrough')
      expect(options).not_to be_frozen
      expect(Blueprinter::RenderSnapshot.current).to be_nil
    end
  end
end
