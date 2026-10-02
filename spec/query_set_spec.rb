# frozen_string_literal: true
require 'spec_helper'

describe JMESPath::QuerySet do
  let(:data) do
    {
      'meta' => { 'total' => 4, 'page' => 1 },
      'people' => [
        { 'name' => 'alice', 'age' => 34, 'tags' => %w[admin dev] },
        { 'name' => 'bob',   'age' => 12, 'tags' => %w[user] },
        { 'name' => 'carol', 'age' => 27, 'tags' => %w[dev] },
        { 'name' => 'dave',  'age' => 51, 'tags' => %w[admin] }
      ]
    }
  end

  # A dozen queries over the same response, with heavy overlap:
  # shared field prefixes, several filters over the same big array,
  # projections, functions, expression references and a pipe.
  let(:queries) do
    {
      total:        'meta.total',
      page:         'meta.page',
      names:        'people[*].name',
      first_name:   'people[*].name | [0]',
      adults:       'people[?age >= `18`].name',
      seniors:      'people[?age >= `50`].name',
      admins:       'people[?contains(tags, `"admin"`)].name',
      oldest:       'max_by(people, &age).name',
      youngest:     'min_by(people, &age).name',
      sorted:       'sort_by(people, &age)[*].name',
      name_age:     'people[*].{n: name, a: age}',
      tag_lists:    'people[].tags'
    }
  end

  def build(hash = queries)
    set = JMESPath::QuerySet.new
    hash.each { |name, expr| set.add(name, expr) }
    set
  end

  describe 'correctness' do
    it 'returns the same results as searching each expression individually' do
      result = build.search(data)
      queries.each do |name, expression|
        expect(result[name]).to eq(JMESPath.search(expression, data)),
          "expected #{name} (#{expression}) to match JMESPath.search"
      end
      expect(result).to be_success
    end

    it 'matches individual searches when data is missing or nil' do
      sparse = { 'people' => [{ 'name' => 'x', 'age' => 5, 'tags' => [] }] }
      result = build.search(sparse)
      queries.each do |name, expression|
        expect(result.values[name]).to eq(JMESPath.search(expression, sparse))
      end
    end

    it 'supports method chaining and exposes registered names' do
      set = JMESPath::QuerySet.new
      expect(set.add(:a, 'meta.total')).to be(set)
      set.add(:b, 'meta.page')
      expect(set.names).to contain_exactly(:a, :b)
      expect(set[:a]).to eq('meta.total')
    end
  end

  describe 'semantic (not string) prefix handling' do
    it 'does not merge expressions that merely share a string prefix' do
      doc = { 'meta' => { 'total' => 1, 'total_pages' => 9 } }
      set = JMESPath::QuerySet.new
      set.add(:short, 'meta.total')
      set.add(:longer, 'meta.total_pages')
      result = set.search(doc)
      expect(result[:short]).to eq(1)
      expect(result[:longer]).to eq(9)
    end

    it 'distinguishes a pipe from a continued projection' do
      set = JMESPath::QuerySet.new
      set.add(:pipe, 'people[*].name | [0]')
      set.add(:projection, 'people[*].name[0]')
      result = set.search(data)
      expect(result[:pipe]).to eq('alice')
      expect(result[:projection]).to eq(JMESPath.search('people[*].name[0]', data))
      expect(result[:projection]).not_to eq(result[:pipe])
    end

    it 'merges a pipe with a dotted chain when they are semantically equal' do
      set = JMESPath::QuerySet.new
      set.add(:pipe, 'people[0] | name')
      set.add(:plain, 'people[0].name')
      terminals = []
      walk = lambda do |node|
        terminals << node[:names] unless node[:names].empty?
        node[:children].each { |c| walk.call(c) }
      end
      walk.call(set.plan)
      # both queries terminate on the very same plan node
      expect(terminals).to contain_exactly(%i[pipe plain])
      result = set.search(data)
      expect(result[:pipe]).to eq('alice')
      expect(result[:plain]).to eq('alice')
    end

    it 'keeps different filter conditions on the same array semantically distinct' do
      result = build.search(data)
      expect(result[:adults]).to eq(%w[alice carol dave])
      expect(result[:seniors]).to eq(%w[dave])
      expect(result[:admins]).to eq(%w[alice dave])
    end

    it 'distinguishes expression references inside functions' do
      result = build.search(data)
      expect(result[:oldest]).to eq('dave')
      expect(result[:youngest]).to eq('bob')
      expect(result[:sorted]).to eq(%w[bob carol alice dave])
    end
  end

  describe 'error handling' do
    let(:failing_queries) do
      {
        ok:      'meta.total',
        bad:     'abs(meta)', # InvalidTypeError at visit time
        also_ok: 'people[0].name'
      }
    end

    it 'fails the whole group by default' do
      set = build(failing_queries)
      expect { set.search(data) }.to raise_error(JMESPath::Errors::InvalidTypeError)
    end

    it 'keeps other results with on_error: :partial' do
      result = build(failing_queries).search(data, on_error: :partial)
      expect(result[:ok]).to eq(4)
      expect(result[:also_ok]).to eq('alice')
      expect(result.values).not_to have_key(:bad)
      expect(result.errors[:bad]).to be_a(JMESPath::Errors::InvalidTypeError)
      expect(result).not_to be_success
    end

    it 'fails every query under a broken shared prefix, and only those' do
      set = JMESPath::QuerySet.new
      set.add(:x, 'abs(meta).a')
      set.add(:y, 'abs(meta).b')
      set.add(:z, 'meta.total')
      result = set.search(data, on_error: :partial)
      expect(result.errors.keys).to contain_exactly(:x, :y)
      expect(result[:z]).to eq(4)
    end

    it 'raises parse errors by default and reports them with :partial' do
      set = JMESPath::QuerySet.new
      set.add(:bad, 'people[?')
      set.add(:good, 'meta.total')
      expect { set.search(data) }.to raise_error(JMESPath::Errors::SyntaxError)
      result = set.search(data, on_error: :partial)
      expect(result[:good]).to eq(4)
      expect(result.errors[:bad]).to be_a(JMESPath::Errors::SyntaxError)
    end

    it 'recovers when a query is fixed after a failed compile' do
      set = JMESPath::QuerySet.new
      set.add(:bad, 'people[?')
      expect { set.search(data) }.to raise_error(JMESPath::Errors::SyntaxError)
      set.add(:bad, 'people[0].name')
      expect(set.search(data)[:bad]).to eq('alice')
    end
  end

  describe 'access counting' do
    let(:big_data) do
      { 'items' => (1..100).map { |i| { 'price' => i, 'sku' => "s#{i}" } },
        'meta' => { 'total' => 100 } }
    end

    let(:big_queries) do
      {
        over10: 'items[?price > `10`].sku',
        over20: 'items[?price > `20`].sku',
        over30: 'items[?price > `30`].sku',
        total:  'meta.total'
      }
    end

    it 'evaluates the shared filter target with a single pass over the array' do
      merged = build(big_queries).search(big_data, count: true)

      baseline_steps = 0
      baseline_elements = 0
      big_queries.each do |name, expression|
        single = JMESPath::QuerySet.new
        single.add(name, expression)
        result = single.search(big_data, count: true)
        baseline_steps += result.stats.step_visits
        baseline_elements += result.stats.element_visits
        expect(merged[name]).to eq(result[name])
      end

      # each of the three filtered queries scans the 100 element array on
      # its own in the baseline; the merged plan scans it exactly once
      expect(baseline_elements).to eq(300)
      expect(merged.stats.element_visits).to eq(100)
      # the shared `items` target is resolved once, not three times
      expect(merged.stats.step_visits).to be < baseline_steps
    end

    it 'evaluates a shared field prefix once' do
      set = JMESPath::QuerySet.new
      set.add(:a, 'meta.total')
      set.add(:b, 'meta.page')
      result = set.search(data, count: true)
      # meta + total + page = 3 steps; individually it would be 4
      expect(result.stats.step_visits).to eq(3)
    end

    it 'does not count visits for queries hidden behind a failed prefix' do
      set = JMESPath::QuerySet.new
      set.add(:x, 'abs(meta).deep.deeper')
      set.add(:y, 'meta.total')
      result = set.search(data, on_error: :partial, count: true)
      expect(result.errors.keys).to eq([:x])
      expect(result[:y]).to eq(4)
    end
  end
end
