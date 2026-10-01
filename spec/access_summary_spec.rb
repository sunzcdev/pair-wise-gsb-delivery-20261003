# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  describe AccessSummary do
    W = AccessSummary::WILDCARD

    def summarize(expression)
      JMESPath.access_summary(expression)
    end

    def paths(summary, certainty = nil)
      accesses = certainty ? summary.accesses.select { |a| a.certainty == certainty } : summary.accesses
      accesses.map(&:path)
    end

    describe 'field chains' do
      it 'reports simple field chains as required fields' do
        summary = summarize('foo.bar.baz')
        expect(paths(summary, :required)).to include(%w[foo bar baz])
        expect(summary.required.first.kind).to eq(:field)
      end

      it 'reports indices' do
        summary = summarize('matrix[1][0]')
        expect(paths(summary)).to include(['matrix', 1, 0])
      end
    end

    describe 'required vs possible' do
      it 'marks the right side of || as possible' do
        summary = summarize('a || b.c')
        expect(paths(summary, :required)).to include(['a'])
        expect(paths(summary, :possible)).to include(['b'], ['b', 'c'])
      end

      it 'marks the right side of && as possible' do
        summary = summarize('foo.bar && foo.baz')
        expect(paths(summary, :required)).to include(%w[foo bar])
        expect(paths(summary, :possible)).to include(%w[foo baz])
      end

      it 'marks filter projection results as possible' do
        summary = summarize('users[?age > `20`].name')
        expect(paths(summary, :possible)).to include(['users', W, 'name'])
      end
    end

    describe 'filter conditions' do
      it 'includes fields read by filter tests, not just output fields' do
        summary = summarize('users[?age > `20`].name')
        filter_paths = summary.accesses.select { |a| a.context == :filter }.map(&:path)
        expect(filter_paths).to include(['users', W, 'age'])
      end

      it 'includes both sides of a filter comparison' do
        summary = summarize('users[?age > `20` && tags].name')
        filter_paths = summary.accesses.select { |a| a.context == :filter }.map(&:path)
        expect(filter_paths).to include(['users', W, 'age'], ['users', W, 'tags'])
      end

      it 'conservatively marks equality filters as deep reads' do
        summary = summarize('orders[?lines == `[]`].id')
        subtree = summary.accesses.find { |a| a.kind == :subtree }
        expect(subtree).not_to be_nil
        expect(subtree.path).to eq(['orders', W, 'lines'])
        expect(subtree.reason).to match(/deep-read/)
      end
    end

    describe 'sort_by and expression references' do
      it 'reports the sort key expression applied to every element' do
        summary = summarize('sort_by(orders, &total)[*].id')
        sort_keys = summary.accesses.select { |a| a.context == :sort_key }
        expect(sort_keys.map(&:path)).to include(['orders', W, 'total'])
        expect(paths(summary)).to include(['orders', W, 'id'])
      end

      it 'reports max_by/min_by expression references' do
        summary = summarize('max_by(orders, &total).id')
        sort_keys = summary.accesses.select { |a| a.context == :sort_key }
        expect(sort_keys.map(&:path)).to include(['orders', W, 'total'])
        expect(paths(summary)).to include(['orders', W, 'id'])
      end
    end

    describe 'multi-select structures' do
      it 'reports accesses inside multi-select hashes' do
        summary = summarize('users[0].{name: name, email: contact.email}')
        expect(paths(summary)).to include(
          ['users', 0, 'name'], ['users', 0, 'contact'], ['users', 0, 'contact', 'email']
        )
      end

      it 'reports accesses inside multi-select lists' do
        summary = summarize('users[0].[name, age]')
        expect(paths(summary)).to include(['users', 0, 'name'], ['users', 0, 'age'])
      end
    end

    describe 'wildcards and conservative ranges' do
      it 'reports array projections as wildcards' do
        summary = summarize('users[*].name')
        wildcard = summary.wildcards.find { |a| a.path == ['users'] }
        expect(wildcard.detail).to eq(:array_elements)
        expect(paths(summary)).to include(['users', W, 'name'])
      end

      it 'reports object wildcards' do
        summary = summarize('foo.*.bar')
        wildcard = summary.wildcards.find { |a| a.path == ['foo'] }
        expect(wildcard.detail).to eq(:object_values)
        expect(paths(summary)).to include(['foo', W, 'bar'])
      end

      it 'reports flatten over both nesting levels' do
        summary = summarize('items[].name')
        expect(paths(summary)).to include(['items', W, 'name'], ['items', W, W, 'name'])
      end

      it 'reports slices as conservative ranges with a reason' do
        summary = summarize('users[0:2].name')
        wildcard = summary.wildcards.find { |a| a.path == ['users'] }
        expect(wildcard.detail).to eq(:array_range)
        expect(wildcard.reason).to match(/slice/)
      end

      it 'reports indices into projected lists as conservative wildcards' do
        summary = summarize('users[*] | [0]')
        wildcard = summary.wildcards.find { |a| a.path == ['users', W] }
        expect(wildcard).not_to be_nil
        expect(wildcard.reason).to match(/depends on the data/)
      end

      it 'reports keys() as an object-keys wildcard' do
        summary = summarize('keys(meta)')
        wildcard = summary.wildcards.find { |a| a.path == ['meta'] }
        expect(wildcard.detail).to eq(:object_keys)
      end

      it 'reports to_string() on containers as a subtree read with a reason' do
        summary = summarize('to_string(config)')
        subtree = summary.wildcards.find { |a| a.path == ['config'] && a.kind == :subtree }
        expect(subtree).not_to be_nil
        expect(subtree.reason).to match(/serializes the entire value/)
      end

      it 'reports merge() as a subtree read of its arguments' do
        summary = summarize('merge(a, b)')
        subtrees = summary.wildcards.select { |a| a.kind == :subtree }.map(&:path)
        expect(subtrees).to include(['a'], ['b'])
      end
    end

    describe 'Summary#to_h' do
      it 'returns plain data grouped by certainty and wildcards' do
        hash = summarize('users[?age > `20`].name').to_h
        expect(hash.keys).to contain_exactly('required', 'possible', 'wildcards')
        required_paths = hash['required'].map { |entry| entry['path'] }
        expect(required_paths).to include('users', 'users[*].age')
        possible_paths = hash['possible'].map { |entry| entry['path'] }
        expect(possible_paths).to include('users[*].name')
      end
    end

    describe 'Runtime#access_summary' do
      it 'is available on runtimes' do
        runtime = JMESPath::Runtime.new(cache_expressions: false)
        summary = runtime.access_summary('foo.bar')
        expect(summary).to be_a(AccessSummary::Summary)
        expect(paths(summary)).to include(%w[foo bar])
      end
    end

    describe 'no under-reporting against actual reads' do
      ITERATE = AccessSummary::ITERATE

      # Wraps data in Hash/Array subclasses that record every read of a
      # key, index, or full-container iteration, together with the path
      # from the document root.
      class AccessRecorder
        def self.wrap(value, path = [], log = [])
          case value
          when Hash
            hash = RecHash.new(path, log)
            value.each { |k, v| hash[k] = wrap(v, path + [k], log) }
            hash
          when Array
            array = RecArray.new(path, log)
            value.each { |v| array << wrap(v, path + [array.size], log) }
            array
          else
            value
          end
        end

        class RecHash < Hash
          def initialize(path, log)
            @path = path
            @log = log
            super()
          end

          def [](key)
            @log << (@path + [key])
            super
          end

          def values
            @log << (@path + [ITERATE])
            super
          end

          def keys
            @log << (@path + [ITERATE])
            super
          end

          def each_pair(&block)
            @log << (@path + [ITERATE])
            super
          end

          def to_json(*args)
            @log << (@path + [ITERATE])
            super
          end
        end

        class RecArray < Array
          def initialize(path, log)
            @path = path
            @log = log
            super()
          end

          def [](index, *rest)
            @log << (@path + (rest.empty? && index.is_a?(Integer) ? [index] : [ITERATE]))
            super
          end

          def first
            @log << (@path + [0])
            super
          end

          # Util.falsey? calls entries.any? as an emptiness (cardinality)
          # check; like #size and #empty? it inspects no content, so it
          # is not logged as a read
          def entries
            to_a
          end

          %i[each map inject each_with_object sort_by any? all? find
             compact reverse join to_json].each do |method_name|
            define_method(method_name) do |*args, &block|
              @log << (@path + [ITERATE])
              super(*args, &block)
            end
          end
        end
      end

      DATA = {
        'users' => [
          { 'name' => 'ann', 'age' => 34, 'contact' => { 'email' => 'a@x' }, 'tags' => %w[admin ops] },
          { 'name' => 'bob', 'age' => 17, 'contact' => { 'email' => 'b@x' }, 'tags' => [] },
          { 'name' => 'cid', 'age' => 52, 'contact' => {} }
        ],
        'orders' => [
          { 'id' => 1, 'total' => 30, 'lines' => [{ 'sku' => 'a', 'qty' => 2 }] },
          { 'id' => 2, 'total' => 10, 'lines' => [] }
        ],
        'config' => { 'features' => { 'a' => true, 'b' => false }, 'level' => 3 },
        'matrix' => [[1, 2], [3, 4]],
        'meta' => { 'x' => 1, 'y' => 2 }
      }.freeze

      EXPRESSIONS = [
        'users[?age > `20`].{name: name, email: contact.email}',
        'users[?age > `20` && tags].name',
        'users[?contact.email == `"a@x"`].age',
        'orders[?lines == `[]`].id',
        'sort_by(orders, &total)[*].{id: id, total: total}',
        'max_by(orders, &total).id',
        'min_by(orders, &total)',
        'map(&id, orders)',
        'orders[].lines[].sku',
        'users[].tags[]',
        'matrix[1][0]',
        'users[0:2].name',
        'users[::-1]',
        'config.*',
        'config.features.* | [0]',
        'users[*].tags[0]',
        'users[*] | [0]',
        'users[?age > `20`].age | [0]',
        'not_null(users[0].nick, users[0].name)',
        'config.level > `2` && meta.x || meta.y',
        'users[0].[name, age]',
        '{names: users[*].name, count: length(users)}',
        'keys(meta)',
        'values(meta)',
        'to_string(config)',
        'sum(orders[*].total)',
        'contains(users[0].tags, `"admin"`)',
        'users[0].[name, missing, contact.email]'
      ].freeze

      EXPRESSIONS.each do |expression|
        it "accounts for every read of #{expression}" do
          log = []
          data = AccessRecorder.wrap(DATA, [], log)
          JMESPath.search(expression, data)
          summary = JMESPath.access_summary(expression)

          uncovered = log.reject { |path| summary.covers?(path) }
          expect(uncovered).to be_empty,
                               "summary for #{expression} missed actual reads:\n" \
                               "#{uncovered.map(&:inspect).join("\n")}\n" \
                               "summary:\n#{summary.accesses.map(&:to_s).join("\n")}"
        end
      end

      it 'would catch an implementation that only reports output fields' do
        # sanity check on the recorder itself: the filter field `age` is
        # actually read even though it is not part of the result
        log = []
        data = AccessRecorder.wrap(DATA, [], log)
        JMESPath.search('users[?age > `20`].name', data)
        expect(log).to include(['users', 0, 'age'])
        expect(log).not_to include(['users', 1, 'name']) # filtered out
      end
    end
  end
end
