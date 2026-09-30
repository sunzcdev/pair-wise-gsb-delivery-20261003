# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  describe '.search_with_sources' do
    let(:data) do
      {
        'people' => [
          { 'name' => 'Arthur', 'age' => 21, 'city' => 'Berlin' },
          { 'name' => 'Trillian', 'age' => 31, 'city' => 'London' },
          { 'name' => 'Ford', 'age' => nil, 'city' => 'Betelgeuse' },
          { 'name' => 'Zaphod', 'age' => 42, 'city' => 'Betelgeuse' }
        ],
        'groups' => [[1, 2], [3], 9, []],
        'a/b' => { 'c~d' => 7 },
        'duplicates' => [
          { 'tag' => 'same' },
          { 'tag' => 'same' }
        ],
        'nested' => { 'x' => { 'y' => 'deep' } }
      }
    end

    it 'returns the same value as .search' do
      expect(JMESPath.search_with_sources('people[*].name', data).value)
        .to eq(JMESPath.search('people[*].name', data))
    end

    it 'does not change the return type of .search' do
      expect(JMESPath.search('people[*].name', data)).to be_an(Array)
      expect(JMESPath.search('nested.x.y', data)).to eq('deep')
    end

    it 'points a verbatim leaf at its JSON Pointer' do
      result = JMESPath.search_with_sources('nested.x.y', data)

      expect(result.value).to eq('deep')
      expect(result.location_at('')).to be_sourced
      expect(result.location_at('').pointer).to eq('/nested/x/y')
      expect(result[''].pointer).to eq('/nested/x/y')
    end

    it 'accepts pointer components for lookup' do
      result = JMESPath.search_with_sources('nested', data)
      expect(result.location_at('x', 'y').pointer).to eq('/nested/x/y')
    end

    it 'escapes ~ and / in JSON Pointers' do
      result = JMESPath.search_with_sources('"a/b"."c~d"', data)

      expect(result.value).to eq(7)
      expect(result[''].pointer).to eq('/a~1b/c~0d')
    end

    context 'projections' do
      it 'keeps result indices distinct from source indices' do
        result = JMESPath.search_with_sources('people[*].name', data)

        expect(result.value).to eq(%w[Arthur Trillian Ford Zaphod])
        expect(result.location_at(0).pointer).to eq('/people/0/name')
        expect(result.location_at(3).pointer).to eq('/people/3/name')
      end

      it 'skips elements projected to null and points survivors at their real positions' do
        result = JMESPath.search_with_sources('people[*].age', data)

        expect(result.value).to eq([21, 31, 42])
        expect(result.locations.keys).to eq(['', '/0', '/1', '/2'])
        expect(result.location_at(0).pointer).to eq('/people/0/age')
        expect(result.location_at(1).pointer).to eq('/people/1/age')
        # result index 2 must not masquerade as source index 2
        expect(result.location_at(2).pointer).to eq('/people/3/age')
      end

      it 'keeps explicit nulls inside multi-select lists' do
        result = JMESPath.search_with_sources('people[*][name, age]', data)

        expect(result.value).to eq(
          [['Arthur', 21], ['Trillian', 31], ['Ford', nil], ['Zaphod', 42]]
        )
        null_age = result.location_at(2, 1)
        expect(null_age).to be_sourced
        expect(null_age.pointer).to eq('/people/2/age')
      end

      it 'traverses filters without renumbering the source array' do
        result = JMESPath.search_with_sources('people[?age > `30`].name', data)

        expect(result.value).to eq(['Trillian', 'Zaphod'])
        expect(result.location_at(0).pointer).to eq('/people/1/name')
        expect(result.location_at(1).pointer).to eq('/people/3/name')
        expect(result.locations[''].derived?).to be(true)
        expect(result.locations[''].inputs).to eq(['/people/1/name', '/people/3/name'])
      end

      it 'marks a projection container as derived' do
        result = JMESPath.search_with_sources('people[*].name', data)

        expect(result.locations['']).to be_derived
        expect(result.locations[''].inputs)
          .to eq(%w[/people/0/name /people/1/name /people/2/name /people/3/name])
      end
    end

    context 'slices' do
      it 'keeps source positions through positive slices' do
        result = JMESPath.search_with_sources('people[1:3].name', data)

        expect(result.value).to eq(%w[Trillian Ford])
        expect(result.location_at(0).pointer).to eq('/people/1/name')
        expect(result.location_at(1).pointer).to eq('/people/2/name')
      end

      it 'keeps source positions through stepped and negative slices' do
        result = JMESPath.search_with_sources('people[::-1].name', data)

        expect(result.value).to eq(%w[Zaphod Ford Trillian Arthur])
        expect(result.location_at(0).pointer).to eq('/people/3/name')
        expect(result.location_at(3).pointer).to eq('/people/0/name')
      end

      it 'marks sliced strings as derived with the string as direct input' do
        expect(JMESPath.search('nested.x.y[0:2]', data)).to be_nil
        result = JMESPath.search_with_sources('nested.x.y[0:2]', data)

        expect(result.value).to be_nil
        expect(result.locations['']).to be_missing
      end
    end

    context 'flatten' do
      it 'preserves the original nested positions' do
        result = JMESPath.search_with_sources('groups[]', data)

        expect(result.value).to eq([1, 2, 3, 9])
        expect(result.location_at(0).pointer).to eq('/groups/0/0')
        expect(result.location_at(1).pointer).to eq('/groups/0/1')
        expect(result.location_at(2).pointer).to eq('/groups/1/0')
        expect(result.location_at(3).pointer).to eq('/groups/2')
      end
    end

    context 'sort_by' do
      it 'reorders provenance by the computed order' do
        result = JMESPath.search_with_sources('sort_by(people[?age != null], &age)[*].name', data)

        expect(result.value).to eq(%w[Arthur Trillian Zaphod])
        expect(result.location_at(0).pointer).to eq('/people/0/name')
        expect(result.location_at(1).pointer).to eq('/people/1/name')
        expect(result.location_at(2).pointer).to eq('/people/3/name')
      end

      it 'does not find provenance by value equality' do
        result = JMESPath.search_with_sources('sort_by(duplicates, &tag)[0].tag', data)

        expect(result.value).to eq('same')
        expect(result.locations[''].pointer).to eq('/duplicates/0/tag')

        result = JMESPath.search_with_sources('sort_by(duplicates, &tag)[1].tag', data)

        expect(result.value).to eq('same')
        expect(result.locations[''].pointer).to eq('/duplicates/1/tag')
      end

      it 'projects reordered provenance without searching by equality' do
        result = JMESPath.search_with_sources('sort_by(duplicates, &tag)[*].tag', data)

        expect(result.value).to eq(['same', 'same'])
        expect(result.location_at(0).pointer).to eq('/duplicates/0/tag')
        expect(result.location_at(1).pointer).to eq('/duplicates/1/tag')
      end

      it 'resolves equal keys stably' do
        result = JMESPath.search_with_sources('sort_by(people[?age != null], &city)[*].name', data)

        expect(result.value).to eq(['Arthur', 'Zaphod', 'Trillian'])
        expect(result.location_at(0).pointer).to eq('/people/0/name')
        expect(result.location_at(1).pointer).to eq('/people/3/name')
        expect(result.location_at(2).pointer).to eq('/people/1/name')
      end
    end

    context 'duplicate values' do
      it 'attributes identical values to distinct sources' do
        result = JMESPath.search_with_sources('[duplicates[0].tag, duplicates[1].tag]', data)

        expect(result.value).to eq(%w[same same])
        expect(result.location_at(0).pointer).to eq('/duplicates/0/tag')
        expect(result.location_at(1).pointer).to eq('/duplicates/1/tag')
      end

      it 'keeps provenance when equal objects survive a filter' do
        result = JMESPath.search_with_sources('duplicates[?tag == `"same"`].tag', data)

        expect(result.value).to eq(%w[same same])
        expect(result.location_at(0).pointer).to eq('/duplicates/0/tag')
        expect(result.location_at(1).pointer).to eq('/duplicates/1/tag')
      end
    end

    context 'derived values' do
      it 'lists direct inputs of function results' do
        result = JMESPath.search_with_sources('sum(people[?age != null].age)', data)

        expect(result.value).to eq(94)
        expect(result.locations['']).to be_derived
        expect(result.locations[''].inputs).to eq(['/people/0/age', '/people/1/age', '/people/3/age'])
      end

      it 'passes the chosen element through for max()' do
        result = JMESPath.search_with_sources('max(people[?age != null].age)', data)

        expect(result.value).to eq(42)
        expect(result.locations[''].pointer).to eq('/people/3/age')
      end

      it 'lists element inputs for sum() rather than matching by value' do
        result = JMESPath.search_with_sources('sum(groups[0])', data)

        expect(result.value).to eq(3)
        expect(result.locations[''].inputs).to eq(['/groups/0/0', '/groups/0/1'])
      end

      it 'marks literals and comparisons as derived without inputs' do
        result = JMESPath.search_with_sources('`"constant"`', data)
        expect(result.value).to eq('constant')
        expect(result.locations['']).to be_derived
        expect(result.locations[''].inputs).to eq([])
      end

      it 'marks multi-select hashes as derived with direct child inputs' do
        result = JMESPath.search_with_sources('{oldest: people[3].name, total: sum(groups[0])}', data)

        expect(result.value).to eq('oldest' => 'Zaphod', 'total' => 3)
        expect(result.location_at(:oldest).pointer).to eq('/people/3/name')
        expect(result.location_at(:total).inputs).to eq(['/groups/0/0', '/groups/0/1'])
        expect(result.locations[''].inputs).to eq(['/people/3/name', '/groups/0/0', '/groups/0/1'])
      end

      it 'attributes join() to the joined values' do
        result = JMESPath.search_with_sources("join('-', people[0:2].name)", data)

        expect(result.value).to eq('Arthur-Trillian')
        expect(result.locations[''].inputs).to eq(['/people/0/name', '/people/1/name'])
      end
    end

    context 'missing values' do
      it 'marks unresolved positions as missing' do
        result = JMESPath.search_with_sources('people[9].name', data)

        expect(result.value).to be_nil
        expect(result.locations['']).to be_missing
      end

      it 'keeps document null distinct from a missing path' do
        sourced_null = JMESPath.search_with_sources('people[2].age', data)
        missing = JMESPath.search_with_sources('people[2].age.missing', data)

        expect(sourced_null.value).to be_nil
        expect(sourced_null.locations[''].pointer).to eq('/people/2/age')
        expect(missing.locations['']).to be_missing
      end
    end

    context 'object projections' do
      it 'retains member key provenance' do
        result = JMESPath.search_with_sources('nested.*.y', data)

        expect(result.value).to eq(['deep'])
        expect(result.location_at(0).pointer).to eq('/nested/x/y')
      end
    end

    it 'supports a Runtime directly' do
      runtime = JMESPath::Runtime.new(cache_expressions: false)
      result = runtime.search_with_sources('people[0].name', data)

      expect(result).to be_a(SourcedResult)
      expect(result.value).to eq('Arthur')
      expect(result.location_at(0)).to be_nil
      expect(result[''].pointer).to eq('/people/0/name')
    end

    it 'honours disable_visit_errors without raising' do
      runtime = JMESPath::Runtime.new(disable_visit_errors: true)
      mixed = { 'rows' => [{ 'n' => 1 }, { 'n' => 'x' }] }

      expect(runtime.search('max_by(rows, &n)', mixed)).to be_nil
      result = runtime.search_with_sources('max_by(rows, &n)', mixed)
      expect(result.value).to be_nil
      expect(result.locations['']).to be_derived
    end

    it 'caches and reuses parsed expressions across calls' do
      runtime = JMESPath::Runtime.new
      first = runtime.search_with_sources('people[0].name', data)
      second = runtime.search_with_sources('people[1].name', data)

      expect(first[''].pointer).to eq('/people/0/name')
      expect(second[''].pointer).to eq('/people/1/name')
    end
  end
end
