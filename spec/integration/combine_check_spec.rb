# frozen_string_literal: true

RSpec.describe 'Relation#combine_check' do
  include_context 'database setup'

  with_adapters do
    let(:tasks) { container.relations[:tasks] }
    let(:users) { container.relations[:users] }

    before do
      inferrable_relations.push(:users, :tasks)
    end

    # A legacy database without enforced foreign keys, where NULL foreign
    # keys, dangling keys and valid rows are interleaved
    setup_tables do
      conn.create_table :users do
        primary_key :id
        String :name
      end

      conn.create_table :tasks do
        primary_key :id
        Integer :user_id
        String :title
      end
    end

    setup_relations do
      conf.relation(:users) do
        schema(infer: true) do
          associations do
            has_many :tasks
          end
        end
      end

      conf.relation(:tasks) do
        schema(infer: true) do
          associations do
            belongs_to :user
            belongs_to :user, as: :assignee
          end
        end
      end
    end

    seed do
      conn[:users].insert id: 1, name: 'Jane'
      conn[:users].insert id: 2, name: 'Joe'

      conn[:tasks].insert id: 1, user_id: 2,    title: "Joe's task"
      conn[:tasks].insert id: 2, user_id: nil,  title: 'Unassigned task'
      conn[:tasks].insert id: 3, user_id: 404,  title: 'Orphaned task'
      conn[:tasks].insert id: 4, user_id: 1,    title: "Jane's task"
      conn[:tasks].insert id: 5, user_id: nil,  title: 'Another unassigned task'
      conn[:tasks].insert id: 6, user_id: 555,  title: 'Another orphaned task'
    end

    it 'reports a status per owner while the projection hides matching keys' do
      report = tasks.select(:title).combine_check(:user)

      expect(report).to eql([
        { title: "Joe's task", user_status: :ok },
        { title: 'Unassigned task', user_status: :missing_key },
        { title: 'Orphaned task', user_status: :missing_target },
        { title: "Jane's task", user_status: :ok },
        { title: 'Another unassigned task', user_status: :missing_key },
        { title: 'Another orphaned task', user_status: :missing_target }
      ])
    end

    it 'tells NULL foreign keys apart from dangling ones per owner' do
      report = tasks.select(:title).combine_check(:user)

      by_status = report.group_by { |tuple| tuple[:user_status] }

      expect(by_status[:missing_key].map { |tuple| tuple[:title] })
        .to eql(['Unassigned task', 'Another unassigned task'])

      expect(by_status[:missing_target].map { |tuple| tuple[:title] })
        .to eql(['Orphaned task', 'Another orphaned task'])
    end

    it 'keeps explicitly projected keys in the output' do
      report = tasks.select(:id, :title).combine_check(:user)

      expect(report.map { |tuple| tuple[:id] }).to eql([1, 2, 3, 4, 5, 6])
      expect(report.map { |tuple| tuple[:user_status] })
        .to eql(%i[ok missing_key missing_target ok missing_key missing_target])
    end

    it 'checks restricted relations' do
      report = tasks.where(user_id: nil).select(:title).combine_check(:user)

      expect(report).to eql([
        { title: 'Unassigned task', user_status: :missing_key },
        { title: 'Another unassigned task', user_status: :missing_key }
      ])
    end

    it 'checks many associations at once' do
      report = tasks.select(:title).combine_check(:user, :assignee)

      expect(report.first).to eql(
        title: "Joe's task", user_status: :ok, assignee_status: :ok
      )
    end

    it 'raises when an association is not a many-to-one' do
      expect { users.combine_check(:tasks) }.to raise_error(
        ArgumentError, /only many-to-one associations can be checked/
      )
    end
  end
end
