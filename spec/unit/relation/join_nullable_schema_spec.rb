# frozen_string_literal: true

RSpec.describe ROM::Relation, 'outer join derived schemas' do
  subject(:relation) { relations[:users] }

  include_context 'database setup'

  let(:tasks) { container.relations[:tasks] }

  setup_tables do
    conn.create_table :users do
      primary_key :id
      column :name, String, null: false
    end

    conn.create_table :tasks do
      primary_key :id
      foreign_key :user_id, :users, null: false
      column :title, String, null: false
    end
  end

  setup_relations do
    conf.relation(:users) do
      schema do
        attribute :id, ROM::SQL::Types::Serial
        attribute :name, Types::String
      end
    end

    conf.relation(:tasks) do
      schema do
        attribute :id, ROM::SQL::Types::Serial
        attribute :user_id, Types::Integer
        attribute :title, Types::String
      end
    end
  end

  seed do
    conn[:users].insert id: 1, name: 'Jane'
    conn[:users].insert id: 2, name: 'Joe'
    conn[:tasks].insert id: 1, user_id: 1, title: "Jane's task"
    # Joe has a task, so the inner alias matches while the restricted left
    # alias can still be missing
    conn[:tasks].insert id: 2, user_id: 2, title: "Joe's task"
  end

  with_adapters do
    def find_attr(schema, key)
      schema.find { |attr| attr.key.equal?(key) }
    end

    context 'with two aliases of the same table' do
      let(:inner_alias) { Sequel.as(:tasks, :inner_tasks) }
      let(:left_alias) { Sequel.as(:tasks, :left_tasks) }

      def joined_relation
        relation
          .left_join(left_alias,
                     Sequel[:left_tasks][:user_id] => Sequel[:users][:id],
                     Sequel[:left_tasks][:id] => 1)
          .inner_join(inner_alias,
                      Sequel[:inner_tasks][:user_id] => Sequel[:users][:id])
      end

      it 'marks only attributes from the left join alias as optional' do
        result = joined_relation.select(
          relation[:name],
          tasks[:title].qualified(:inner_tasks).as(:inner_title),
          tasks[:title].qualified(:left_tasks).as(:left_title)
        )

        expect(find_attr(result.schema, :inner_title).optional?).to be(false)
        expect(find_attr(result.schema, :left_title).optional?).to be(true)
        expect(result.schema[:name].optional?).to be(false)
      end

      it 'reads nil from the missing left side without a type error' do
        result = joined_relation.select(
          relation[:name],
          tasks[:title].qualified(:inner_tasks).as(:inner_title),
          tasks[:title].qualified(:left_tasks).as(:left_title)
        )

        expect(result.to_a).to include(
          name: 'Joe', inner_title: "Joe's task", left_title: nil
        )
      end

      it 'keeps the inner-join alias non-null across another projection' do
        result = joined_relation
                 .select(relation[:name], tasks[:title].qualified(:inner_tasks))
                 .select(:name)

        expect(result.schema[:name].optional?).to be(false)
      end

      it 'keeps nullability after select_append' do
        result = relation
                 .left_join(left_alias,
                            Sequel[:left_tasks][:user_id] => Sequel[:users][:id],
                            Sequel[:left_tasks][:id] => 1)
                 .select_append(tasks[:title].qualified(:left_tasks).as(:left_title))

        expect(find_attr(result.schema, :left_title).optional?).to be(true)
        expect(result.to_a).to include(hash_including(name: 'Joe', left_title: nil))
      end
    end

    context 'with plain table names' do
      before do
        relation.insert id: 3, name: 'Jade'
      end

      it 'marks the joined side of a left join as optional' do
        result = relation
                 .left_join(:tasks, user_id: :id)
                 .select(:name, tasks[:title])

        expect(result.schema[:title].optional?).to be(true)
        expect(result.to_a).to include(name: 'Jade', title: nil)
      end

      it 'does not mark an inner join side as optional' do
        result = relation
                 .inner_join(:tasks, user_id: :id)
                 .select(:name, tasks[:title])

        expect(result.schema[:title].optional?).to be(false)
      end

      it 'marks the joined side of a right join as optional' do
        result = tasks
                 .right_join(:users, id: :user_id)
                 .select(relation[:name], tasks[:title])

        expect(result.schema[:title].optional?).to be(true)
        expect(result.to_a).to include(name: 'Jade', title: nil)
      end
    end

    context 'with the table_alias join option' do
      it 'marks aliased left join attributes as optional' do
        relation.insert id: 3, name: 'Jade'

        result = relation
                 .left_join(:tasks, { user_id: :id }, table_alias: :t1)
                 .select(:name, tasks[:title].qualified(:t1))

        expect(result.schema[:title].optional?).to be(true)
        expect(result.to_a).to include(name: 'Jade', title: nil)
      end
    end

    describe 'canonical relation constraints' do
      it 'keeps the original relation schemas non-null' do
        relation
          .left_join(:tasks, user_id: :id)
          .select(:name, tasks[:title])

        expect(tasks.schema[:title].optional?).to be(false)
        expect(relation.schema[:name].optional?).to be(false)
      end

      it 'still rejects nil writes through the original relation' do
        expect {
          tasks.command(:create).call(id: 10, user_id: 1, title: nil)
        }.to raise_error(Dry::Types::SchemaError, /:title/)
      end
    end
  end
end
