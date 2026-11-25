# frozen_string_literal: true

require_relative 'pushdown_spec_helper'

RSpec.describe ROM::SQL::Relation, 'SQL pushdown with bounded local fallback', :sqlite do
  let(:log) { ROM::SQL::Relation::Pushdown::ExecutionLog }

  describe 'gateway capability discovery' do
    it 'answers from the real connection instead of the last created gateway' do
      first = PushdownFakeConnection.new(:first, supports: true)
      second = PushdownFakeConnection.new(:second, supports: false)

      first_gateway = build_gateway(first)
      second_gateway = build_gateway(second)

      expect(first_gateway.supports_sql_feature?(:window_functions)).to be(true)
      expect(second_gateway.supports_sql_feature?(:window_functions)).to be(false)

      # swap creation order, decisions must stay attached to each connection
      third = PushdownFakeConnection.new(:third, supports: false)
      build_gateway(third)

      expect(first_gateway.supports_sql_feature?(:window_functions)).to be(true)
      expect(second_gateway.supports_sql_feature?(:window_functions)).to be(false)
    end

    it 'treats missing feature probes as unsupported rather than raising' do
      connection = PushdownFakeConnection.new(:plain, supports: false)
      gateway = build_gateway(connection)

      expect(gateway.supports_sql_feature?(:never_heard_of_it)).to be(false)
    end

    it 'resolves the owning gateway through the relation dataset connection' do
      connection = PushdownFakeConnection.new(:owned, supports: true)
      relation = build_relation(connection)
      gateway = connection.rom_sql_gateway

      expect(relation.source_gateway).to be(gateway)
      expect(relation.source_gateway.connection).to be(connection)
    end
  end

  describe 'interleaved execution of the same query' do
    let(:supporting_relation) { build_relation(PushdownFakeConnection.new(:capable, supports: true)) }
    let(:unsupported_relation) { build_relation(PushdownFakeConnection.new(:limited, supports: false)) }

    let(:query) do
      lambda { |relation|
        relation.with_sql_pushdown(:window_functions, fallback_limit: 100) do |r|
          r.where(age: 30).order(:name).select(:id, :name)
        end
      }
    end

    it 'pushes down on the capable double and falls back on the limited double' do
      entries = log.recording do
        @pushed = query.call(supporting_relation).to_a
        @fallback = query.call(unsupported_relation).to_a
        @pushed_again = query.call(supporting_relation).to_a
        @fallback_again = query.call(unsupported_relation).to_a
      end

      expect(entries.map(&:decision)).to eql(
        %i[pushed_down fallback pushed_down fallback]
      )
      expect(entries.map { |entry| entry.connection.label }).to eql(
        %i[capable limited capable limited]
      )
    end

    it 'keeps results and selected conditions identical across both paths' do
      pushed = query.call(supporting_relation).to_a
      fallback = query.call(unsupported_relation).to_a

      expect(pushed).to eql([id: 1, name: 'Jane'])
      expect(fallback).to eql(pushed)
    end

    it 'records how many tuples each fallback buffered' do
      entries = log.recording do
        query.call(unsupported_relation).to_a
      end

      fallback_entry = entries.find(&:fallback?)
      expect(fallback_entry.fetched).to be(1)
      expect(fallback_entry.feature).to be(:window_functions)
      expect(fallback_entry.relation).to be(:users)
    end

    it 'does not execute any SQL pushdown on the limited connection' do
      limited = unsupported_relation
      allow(limited.dataset.db).to receive(:supports_window_functions?).and_call_original

      log.recording { query.call(limited).to_a }

      entry = log.entries.last
      expect(entry).to be_fallback
      expect(entry.connection.label).to be(:limited)
    end
  end

  describe 'rejected fallback when the bounded scope is exceeded' do
    let(:crowded) do
      connection = PushdownFakeConnection.new(
        :crowded,
        supports: false,
        rows: 6.times.map { |index| { id: index, name: "user#{index}", age: index } }
      )
      build_relation(connection)
    end

    it 'raises once and records a rejected entry instead of buffering everything' do
      entries = log.recording do
        expect {
          crowded.with_sql_pushdown(:window_functions, fallback_limit: 5) { |r| r }.to_a
        }.to raise_error(ROM::SQL::FallbackExceededError)
      end

      expect(entries.size).to be(1)
      expect(entries.first).to be_rejected
      expect(entries.first.reason).to be(:fallback_limit_exceeded)
      expect(entries.first.fetched).to be(6)
    end

    it 'rejects immediately when fallback is disabled with a zero limit' do
      relation = build_relation(PushdownFakeConnection.new(:strict, supports: false))

      entries = log.recording do
        expect {
          relation.with_sql_pushdown(:window_functions, fallback_limit: 0) { |r| r }.to_a
        }.to raise_error(ROM::SQL::FallbackExceededError)
      end

      expect(entries.first.reason).to be(:fallback_disabled)
      expect(entries.first.fetched).to be(0)
    end

    it 'mentions the feature and owning gateway in the rejection message' do
      relation = build_relation(PushdownFakeConnection.new(:strict, supports: false))

      expect {
        relation.with_sql_pushdown(:window_functions, fallback_limit: 0) { |r| r }.to_a
      }.to raise_error(
        ROM::SQL::FallbackExceededError,
        /window_functions.*strict/
      )
    end
  end

  describe 'execution log' do
    let(:relation) { build_relation(PushdownFakeConnection.new(:logged, supports: true)) }

    it 'isolates recordings and restores the surrounding trail' do
      log.record(
        ROM::SQL::Relation::Pushdown::ExecutionEntry.new(
          feature: :outer, connection: nil, decision: :fallback, relation: :users
        )
      )

      inside = log.recording do
        relation.with_sql_pushdown(:window_functions) { |r| r }.to_a
      end

      expect(inside.map(&:feature)).to eql([:window_functions])
      expect(log.entries.map(&:feature)).to eql(%i[outer window_functions])
    end

    it 'exposes predicate helpers on each entry' do
      log.recording do
        relation.with_sql_pushdown(:window_functions) { |r| r }.to_a
      end

      entry = log.entries.last
      expect(entry).to be_pushed_down
      expect(entry).not_to be_fallback
      expect(entry).not_to be_rejected
      expect(entry.to_h).to include(
        feature: :window_functions,
        decision: :pushed_down,
        relation: :users,
        reason: nil,
        fetched: 0
      )
    end
  end

  describe 'with a real SQLite connection' do
    let(:container) { sqlite_container.first }
    let(:users) { container.relations[:users] }

    it 'pushes a supported expression down and preserves the condition' do
      entries = log.recording do
        @result = users.with_sql_pushdown(:cte) { |r|
          r.where(age: 30).order(:name).select(:id, :name)
        }.to_a
      end

      expect(@result).to eql([id: 1, name: 'Jane'])
      expect(entries.first).to be_pushed_down
      expect(entries.first.connection).to be(users.source_gateway.connection)
    end

    it 'falls back locally when a feature probe is stubbed off' do
      users.source_gateway.connection.define_singleton_method(:supports_cte?) { false }

      entries = log.recording do
        @result = users.with_sql_pushdown(:cte, fallback_limit: 10) { |r|
          r.where(age: 30).order(:name).select(:id, :name)
        }.to_a
      end

      expect(@result).to eql([id: 1, name: 'Jane'])
      expect(entries.first).to be_fallback
      expect(entries.first.fetched).to be(1)
    end

    it 'leaves relations without a plan on the regular materialization path' do
      expect(users.where(age: 40).to_a).to eql([id: 3, name: 'Ann', age: 40])
      expect(log.entries).to be_empty
    end

    it 'evaluates a nested plan on its own merits rather than inheriting the outer plan' do
      users.source_gateway.connection.define_singleton_method(:supports_magic?) { false }

      result = users.with_sql_pushdown(:cte) do |relation|
        relation.with_sql_pushdown(:magic, fallback_limit: 10) do |inner|
          inner.where(age: 30).select(:name)
        end
      end.to_a

      expect(result).to eql([name: 'Jane'])
      expect(log.entries.map(&:decision)).to eql(%i[pushed_down fallback])
      expect(log.entries.map(&:feature)).to eql(%i[cte magic])
    end

    it 're-evaluates composed predicates and ordering during a local fallback' do
      users.source_gateway.connection.define_singleton_method(:supports_cte?) { false }

      result = users.with_sql_pushdown(:cte, fallback_limit: 10) { |r|
        r.where { (age > 18) & (age < 35) }.order(Sequel.desc(:name))
      }.to_a

      expect(result.map { |tuple| tuple[:name] }).to eql(%w[Joe Jane])
      expect(log.entries.last).to be_fallback
    end

    it 're-evaluates LIKE and IN predicates and offset/limit locally' do
      users.source_gateway.connection.define_singleton_method(:supports_cte?) { false }

      like_result = users.with_sql_pushdown(:cte, fallback_limit: 10) { |r|
        r.where { name.like('J%') }.order(:name)
      }.to_a
      expect(like_result.map { |tuple| tuple[:name] }).to eql(%w[Jane Joe])

      in_result = users.with_sql_pushdown(:cte, fallback_limit: 10) { |r|
        r.where(age: [20, 40]).order(:name)
      }.to_a
      expect(in_result.map { |tuple| tuple[:name] }).to eql(%w[Ann Joe])

      paged = users.with_sql_pushdown(:cte, fallback_limit: 10) { |r|
        r.order(:name).limit(2).offset(1)
      }.to_a
      expect(paged.map { |tuple| tuple[:name] }).to eql(%w[Jane Joe])
    end
  end
end
