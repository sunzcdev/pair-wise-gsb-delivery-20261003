# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  describe QuerySet, 'fusion & sharing' do
    let(:people) do
      Array.new(10) { |i| { 'name' => "n#{i}", 'age' => 10 * i, 'email' => "e#{i}@x", 'city' => (i.even? ? 'NY' : 'LA') } }
    end
    let(:data) { { 'people' => people } }

    def plan_groups(queries)
      planner = QuerySet::Planner.new
      parser = CachingParser.new
      entries = queries.map do |name, expression|
        ast = parser.parse(expression)
        QuerySet::Planner::Entry.new(name, [name], QuerySet::Normalizer.to_steps(ast))
      end
      planner.compile(entries).groups
    end

    def projection_groups(groups)
      groups.select { |g| [:array_projection, :object_projection].include?(g.type) }
    end

    describe 'one traversal for many filters over one array' do
      let(:queries) do
        {
          adult_names:   'people[?age > `18`].name',
          adult_emails:  'people[?age > `18`].email',
          adult_cities:  'people[?age > `18`].city',
          senior_names:  'people[?age > `65`].name',
          ny_residents:  'people[?city == `NY`].name'
        }
      end

      it 'compiles the shared prefix into a single projection group' do
        groups = plan_groups(queries)
        expect(projection_groups(groups).size).to eq(1)
        proj = projection_groups(groups).first
        # all five queries flow through the one projection
        expect(proj.entry_ids.sort).to eq(queries.keys.sort)
      end

      it 'iterates the source array once instead of once per query' do
        result = QuerySet.new(queries).search(data)
        # 5 independent searches each walk 10 elements = 50 iterations;
        # the fused plan walks the array once = 10 iterations.
        expect(result.stats.separate.array_iterations).to eq(50)
        expect(result.stats.fused.array_iterations).to eq(10)
        expect(result.stats.iterations_saved).to eq(40)
      end

      it 'still visits fewer AST nodes' do
        result = QuerySet.new(queries).search(data)
        expect(result.stats.fused.visits).to be < result.stats.separate.visits
      end

      it 'returns the same values as separate searches' do
        result = QuerySet.new(queries).search(data)
        queries.each do |name, expression|
          expect(result[name]).to eq(JMESPath.search(expression, data))
        end
      end
    end

    describe 'different filter predicates stay semantically distinct' do
      it 'uses one projection but separate condition branches' do
        groups = plan_groups(
          a: 'people[?age > `18`].name',
          b: 'people[?age > `65`].name'
        )
        proj = projection_groups(groups).first
        conditions = proj.branch_level.groups.select { |g| g.type == :condition }
        expect(conditions.map(&:entry_ids)).to contain_exactly([:a], [:b])
      end

      it 'does not merge a `==` filter with a `!=` filter' do
        groups = plan_groups(
          a: 'people[?city == `NY`].name',
          b: 'people[?city != `NY`].name'
        )
        proj = projection_groups(groups).first
        conditions = proj.branch_level.groups.select { |g| g.type == :condition }
        expect(conditions.size).to eq(2)
      end

      it 'merges the identical predicate and shares the test' do
        groups = plan_groups(
          a: 'people[?age > `18`].name',
          b: 'people[?age > `18`].email'
        )
        proj = projection_groups(groups).first
        conditions = proj.branch_level.groups.select { |g| g.type == :condition }
        expect(conditions.size).to eq(1)
        expect(conditions.first.entry_ids.sort).to eq(%i[a b])
      end
    end

    describe 'pipes vs dots are distinguished semantically, not textually' do
      it 'does not fuse a dotted chain across a pipe boundary' do
        groups = plan_groups(x: 'a.b.c', y: 'a|b.c')
        # the root `a` is shared, but the `b` step has different provenance
        shared_root = groups.first
        expect(shared_root.entry_ids.sort).to eq(%i[x y])
        next_heads = shared_root.next_level.groups.map { |g| g.node.class }
        expect(next_heads.size).to eq(2)
      end

      it 'fuses two dot chains that share a real prefix' do
        groups = plan_groups(x: 'a.b.c', y: 'a.b.d')
        expect(groups.first.entry_ids.sort).to eq(%i[x y])
        expect(groups.first.next_level.groups.first.entry_ids.sort).to eq(%i[x y])
      end

      it 'fuses two pipe chains that share a real prefix' do
        groups = plan_groups(x: 'a|b|c', y: 'a|b|d')
        expect(groups.first.entry_ids.sort).to eq(%i[x y])
        expect(groups.first.next_level.groups.first.entry_ids.sort).to eq(%i[x y])
      end

      it 'produces correct values for dot/pipe lookalikes' do
        doc = { 'foo' => { 'bar' => 1 }, 'bar' => 'via_root_bar' }
        result = QuerySet.new(d: 'foo.bar', p: 'foo|bar').search(doc)
        expect(result[:d]).to eq(1)
        expect(result[:p]).to eq(1)
      end
    end

    describe 'expression references are opaque boundaries' do
      it 'keeps map(&age) and map(&name) in separate groups' do
        groups = plan_groups(a: 'map(&age, people)', b: 'map(&name, people)')
        expect(groups.size).to eq(2)
      end

      it 'fuses two identical map(&age) expressions' do
        groups = plan_groups(a: 'map(&age, people)', b: 'map(&age, people)')
        expect(groups.size).to eq(1)
        expect(groups.first.entry_ids.sort).to eq(%i[a b])
      end

      it 'never merges an expression-reference body into a plain prefix' do
        groups = plan_groups(a: 'map(&age, people)', b: 'people')
        expect(groups.size).to eq(2)
      end

      it 'computes both references correctly in one pass' do
        doc = { 'people' => [{ 'age' => 1, 'name' => 'a' }, { 'age' => 2, 'name' => 'b' }] }
        result = QuerySet.new(
          ages: 'map(&age, people)', names: 'map(&name, people)'
        ).search(doc)
        expect(result[:ages]).to eq([1, 2])
        expect(result[:names]).to eq(%w[a b])
      end
    end

    describe 'visit accounting matches the baseline ruler' do
      it 'counts identical totals when nothing is shareable' do
        # totally independent roots -> no sharing possible
        queries = { a: 'alpha', b: 'beta', c: 'gamma' }
        doc = { 'alpha' => 1, 'beta' => 2, 'gamma' => 3 }
        result = QuerySet.new(queries).search(doc)
        expect(result.stats.fused.visits).to eq(result.stats.separate.visits)
        expect(result.stats.iterations_saved).to eq(0)
      end

      it 'reduces both visits and iterations for a shared prefix' do
        queries = {
          a: 'people[?age > `18`].name',
          b: 'people[?age > `18`].email',
          c: 'people[?age > `18`].city'
        }
        result = QuerySet.new(queries).search(data)
        expect(result.stats.fused.visits).to be < result.stats.separate.visits
        expect(result.stats.fused.array_iterations).to eq(10)
        expect(result.stats.separate.array_iterations).to eq(30)
      end

      it 'exposes a stable stats hash' do
        result = QuerySet.new(a: 'people[*].name').search(data)
        hash = result.stats.to_h
        expect(hash[:fused].keys).to contain_exactly(:visits, :array_iterations, :object_iterations)
        expect(hash[:separate].keys).to contain_exactly(:visits, :array_iterations, :object_iterations)
        expect(result.stats.visits_saved).to eq(hash[:separate][:visits] - hash[:fused][:visits])
      end

      it 'counts object projection iterations with the same ruler' do
        doc = { 'obj' => { 'a' => { 'v' => 1 }, 'b' => { 'v' => 2 }, 'c' => { 'v' => 3 } } }
        result = QuerySet.new(x: 'obj.*.v', y: 'obj.*.v').search(doc)
        expect(result[:x]).to eq([1, 2, 3])
        expect(result.stats.fused.object_iterations).to eq(3)
        expect(result.stats.separate.object_iterations).to eq(6)
        expect(result.stats.iterations_saved).to eq(3)
      end

      it 'counts flatten iterations with the same ruler' do
        doc = { 'matrix' => [[1, 2], [3], [4, 5, 6]] }
        result = QuerySet.new(x: 'matrix[]').search(doc)
        expect(result[:x]).to eq([1, 2, 3, 4, 5, 6])
        # flatten walks the 3 sub-arrays (6 elements), then the resulting
        # projection walks 3 targets; fused and separate must agree exactly.
        expect(result.stats.fused.array_iterations).to eq(result.stats.separate.array_iterations)
        expect(result.stats.fused.array_iterations).to eq(9)
      end
    end
  end
end