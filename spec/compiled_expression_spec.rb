require 'spec_helper'
require 'dentaku'
require 'dentaku/calculator'

describe Dentaku::CompiledExpression do
  let(:calculator) { Dentaku::Calculator.new }

  describe 'compilation' do
    it 'rejects unknown function modes' do
      expect { calculator.compile('1+1', functions: :bogus) }
        .to raise_error(ArgumentError, /pinned|latest/)
    end

    it 'rejects non-string expressions' do
      expect { calculator.compile(calculator.ast('1+1')) }
        .to raise_error(ArgumentError, /String/)
    end

    it 'carries the function registry generation it was compiled against' do
      compiled = calculator.compile('1+1')
      expect(compiled.registry_generation).to eq(calculator.function_registry_generation)

      calculator.add_function(:gen_f, :numeric, -> { 1 })
      expect(compiled.registry_generation).to be < calculator.function_registry_generation
    end

    it 'defaults to pinned mode' do
      expect(calculator.compile('1+1')).to be_pinned
    end
  end

  describe 'pinned mode' do
    it 'keeps calling the definition captured at compile time' do
      calculator.add_function(:pinned_f, :numeric, -> { 1 })
      compiled = calculator.compile('pinned_f()', functions: :pinned)

      calculator.add_function(:pinned_f, :numeric, -> { 2 })

      expect(compiled.value).to eq(1)
      expect(calculator.evaluate!('pinned_f()')).to eq(2)
    end

    it 'does not go stale when the registry changes' do
      calculator.add_function(:pinned_s, :numeric, -> { 1 })
      compiled = calculator.compile('pinned_s()', functions: :pinned)
      calculator.add_function(:pinned_s, :numeric, -> { 2 })
      expect(compiled).to be_stale
      expect(compiled.value).to eq(1)
    end
  end

  describe 'latest mode' do
    it 'picks up re-registered definitions' do
      calculator.add_function(:latest_f, :numeric, -> { 1 })
      compiled = calculator.compile('latest_f()', functions: :latest)
      expect(compiled.value).to eq(1)

      calculator.add_function(:latest_f, :numeric, -> { 2 })
      expect(compiled).to be_stale
      expect(compiled.value).to eq(2)
      expect(compiled).not_to be_stale
    end

    it 'updates the registry generation after refreshing' do
      calculator.add_function(:gen_l, :numeric, -> { 1 })
      compiled = calculator.compile('gen_l()', functions: :latest)
      before = compiled.registry_generation

      calculator.add_function(:gen_l, :numeric, -> { 2 })
      compiled.value

      expect(compiled.registry_generation).to be > before
    end

    it 'revalidates argument counts against the new definition' do
      calculator.add_function(:arity_f, :numeric, ->(x) { x })
      compiled = calculator.compile('arity_f(1)', functions: :latest)
      expect(compiled.value).to eq(1)

      calculator.add_function(:arity_f, :numeric, ->(x, y) { x + y })
      expect { compiled.value }.to raise_error(Dentaku::ParseError, /too few/)
    end

    it 'revalidates declared return types against the new definition' do
      calculator.add_function(:typed_f, :numeric, -> { 1 })
      compiled = calculator.compile('typed_f() + 1', functions: :latest)
      expect(compiled.value).to eq(2)

      calculator.add_function(:typed_f, :string, -> { 'nope' })
      expect { compiled.value }.to raise_error(Dentaku::ParseError, /not string/)
    end

    it 'treats alias changes as an identity change' do
      Dentaku.aliases = { round: ['rrround'] }
      compiled = calculator.compile('rrround(5.1)', functions: :latest)
      expect(compiled.value).to eq(5)

      Dentaku.aliases = {}
      expect(compiled).to be_stale
      expect { compiled.value }.to raise_error(Dentaku::ParseError, /Undefined function/)
    ensure
      Dentaku.aliases = {}
    end

    it 'treats an instance function overriding a global one as an identity change' do
      Dentaku::Calculator.add_function(:override_f, :numeric, -> { 1 })
      compiled = calculator.compile('override_f()', functions: :latest)
      expect(compiled.value).to eq(1)

      calculator.add_function(:override_f, :numeric, -> { 2 })
      expect(compiled).to be_stale
      expect(compiled.value).to eq(2)
    ensure
      Dentaku::AST::Function.registry.delete('override_f')
    end

    it 'treats global registrations as an identity change for instance calculators' do
      compiled = calculator.compile('1 + 1', functions: :latest)
      expect(compiled).not_to be_stale

      Dentaku::Calculator.add_function(:global_only_f, :numeric, -> { 1 })
      expect(compiled).to be_stale
    ensure
      Dentaku::AST::Function.registry.delete('global_only_f')
    end
  end

  describe 'evaluation through the calculator' do
    it 'evaluates a compiled expression with evaluate!' do
      calculator.add_function(:double_f, :numeric, ->(x) { x * 2 })
      compiled = calculator.compile('double_f(n) + 1')
      expect(calculator.evaluate!(compiled, n: 3)).to eq(7)
    end

    it 'refreshes a stale latest expression during evaluate!' do
      calculator.add_function(:eval_f, :numeric, -> { 1 })
      compiled = calculator.compile('eval_f()', functions: :latest)
      calculator.add_function(:eval_f, :numeric, -> { 2 })
      expect(calculator.evaluate!(compiled)).to eq(2)
    end

    it 'reports dependencies of the current definitions' do
      calculator.add_function(:dep_f, :numeric, ->(x) { x })
      compiled = calculator.compile('dep_f(a)', functions: :latest)
      expect(calculator.dependencies(compiled)).to eq(%w(a))
    end
  end

  describe 'AST cache contract' do
    let(:cache) { {} }

    def caching_calculator
      Dentaku::Calculator.new(ast_cache: cache, cache_ast: true)
    end

    it 'does not let results depend on which calculator computed first' do
      first = caching_calculator
      second = caching_calculator
      first.add_function(:shared_f, :numeric, -> { 1 })
      second.add_function(:shared_f, :numeric, -> { 2 })

      expect(first.evaluate!('shared_f()')).to eq(1)
      expect(second.evaluate!('shared_f()')).to eq(2)
      expect(first.evaluate!('shared_f()')).to eq(1)
    end

    it 'gives the same results when the other calculator warms the cache' do
      first = caching_calculator
      second = caching_calculator
      first.add_function(:shared_g, :numeric, -> { 1 })
      second.add_function(:shared_g, :numeric, -> { 2 })

      expect(second.evaluate!('shared_g()')).to eq(2)
      expect(first.evaluate!('shared_g()')).to eq(1)
      expect(second.evaluate!('shared_g()')).to eq(2)
    end

    it 'shares cache entries between calculators with identical registries' do
      first = caching_calculator
      second = caching_calculator

      first.evaluate!('1 + 1')
      expect(cache['1 + 1'].length).to eq(1)

      second.evaluate!('1 + 1')
      expect(cache['1 + 1'].length).to eq(1)
    end

    it 'revalidates a cached expression when the registry changes' do
      calculator = caching_calculator
      calculator.add_function(:cached_f, :numeric, ->(x) { x })
      expect(calculator.evaluate!('cached_f(1)')).to eq(1)

      calculator.add_function(:cached_f, :numeric, ->(x, y) { x + y })
      expect { calculator.evaluate!('cached_f(1)') }.to raise_error(Dentaku::ParseError, /too few/)
      expect(calculator.evaluate!('cached_f(1, 2)')).to eq(3)
    end

    it 'never keys the cache on the data being evaluated' do
      calculator = caching_calculator
      expect(calculator.evaluate!('secret + 1', secret: 41)).to eq(42)
      expect(calculator.evaluate!('secret + 1', secret: 100)).to eq(101)

      expect(cache.keys).to eq(['secret + 1'])
    end

    it 'still honors pre-warmed caches that hold raw nodes' do
      node = Dentaku::Calculator.new.ast('1+1')
      warmed = Dentaku::Calculator.new(ast_cache: { '1+1' => node }, cache_ast: true)
      expect(warmed.evaluate!('1+1')).to eq(2)
    end
  end
end
