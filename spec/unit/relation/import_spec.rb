# frozen_string_literal: true

RSpec.describe ROM::Relation, '#import' do
  subject(:relation) { relations[:users] }

  include_context 'users'

  let(:source) { relations[:users_for_loading] }

  with_adapters(:postgres) do
    context 'within a single gateway' do
      setup_tables(users_for_loading: :db) do
        conn.drop_table?(:users_for_loading)

        conn.create_table(:users_for_loading) do
          primary_key :id
          column :full_name, String, null: false
        end
      end

      setup_relations do
        conf.relation(:users_for_loading) do
          schema(:users_for_loading, infer: true)
        end
      end

      seed do
        conn[:users_for_loading].insert(full_name: 'Jack')
        conn[:users_for_loading].insert(full_name: 'John')
      end

      it 'inserts data from another relation' do
        relation.import(source.project { [(id + 10).as(:id), full_name.as(:name)] })

        expect(relation.to_a).to eql([
          { id: 1, name: 'Jane' },
          { id: 2, name: 'Joe' },
          { id: 11, name: 'Jack' },
          { id: 12, name: 'John' }
        ])
      end
    end

    context 'with a different gateway' do
      let(:conf) { TestConfiguration.new(default: [:sql, conn], other: [:memory]) }
      let(:source_dataset) do
        data = [{ id: 11, name: 'Jack', age: 30 }, { id: 12, name: 'John', age: 40 }]
        ROM::Memory::Dataset.new(data)
      end

      setup_relations do
        conf.relation(:users_for_loading, adapter: :memory) do
          gateway :other

          schema(:users_for_loading) do
            attribute :id,   ROM::Types::Integer
            attribute :name, ROM::Types::String
            attribute :age,  ROM::Types::Integer
          end
        end
      end

      it 'inserts data' do
        relation.import(source.new(source_dataset).project(source[:id], source[:name]))

        expect(relation.to_a).to eql([
          { id: 1, name: 'Jane' },
          { id: 2, name: 'Joe' },
          { id: 11, name: 'Jack' },
          { id: 12, name: 'John' }
        ])
      end
    end
  end

  context 'with two local SQLite gateways', sqlite: true do
    subject(:target) { relations[:target_people] }

    include_context 'database setup'

    let(:source_conn) { Sequel.connect(DB_URIS[:sqlite]) }
    let(:conf) { TestConfiguration.new(default: [:sql, conn], source: [:sql, source_conn]) }

    let(:source) { relations[:source_people] }
    let(:import_batch) { ROM::SQL::Relation::Writing::ImportBatch }

    setup_tables do
      conn.drop_table?(:target_people)
      conn.drop_table?(:constrained_people)
      source_conn.drop_table?(:source_people)

      conn.create_table(:target_people) do
        primary_key :id
        column :name, String, null: false
      end

      conn.run(
        'CREATE TABLE constrained_people (' \
          'id INTEGER PRIMARY KEY AUTOINCREMENT, ' \
          "name varchar(255) NOT NULL CHECK (name NOT LIKE 'invalid%'))"
      )

      source_conn.create_table(:source_people) do
        primary_key :id
        column :name, String, null: false
      end
    end

    setup_relations do
      conf.relation(:source_people) do
        gateway :source
        schema(:source_people, infer: true)
      end

      conf.relation(:target_people) do
        schema(:target_people, infer: true)
      end

      conf.relation(:constrained_people) do
        schema(:constrained_people, infer: true)
      end
    end

    seed do
      6.times { |index| source_conn[:source_people].insert(name: "person #{index}") }
    end

    after do
      source_conn.disconnect
    end

    it 'imports rows through bounded limit/offset batches and reports each batch' do
      committed = []
      batches = target.import(source, slice: 2) { |batch| committed << batch }

      expect(batches).to eql([
        import_batch.new(index: 0, offset: 0, rows: 2),
        import_batch.new(index: 1, offset: 2, rows: 2),
        import_batch.new(index: 2, offset: 4, rows: 2)
      ])
      expect(committed).to eql(batches)
      expect(target.count).to eq(6)
    end

    it 'keeps earlier batches when a later batch fails and resumes from the cursor' do
      # counting cursor: the third batch write is forced to fail
      attempts = 0
      target.define_singleton_method(:write_import_batch) do |columns, values, options|
        attempts += 1
        raise 'third batch failed' if attempts == 3

        self.class.instance_method(:write_import_batch).bind_call(self, columns, values, options)
      end

      committed = []

      expect {
        target.import(source, slice: 2) { |batch| committed << batch }
      }.to raise_error(RuntimeError, 'third batch failed')

      # only the batches committed before the failure are visible
      expect(committed).to eql([
        import_batch.new(index: 0, offset: 0, rows: 2),
        import_batch.new(index: 1, offset: 2, rows: 2)
      ])
      expect(target.to_a.map { |row| row[:name] }).to eql(
        ['person 0', 'person 1', 'person 2', 'person 3']
      )

      # the failed batch itself was rolled back, nothing partial was written
      expect(target.count).to eq(4)

      # recovery: remove the fault and resume past the committed cursor,
      # so already committed rows are never rewritten
      target.singleton_class.send(:remove_method, :write_import_batch)

      resumed = target.import(source, slice: 2, offset: 4)

      expect(resumed).to eql([import_batch.new(index: 0, offset: 4, rows: 2)])
      expect(target.count).to eq(6)
      expect(target.to_a.map { |row| row[:id] }).to eql([1, 2, 3, 4, 5, 6])
    end

    it 'never materializes the whole source relation in memory' do
      logged_sql = []
      query_logger = Object.new
      query_logger.define_singleton_method(:info) { |message| logged_sql << message }
      source_conn.loggers << query_logger

      target.import(source, slice: 2)

      # three data batches plus one empty probe that stops the cursor,
      # every read is bounded by the same slice size
      batch_queries = logged_sql.grep(/LIMIT \d+ OFFSET/)

      expect(batch_queries.size).to eq(4)
      expect(batch_queries).to all(match(/LIMIT 2/))
      expect(batch_queries.map { |sql| sql[/OFFSET \d+/].to_s }).to eql(
        ['OFFSET 0', 'OFFSET 2', 'OFFSET 4', 'OFFSET 6']
      )

      # no read pulls the whole table: data queries always carry the limit
      data_queries = logged_sql.grep(/FROM .source_people./).grep_v(/LIMIT 1/)
      expect(data_queries).to all(match(/LIMIT 2/))
    end

    it 'rolls the whole failing batch back while keeping earlier batches' do
      # a real CHECK constraint rejects the last tuple of the third batch
      constrained = relations[:constrained_people]

      source_conn[:source_people].where(id: 6).update(name: 'invalid row')

      committed = []

      expect {
        constrained.import(source, slice: 2) { |batch| committed << batch }
      }.to raise_error(Sequel::ConstraintViolation)

      expect(committed.map(&:rows)).to eql([2, 2])
      # the third batch was fully rolled back, no partial rows remain
      expect(constrained.count).to eq(4)
    end

    it 'materializes the source relation when no slice is given' do
      target.import(source)

      expect(target.count).to eq(6)
    end

    it 'rejects a non-positive slice' do
      expect { target.import(source, slice: 0) }.to raise_error(
        ArgumentError, /\+:slice\+ must be a positive integer/
      )
    end

    it 'rejects a negative offset' do
      expect { target.import(source, slice: 2, offset: -1) }.to raise_error(
        ArgumentError, /\+:offset\+ must be a non-negative integer/
      )
    end
  end
end
