# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  describe AccessSummary do
    def summarize(expression)
      AccessSummary.analyze(expression)
    end

    describe 'field classification' do
      it 'marks plain field chains as required' do
        summary = summarize('foo.bar.baz')
        expect(summary.required_paths).to eq(%w[foo foo.bar foo.bar.baz])
        expect(summary.possible_paths).to eq([])
        expect(summary.wildcard_ranges).to eq([])
      end

      it 'includes indices in paths' do
        summary = summarize('items[0].name')
        expect(summary.required_paths).to eq(['items', 'items[0]', 'items[0].name'])
      end

      it 'marks filter conditions and filtered projections as possible' do
        summary = summarize('items[?price > `10`].name')
        expect(summary.required_paths).to contain_exactly('items', 'items[*]')
        expect(summary.possible_paths).to include('items[*].price')
        expect(summary.possible_paths).to include('items[*].name')
        range = summary.wildcard_ranges.find { |e| e.path == ['items', :*] }
        expect(range).not_to be_nil
        expect(range.reason).to match(/projection/)
      end

      it 'covers both sides of filter condition boolean operators' do
        summary = summarize('items[?a && b].c')
        expect(summary.possible_paths).to include('items[*].a', 'items[*].b', 'items[*].c')
      end

      it 'marks the right side of or/and as possible' do
        summary = summarize('a || b')
        expect(summary.required_paths).to eq(['a'])
        expect(summary.possible_paths).to eq(['b'])
      end

      it 'marks both comparator operands as required' do
        summary = summarize('foo == bar')
        expect(summary.required_paths).to include('foo', 'bar')
      end

      it 'covers every value of multi-select hashes' do
        summary = summarize('{x: foo, y: bar.baz}')
        expect(summary.required_paths).to include('foo', 'bar', 'bar.baz')
      end

      it 'covers every element of multi-select lists' do
        summary = summarize('[foo, bar.baz]')
        expect(summary.required_paths).to include('foo', 'bar', 'bar.baz')
      end

      it 'continues analysis through multi-select structures' do
        summary = summarize('{a: foo, b: bar}.a.baz')
        expect(summary.required_paths).to include('foo', 'bar', 'foo.baz')
        expect(summary.required_paths).not_to include('bar.baz')
      end

      it 'reports object projections as wildcard ranges' do
        summary = summarize('settings.*.enabled')
        expect(summary.required_paths).to include('settings', 'settings.*')
        expect(summary.possible_paths).to include('settings.*.enabled')
        range = summary.wildcard_ranges.find { |e| e.path == ['settings', :**] }
        expect(range).not_to be_nil
        expect(range.reason).to match(/object projection/)
      end

      it 'reports slices as wildcard ranges with a reason' do
        summary = summarize('items[1:3].name')
        expect(summary.possible_paths).to include('items[*].name')
        slice = summary.wildcard_ranges.find { |e| e.reason =~ /slice/ }
        expect(slice).not_to be_nil
        expect(slice.path).to eq(['items', :*])
      end

      it 'reports flatten as wildcard ranges with a reason' do
        summary = summarize('matrix[].row')
        expect(summary.possible_paths).to include('matrix[*][*].row')
        expect(summary.wildcard_ranges.map(&:reason).join).to match(/flatten/)
      end

      it 'covers sort_by expression references' do
        summary = summarize('sort_by(items, &price)')
        expect(summary.required_paths).to include('items')
        expect(summary.possible_paths).to include('items[*].price')
        expect(summary.wildcard_ranges.map(&:reason).join).to match(/sort_by/)
      end

      it 'covers map, min_by and max_by expression references' do
        expect(summarize('map(&a, items)').possible_paths).to include('items[*].a')
        expect(summarize('min_by(items, &a)').possible_paths).to include('items[*].a')
        expect(summarize('max_by(items, &a)').possible_paths).to include('items[*].a')
      end

      it 'continues analysis on element-aliasing function results' do
        summary = summarize('sort_by(items, &price)[0].name')
        expect(summary.possible_paths).to include('items[*].name')
      end

      it 'continues analysis through pipes' do
        summary = summarize('foo | bar.baz')
        expect(summary.required_paths).to include('foo', 'foo.bar', 'foo.bar.baz')
      end

      it 'treats scalar function results as opaque' do
        summary = summarize('length(foo)')
        expect(summary.required_paths).to eq(['foo'])
      end

      it 'falls back to conservative unknowns for unregistered functions' do
        custom = Class.new(Nodes::Function)
        Nodes::Function::FUNCTIONS['custom_fn'] = custom
        begin
          summary = summarize('custom_fn(foo).bar')
          expect(summary.required_paths).to include('foo')
          expect(summary.conservative).not_to be_empty
          expect(summary.conservative.map(&:reason).join).to match(/custom_fn/)
          expect(summary.cover?(%w[anything at all])).to be(true)
        ensure
          Nodes::Function::FUNCTIONS.delete('custom_fn')
        end
      end

      it 'analyzes pre-compiled ASTs' do
        ast = Parser.new.parse('foo.bar')
        expect(AccessSummary.analyze(ast).required_paths).to include('foo.bar')
      end

      it 'does not require data to analyze' do
        summary = JMESPath.access_summary('items[?price > `10`].name')
        expect(summary).to be_a(AccessSummary::Summary)
        expect(summary.to_h['possible']).to include('items[*].price')
      end
    end

    describe 'actual access recording (no under-reporting)' do
      # Hash/Array subclasses that record every key/index read performed
      # by the expression evaluator, along with the path of the read.
      class AccessRecorder
        class RecordingHash < Hash
          def initialize(recorder, path)
            super()
            @recorder = recorder
            @path = path
          end

          def [](key)
            @recorder.record(@path + [key])
            super
          end
        end

        class RecordingArray < Array
          def initialize(recorder, path)
            super()
            @recorder = recorder
            @path = path
          end

          def [](index, *args)
            @recorder.record(@path + [index])
            super
          end
        end

        attr_reader :accesses

        def initialize(data)
          @accesses = []
          @data = wrap(data, [])
        end

        def record(path)
          @accesses << path
        end

        def search(expression)
          JMESPath.search(expression, @data)
        end

        private

        def wrap(value, path)
          case value
          when Hash
            wrapped = RecordingHash.new(self, path)
            value.each { |k, v| wrapped[k] = wrap(v, path + [k]) }
            wrapped
          when Array
            wrapped = RecordingArray.new(self, path)
            value.each_with_index { |v, i| wrapped[i] = wrap(v, path + [i]) }
            wrapped
          else
            value
          end
        end
      end

      # Each expression is exercised against several branchy inputs so
      # that different conditional paths are taken. Every access recorded
      # at runtime must be covered by the static summary.
      CASES = {
        'items[?price > `10`].name' => [
          { 'items' => [{ 'price' => 5, 'name' => 'a' }, { 'price' => 20, 'name' => 'b' }] },
          { 'items' => [{ 'price' => 50, 'name' => 'c' }] },
          { 'items' => [] },
          {}
        ],
        'items[?a && b].c' => [
          { 'items' => [{ 'a' => true, 'b' => true, 'c' => 1 }, { 'a' => false, 'c' => 2 }] },
          { 'items' => [{ 'a' => true, 'b' => false }] }
        ],
        'user.name || user.email || fallback.name' => [
          { 'user' => { 'name' => 'n', 'email' => 'e' }, 'fallback' => { 'name' => 'f' } },
          { 'user' => { 'email' => 'e' }, 'fallback' => { 'name' => 'f' } },
          { 'user' => {}, 'fallback' => { 'name' => 'f' } }
        ],
        'sort_by(products, &price)[0].name' => [
          { 'products' => [{ 'price' => 3, 'name' => 'x' }, { 'price' => 1, 'name' => 'y' }] },
          { 'products' => [{ 'price' => 1, 'name' => 'z' }] }
        ],
        'max_by(products, &price).name' => [
          { 'products' => [{ 'price' => 3, 'name' => 'x' }, { 'price' => 9, 'name' => 'y' }] }
        ],
        'items[*].{sku: sku, price: price}' => [
          { 'items' => [{ 'sku' => 's1', 'price' => 1 }, { 'sku' => 's2', 'price' => 2 }] }
        ],
        'items[1:3].name' => [
          { 'items' => [{ 'name' => 'a' }, { 'name' => 'b' }, { 'name' => 'c' }, { 'name' => 'd' }] }
        ],
        'settings.*.enabled' => [
          { 'settings' => { 'a' => { 'enabled' => true }, 'b' => { 'enabled' => false } } }
        ],
        'matrix[].row' => [
          { 'matrix' => [[{ 'row' => 1 }], [{ 'row' => 2 }, { 'row' => 3 }]] }
        ],
        'foo | bar.baz' => [
          { 'foo' => { 'bar' => { 'baz' => 1 } } }
        ],
        '{x: foo, y: bar.baz}' => [
          { 'foo' => 1, 'bar' => { 'baz' => 2 } }
        ],
        '{a: foo, b: bar}.a.baz' => [
          { 'foo' => { 'baz' => 1 }, 'bar' => 2 }
        ],
        'items[*].tags[?contains(@, `x`)]' => [
          { 'items' => [{ 'tags' => ['x', 'y'] }, { 'tags' => ['z'] }] }
        ]
      }.freeze

      CASES.each do |expression, data_variants|
        it "covers every recorded access of #{expression.inspect}" do
          summary = summarize(expression)
          data_variants.each do |data|
            recorder = AccessRecorder.new(data)
            recorder.search(expression)
            recorder.accesses.each do |path|
              expect(summary.cover?(path)).to(
                be(true),
                "expected #{expression.inspect} summary to cover access " \
                "#{path.inspect}; summary: #{summary.to_h.inspect}"
              )
            end
          end
        end
      end

      it 'records filter condition reads, not only result fields' do
        recorder = AccessRecorder.new(
          'items' => [{ 'price' => 5, 'name' => 'a' }, { 'price' => 20, 'name' => 'b' }]
        )
        recorder.search('items[?price > `10`].name')
        # sanity: the filter actually read price fields at runtime
        expect(recorder.accesses).to include(%w[items], ['items', 0, 'price'])
        summary = summarize('items[?price > `10`].name')
        expect(summary.cover?(['items', 0, 'price'])).to be(true)
        expect(summary.cover?(['items', 1, 'price'])).to be(true)
        expect(summary.cover?(['items', 1, 'name'])).to be(true)
      end

      it 'does not claim uncovered paths' do
        summary = summarize('items[?price > `10`].name')
        expect(summary.cover?(%w[other])).to be(false)
        expect(summary.cover?(['items', 0, 'sku'])).to be(false)
      end
    end
  end
end
