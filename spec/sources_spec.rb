# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  RSpec.describe '.search_with_sources' do
    def locate(expression, data)
      JMESPath.search_with_sources(expression, data)
    end

    def origin(pointer)
      { 'type' => 'origin', 'pointer' => pointer }
    end

    def derived(inputs, sources = :none)
      hash = { 'type' => 'derived', 'inputs' => inputs }
      hash['sources'] = sources unless sources == :none
      hash
    end

    it 'returns a SearchResult and keeps .search returning plain values' do
      data = { 'foo' => { 'bar' => 'value' } }
      result = locate('foo.bar', data)
      expect(result).to be_a(JMESPath::SearchResult)
      expect(result.value).to eq('value')
      expect(JMESPath.search('foo.bar', data)).to eq('value')
    end

    it 'reports the root and simple field navigation as origins' do
      data = { 'foo' => { 'bar' => 'value' } }
      expect(locate('foo.bar', data).to_h['sources']).to eq(origin('/foo/bar'))
      expect(locate('@', data).value).to equal(data)
      expect(locate('@', data).to_h['sources']).to eq(origin(''))
    end

    it 'escapes slashes and tildes in keys per RFC 6901' do
      data = { 'a/b' => 1, 'c~d' => 2, 'e~/f' => 3 }
      expect(locate('"a/b"', data).to_h['sources']).to eq(origin('/a~1b'))
      expect(locate('"c~d"', data).to_h['sources']).to eq(origin('/c~0d'))
      expect(locate('"e~/f"', data).to_h['sources']).to eq(origin('/e~0~1f'))
    end

    context 'duplicate values' do
      let(:data) do
        { 'people' => [
          { 'name' => 'Pat', 'age' => 40 },
          { 'name' => 'Pat', 'age' => 40 }
        ] }
      end

      it 'does not collapse equal results and keeps each origin distinct' do
        result = locate('people[*].name', data)
        expect(result.value).to eq(%w[Pat Pat])
        expect(result.to_h['sources']).to eq(
          derived(['/people'], [origin('/people/0/name'), origin('/people/1/name')])
        )
      end

      it 'tells duplicate objects apart by position after filtering' do
        result = locate('people[?age >= `40`].name', data)
        expect(result.value).to eq(%w[Pat Pat])
        expect(result.to_h['sources']['sources']).to eq(
          [origin('/people/0/name'), origin('/people/1/name')]
        )
      end
    end

    context 'filtering' do
      let(:data) do
        { 'people' => [
          { 'name' => 'Fred', 'age' => 20 },
          { 'name' => 'James', 'age' => 30 },
          { 'name' => 'Alan', 'age' => 40 },
          { 'name' => 'Zoe', 'age' => nil }
        ] }
      end

      it 'keeps the source array indices instead of the result array indices' do
        result = locate('people[?age > `20`].name', data)
        expect(result.value).to eq(%w[James Alan])
        expect(result.to_h['sources']).to eq(
          derived(['/people'], [origin('/people/1/name'), origin('/people/2/name')])
        )
      end

      it 'marks the projected array as derived from the filtered source array' do
        result = locate('people[?age > `20`]', data)
        expect(result.to_h['sources']['inputs']).to eq(['/people'])
        expect(result.to_h['sources']['sources']).to eq(
          [origin('/people/1'), origin('/people/2')]
        )
      end
    end

    context 'projections and nulls' do
      let(:data) do
        { 'items' => [
          { 'tag' => 'a' },
          { 'tag' => nil },
          nil,
          { 'tag' => 'b' }
        ] }
      end

      it 'omits null projections from both value and sources' do
        result = locate('items[*].tag', data)
        expect(result.value).to eq(%w[a b])
        expect(result.to_h['sources']['sources']).to eq(
          [origin('/items/0/tag'), origin('/items/3/tag')]
        )
      end

      it 'keeps an explicit null when it is navigated directly' do
        result = locate('items[1].tag', data)
        expect(result.value).to be_nil
        expect(result.to_h['sources']).to eq(origin('/items/1/tag'))
      end

      it 'keeps an explicit null element selected by a direct index' do
        result = locate('items[2]', data)
        expect(result.value).to be_nil
        expect(result.to_h['sources']).to eq(origin('/items/2'))
      end
    end

    context 'slices' do
      let(:data) { { 'nums' => [10, 20, 30, 40, 50] } }

      it 'uses the source indices for a forward slice' do
        result = locate('nums[1:4]', data)
        expect(result.value).to eq([20, 30, 40])
        expect(result.to_h['sources']).to eq(
          derived(['/nums'], [origin('/nums/1'), origin('/nums/2'), origin('/nums/3')])
        )
      end

      it 'uses the source indices for a negative step slice' do
        result = locate('nums[::-2]', data)
        expect(result.value).to eq([50, 30, 10])
        expect(result.to_h['sources']['sources']).to eq(
          [origin('/nums/4'), origin('/nums/2'), origin('/nums/0')]
        )
      end
    end

    context 'flatten' do
      let(:data) { { 'matrix' => [[1, 2], [3], [4, 5, 6]] } }

      it 'tracks elements through flatten with nested source indices' do
        result = locate('matrix[]', data)
        expect(result.value).to eq([1, 2, 3, 4, 5, 6])
        expect(result.to_h['sources']['sources']).to eq(
          %w[0/0 0/1 1/0 2/0 2/1 2/2].map { |path| origin("/matrix/#{path}") }
        )
      end

      it 'propagates sources after flattening a derived (filtered) list' do
        data = { 'groups' => [[{ 'v' => 1 }], [{ 'v' => 2 }], [{ 'v' => 3 }]] }
        result = locate('groups[?length(@) > `0`][] | [*].v', data)
        expect(result.value).to eq([1, 2, 3])
        expect(result.to_h['sources']['sources']).to eq(
          [origin('/groups/0/0/v'), origin('/groups/1/0/v'), origin('/groups/2/0/v')]
        )
      end
    end

    context 'sort_by' do
      let(:data) do
        { 'people' => [
          { 'name' => 'Zoe', 'age' => 40, 'id' => 'a' },
          { 'name' => 'Amy', 'age' => 30, 'id' => 'b' },
          { 'name' => 'Mia', 'age' => 20, 'id' => 'c' }
        ] }
      end

      it 'reorders sources with the values and never renumbers them' do
        result = locate("people | sort_by(@, &age)", data)
        expect(result.value.map { |p| p['id'] }).to eq(%w[c b a])
        expect(result.to_h['sources']['sources']).to eq(
          [origin('/people/2'), origin('/people/1'), origin('/people/0')]
        )
      end

      it 'keeps positions after filter then sort_by (result index != source index)' do
        result = locate("people[?age > `20`] | sort_by(@, &age)", data)
        expect(result.value.map { |p| p['id'] }).to eq(%w[b a])
        expect(result.to_h['sources']['sources']).to eq(
          [origin('/people/1'), origin('/people/0')]
        )
      end

      it 'is stable when sort keys are equal' do
        data = { 'people' => [
          { 'id' => 'a', 'age' => 1 },
          { 'id' => 'b', 'age' => 1 },
          { 'id' => 'c', 'age' => 0 }
        ] }
        result = locate('people | sort_by(@, &age)', data)
        expect(result.value.map { |p| p['id'] }).to eq(%w[c a b])
        expect(result.to_h['sources']['sources']).to eq(
          [origin('/people/2'), origin('/people/0'), origin('/people/1')]
        )
      end
    end

    context 'derived values' do
      let(:data) { { 'nums' => [1, 2, 3, 4], 'a' => 3, 'b' => 4 } }

      it 'marks computed scalars as derived with their direct inputs' do
        expect(locate('sum(nums)', data).to_h['sources']).to eq(derived(['/nums']))
        expect(locate('length(nums)', data).to_h['sources']).to eq(derived(['/nums']))
      end

      it 'lists every direct input for multi-argument computations' do
        result = locate('contains(nums, a)', data)
        expect(result.to_h['sources']).to eq(derived(['/nums', '/a']))
      end

      it 'marks literals as derived with no inputs' do
        expect(locate('`42`', data).to_h['sources']).to eq(derived([]))
      end

      it 'preserves origins when indexing into a derived array' do
        result = locate('nums[1:3] | [1]', data)
        expect(result.value).to eq(3)
        expect(result.to_h['sources']).to eq(origin('/nums/2'))
      end

      it 'does not find equal values elsewhere in the document' do
        tricky = { 'nums' => [7, 7], 'other' => { 'also_seven' => 7 } }
        result = locate('nums | sort(@) | [0]', tricky)
        expect(result.value).to eq(7)
        expect(result.to_h['sources']).to eq(origin('/nums/0'))
      end
    end

    context 'multi-select' do
      it 'builds a derived object whose fields keep their own sources' do
        data = { 'people' => [{ 'first' => 'James', 'last' => 'Bond' }] }
        result = locate("people[0].{given: first, family: last}", data)
        expect(result.value).to eq('given' => 'James', 'family' => 'Bond')
        expect(result.to_h['sources']).to eq(
          derived(['/people/0/first', '/people/0/last'],
                  'given' => origin('/people/0/first'),
                  'family' => origin('/people/0/last'))
        )
      end
    end

    it 'returns nil sources for missing values' do
      result = locate('foo.bar', { 'foo' => {} })
      expect(result.value).to be_nil
      expect(result.to_h['sources']).to be_nil
    end
  end
end
