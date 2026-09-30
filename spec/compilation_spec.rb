require 'spec_helper'
require 'dentaku'
require 'dentaku/calculator'

describe Dentaku::Compilation do
  let(:calculator) { Dentaku::Calculator.new(cache_ast: true) }

  describe 'registry generations' do
    it 'bumps the instance generation when an instance function is registered' do
      expect { calculator.add_function(:gen_fn, :numeric, -> { 1 }) }
        .to change { calculator.registry_generation }
    end

    it 'bumps the global generation when a global function is registered' do
      expect { Dentaku::Calculator.add_function(:gen_global_fn, :numeric, -> { 1 }) }
        .to change { calculator.registry_generation }
    end

    it 'exposes the generation captured at compile time' do
      compiled = calculator.compile('1 + 1')
      expect(compiled.registry_generation).to eq(calculator.registry_generation)
      expect(compiled).not_to be_stale

      calculator.add_function(:gen_other_fn, :numeric, -> { 1 })
      expect(compiled).to be_stale
      expect(compiled.registry_generation).not_to eq(calculator.registry_generation)
    end
  end

  describe 'resolution modes' do
    it 'defaults to pinned and rejects unknown modes' do
      expect(calculator.function_resolution).to eq(:pinned)
      expect(calculator.compile('1 + 1')).to be_pinned

      expect { Dentaku::Calculator.new(function_resolution: :bogus) }
        .to raise_error(::ArgumentError, /function resolution/)
      expect { calculator.compile('1 + 1', functions: :bogus) }
        .to raise_error(::ArgumentError, /function resolution/)
    end

    it 'pinned mode keeps calling the original definition after re-registration' do
      calculator.add_function(:pinned_fn, :numeric, ->(x) { x * 2 })
      expect(calculator.evaluate!('pinned_fn(1)')).to eq(2)

      calculator.add_function(:pinned_fn, :numeric, ->(x) { x * 10 })
      expect(calculator.evaluate!('pinned_fn(1)')).to eq(2)
    end

    it 'latest mode calls the newest definition after re-registration' do
      latest = Dentaku::Calculator.new(cache_ast: true, function_resolution: :latest)
      latest.add_function(:latest_fn, :numeric, ->(x) { x * 2 })
      expect(latest.evaluate!('latest_fn(1)')).to eq(2)

      latest.add_function(:latest_fn, :numeric, ->(x) { x * 10 })
      expect(latest.evaluate!('latest_fn(1)')).to eq(10)
    end

    it 'lets the caller choose the mode per compilation' do
      calculator.add_function(:chosen_fn, :numeric, ->(x) { x * 2 })
      pinned = calculator.compile('chosen_fn(1)')
      latest = calculator.compile('chosen_fn(1)', functions: :latest)

      calculator.add_function(:chosen_fn, :numeric, ->(x) { x * 10 })

      expect(calculator.evaluate!(pinned)).to eq(2)
      expect(calculator.evaluate!(latest)).to eq(10)
    end
  end

  describe 're-validation in latest mode' do
    let(:latest) { Dentaku::Calculator.new(cache_ast: true, function_resolution: :latest) }

    it 're-checks arity when the registry changes' do
      latest.add_function(:arity_fn, :numeric, ->(x) { x })
      expect(latest.evaluate!('arity_fn(1)')).to eq(1)

      latest.add_function(:arity_fn, :numeric, ->(x, y) { x + y })
      expect { latest.evaluate!('arity_fn(1)') }.to raise_error(Dentaku::ParseError)
      expect(latest.evaluate!('arity_fn(1, 2)')).to eq(3)
    end

    it 're-checks return types when the registry changes' do
      latest.add_function(:typed_fn, :numeric, ->(x) { x })
      expect(latest.evaluate!('typed_fn(1) + 1')).to eq(2)

      latest.add_function(:typed_fn, :string, ->(x) { x.to_s })
      expect { latest.evaluate!('typed_fn(1) + 1') }.to raise_error(Dentaku::ParseError)
    end

    it 're-validates when an instance function shadows a global one' do
      Dentaku::Calculator.add_function(:shadowed_fn, :numeric, ->(x) { x + 1 })
      expect(latest.evaluate!('shadowed_fn(1)')).to eq(2)

      latest.add_function(:shadowed_fn, :numeric, ->(x) { x + 100 })
      expect(latest.evaluate!('shadowed_fn(1)')).to eq(101)
    end

    it 're-validates when a global function is re-registered' do
      Dentaku::Calculator.add_function(:global_ver_fn, :numeric, -> { 1 })
      expect(latest.evaluate!('global_ver_fn()')).to eq(1)

      Dentaku::Calculator.add_function(:global_ver_fn, :numeric, -> { 2 })
      expect(latest.evaluate!('global_ver_fn()')).to eq(2)
    end
  end

  describe 'cache identity' do
    it 'treats alias changes as a different compilation environment' do
      Dentaku.aliases = { round: ['redondear'] }
      aliased = Dentaku::Calculator.new(cache_ast: true)
      expect(aliased.evaluate!('redondear(5.1)')).to eq(5)

      Dentaku.aliases = {}
      expect { aliased.evaluate!('redondear(5.1)') }.to raise_error(Dentaku::ParseError)
    ensure
      Dentaku.aliases = { roundup: ['roundupup'] }
    end

    it 'keeps same-named functions of calculators sharing a cache isolated' do
      shared = {}
      first = Dentaku::Calculator.new(ast_cache: shared, cache_ast: true)
      first.add_function(:shared_fn, :numeric, ->(x) { x * 2 })
      second = Dentaku::Calculator.new(ast_cache: shared, cache_ast: true)
      second.add_function(:shared_fn, :numeric, ->(x) { x * 200 })

      # interleave evaluations: results must not depend on who computed first
      expect(first.evaluate!('SHARED_FN(1)')).to eq(2)
      expect(second.evaluate!('SHARED_FN(1)')).to eq(200)
      expect(first.evaluate!('SHARED_FN(1)')).to eq(2)
      expect(second.evaluate!('SHARED_FN(1)')).to eq(200)
      expect(shared.keys).to eq(['SHARED_FN(1)'])
    end

    it 'does not store evaluation data in the cache' do
      calculator.evaluate!('business_input + 1', business_input: 424_242)

      expect(calculator.ast_cache.keys).to eq(['business_input + 1'])
      expect(calculator.ast_cache.inspect).not_to include('424242')
    end
  end
end
