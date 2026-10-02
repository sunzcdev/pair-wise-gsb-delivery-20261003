# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  describe QuerySet do
    # A rich document covering arrays, objects, nested projections, filters,
    # functions and expression references.
    let(:data) do
      {
        'people' => [
          { 'name' => 'a', 'age' => 10, 'email' => 'a@x', 'active' => true,  'tags' => %w[x],   'addr' => { 'city' => 'NY' } },
          { 'name' => 'b', 'age' => 20, 'email' => 'b@x', 'active' => false, 'tags' => [],      'addr' => { 'city' => 'LA' } },
          { 'name' => 'c', 'age' => 30, 'email' => 'c@x', 'active' => true,  'tags' => %w[y z], 'addr' => { 'city' => 'NY' } },
          { 'name' => 'd', 'age' => 70, 'email' => 'd@x', 'active' => true,  'tags' => [],      'addr' => {} }
        ],
        'foo'    => { 'bar' => { 'baz' => 'deep' } },
        'nums'   => [1, 2, 3, 4],
        'matrix' => [[1, 2], [3, 4], [5]],
        'str'    => 'hello',
        'obj'    => { 'a' => 1, 'b' => 2 }
      }
    end

    # A broad battery of expression shapes.
    EXPRESSIONS = [
      'foo.bar.baz', 'foo|bar|baz', 'foo.bar|baz', 'foo|bar.baz',
      'people[*].name', 'people[*].age', 'people[*].email',
      'people[?age > `18`].name', 'people[?age > `18`].email',
      'people[?age > `65`].name', 'people[?active].name',
      'people[?active==`true`].name', 'people[?age >= `20`].addr.city',
      'people[0].name', 'people[-1].name', 'people[1:3].name',
      'nums[0:2]', 'nums[*]', 'matrix[]', 'matrix[].name',
      'length(people)', 'length(foo)', 'length(str)', 'avg(nums)', 'sum(nums)',
      'max(nums)', 'min(nums)', 'join(`,`, people[*].name)',
      'map(&age, people)', 'map(&name, people)',
      'sort_by(people, &age)[0].name', 'sort_by(people, &age)[-1].age',
      'max_by(people, &age).email', 'min_by(people, &age).name',
      'people[?age > `18`] | [0].name', 'people[?age > `18`][0].name',
      'nums | [0]', 'nums[0]', 'obj.*', 'obj.* | [0]',
      'foo.*', 'foo.*.baz', 'people[].tags', 'people[].tags | [0]',
      'type(foo)', 'abs(`-5`)', 'active', 'people[?tags].name',
      'people[?addr.city == `NY`].name',
      'people[?age > `18` && active].name',
      'people[?age < `18` || age > `60`].name', 'age',
      'people[*].[name, age]', 'people[*].{n: name, y: age}',
      'nums[?@ > `2`]', 'reverse(nums)', 'keys(foo)', 'values(obj)',
      'not_null(missing, foo)', 'people[*].name | [0]', 'foo.bar | baz',
      'people[?age > `18`].name | [0]', 'nums[?@ > `2`] | length(@)',
      'people[*].{n: name, e: email, city: addr.city}',
      'people[?age > `18`].[name, email]'
    ].freeze

    describe 'correctness vs independent JMESPath.search' do
      EXPRESSIONS.each_with_index do |expression, i|
        it "matches for #{expression.inspect}" do
          qs = QuerySet.new("q#{i}": expression)
          expect(qs.search(data)["q#{i}".to_sym]).to eq(JMESPath.search(expression, data))
        end
      end

      it 'matches for every expression fused together at once' do
        queries = EXPRESSIONS.each_with_index.map { |e, i| ["q#{i}".to_sym, e] }.to_h
        result = QuerySet.new(queries).search(data)
        EXPRESSIONS.each_with_index do |e, i|
          expect(result["q#{i}".to_sym]).to eq(JMESPath.search(e, data))
        end
      end

      it 'matches for many random multi-query groupings' do
        rng = Random.new(7)
        150.times do
          size = rng.rand(1..8)
          chosen = EXPRESSIONS.sample(size, random: rng)
          queries = chosen.each_with_index.map { |e, i| ["q#{i}".to_sym, e] }.to_h
          result = QuerySet.new(queries).search(data)
          chosen.each_with_index do |e, i|
            expect(result["q#{i}".to_sym]).to eq(JMESPath.search(e, data))
          end
        end
      end
    end

    describe 'construction / API' do
      it 'is exposed through JMESPath.query_set and JMESPath.search_all' do
        qs = JMESPath.query_set(a: 'foo')
        expect(qs).to be_a(QuerySet)
        result = JMESPath.search_all({ a: 'foo' }, 'foo' => 1)
        expect(result).to be_a(QuerySet::Result)
        expect(result[:a]).to eq(1)
      end

      it 'is exposed through a Runtime sharing its parser' do
        runtime = JMESPath::Runtime.new
        qs = runtime.query_set(a: 'foo')
        expect(qs.search('foo' => 7)[:a]).to eq(7)
      end

      it 'accepts an explicit hash' do
        qs = QuerySet.new(a: 'foo', b: 'bar')
        expect(qs.search('foo' => 1, 'bar' => 2).to_h).to eq(a: 1, b: 2)
      end

      it 'accepts a block form' do
        qs = QuerySet.new do |q|
          q.add(:a, 'foo')
          q.add(:b, 'bar')
        end
        expect(qs.search('foo' => 1, 'bar' => 2).to_h).to eq(a: 1, b: 2)
      end

      it 'accepts on_error together with inline names' do
        qs = QuerySet.new(a: 'foo', on_error: :nil)
        expect(qs.search('foo' => 9)[:a]).to eq(9)
      end

      it 'rejects an unknown on_error policy' do
        expect { QuerySet.new({ a: 'foo' }, on_error: :explode) }
          .to raise_error(ArgumentError)
      end

      it 'raises a syntax error at build time under :raise' do
        expect { QuerySet.new(a: 'foo..bar') }
          .to raise_error(Errors::SyntaxError)
      end

      it 'is reusable across many documents' do
        qs = QuerySet.new(adults: 'people[?age > `18`].name')
        3.times do |n|
          doc = { 'people' => [{ 'name' => 'x', 'age' => n * 20 }] }
          expect(qs.search(doc)[:adults]).to eq(JMESPath.search('people[?age > `18`].name', doc))
        end
      end
    end

    describe 'Result protocol' do
      let(:result) { QuerySet.new(a: 'a', b: 'b').search('a' => 1, 'b' => 2) }

      it 'supports [] and fetch' do
        expect(result[:a]).to eq(1)
        expect(result.fetch(:b)).to eq(2)
      end

      it 'is enumerable in insertion order' do
        expect(result.map { |k, v| [k, v] }).to eq([[:a, 1], [:b, 2]])
      end

      it 'supports key?, keys and to_h' do
        expect(result.key?(:a)).to be(true)
        expect(result.keys).to eq(%i[a b])
        expect(result.to_h).to eq(a: 1, b: 2)
      end
    end

    describe 'error policy' do
      let(:doc) { { 'foo' => { 'not' => 'number' } } }

      it ':raise aborts the whole group' do
        qs = QuerySet.new(good: 'foo', bad: 'abs(foo)')
        expect { qs.search(doc) }.to raise_error(Errors::InvalidTypeError)
      end

      it ':collect omits the failed query but keeps the others' do
        qs = QuerySet.new({ good: 'foo', bad: 'abs(foo)' }, on_error: :collect)
        result = qs.search(doc)
        expect(result.to_h).to eq(good: { 'not' => 'number' })
        expect(result.keys).to eq([:good])
        expect(result.errors).to include(:bad)
        expect(result.successful?(:good)).to be(true)
        expect(result.successful?(:bad)).to be(false)
      end

      it ':collect yields failures to an optional block' do
        qs = QuerySet.new({ good: 'foo', bad: 'abs(foo)' }, on_error: :collect)
        seen = []
        qs.search(doc) { |name, error| seen << [name, error.class] }
        expect(seen).to eq([[:bad, Errors::InvalidTypeError]])
      end

      it ':nil keeps the failed name with a nil value' do
        qs = QuerySet.new({ good: 'foo', bad: 'abs(foo)' }, on_error: :nil)
        result = qs.search(doc)
        expect(result.to_h).to eq(good: { 'not' => 'number' }, bad: nil)
        expect(result.errors).to include(:bad)
      end

      it 'isolates per-element failures inside projections in collect mode' do
        # A field projection never raises, while length(@) on an integer does.
        doc = { 'nums' => [{ 'name' => 'a' }, 2, { 'name' => 'c' }] }
        qs = QuerySet.new({ good: 'nums[].name', bad: 'nums[].length(@)' }, on_error: :collect)
        result = qs.search(doc)
        expect(result[:good]).to eq(%w[a c])
        expect(result.errors).to include(:bad)
        expect(result.errors).not_to include(:good)
      end
    end
  end
end