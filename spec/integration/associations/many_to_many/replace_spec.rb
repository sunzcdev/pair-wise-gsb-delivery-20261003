# frozen_string_literal: true

RSpec.describe ROM::SQL::Associations::ManyToMany, '#replace' do
  include_context 'users'

  subject(:assoc) { relations[:users].associations[:tags] }

  let(:users) { relations[:users] }
  let(:tags) { relations[:tags] }
  let(:user_tags) { relations[:user_tags] }

  before do
    inferrable_relations.push(:tags, :user_tags)
  end

  with_adapters do
    setup_tables do
      conn.create_table(:tags) do
        primary_key :id
        String :name
      end

      conn.create_table(:user_tags) do
        primary_key %i[user_id tag_id]
        foreign_key :user_id, :users
        foreign_key :tag_id, :tags
        Integer :rank
        index %i[user_id rank], unique: true
      end
    end

    setup_relations do
      conf.relation(:users) do
        schema(infer: true) do
          associations do
            has_many :user_tags
            has_many :tags, through: :user_tags
          end
        end
      end

      conf.relation(:tags) do
        schema(infer: true) do
          associations do
            has_many :user_tags
            has_many :users, through: :user_tags
          end
        end
      end

      conf.relation(:user_tags) do
        schema(infer: true) do
          associations do
            belongs_to :user
            belongs_to :tag
          end
        end
      end
    end

    seed do
      tags.insert(id: 1, name: 'shared')
      tags.insert(id: 2, name: 'second')
      tags.insert(id: 3, name: 'third')

      user_tags.insert(user_id: jane_id, tag_id: 1, rank: 10)
      user_tags.insert(user_id: jane_id, tag_id: 2, rank: 20)
      user_tags.insert(user_id: joe_id, tag_id: 1, rank: 30)
    end

    def jane_tags
      user_tags.where(user_id: jane_id).order(:tag_id).to_a
    end

    def joe_tags
      user_tags.where(user_id: joe_id).order(:tag_id).to_a
    end

    it 'only adds and removes the edges that changed' do
      changes = assoc.replace([{id: jane_id}], [{id: 1}, {id: 3, rank: 40}])

      expect(changes).to eql(
        added: [{user_id: jane_id, tag_id: 3, rank: 40}],
        removed: [{user_id: jane_id, tag_id: 2, rank: 20}]
      )
    end

    it 'keeps the join attributes of members that stay' do
      assoc.replace([{id: jane_id}], [{id: 1, rank: 999}, {id: 3, rank: 40}])

      expect(jane_tags).to eql([
        {user_id: jane_id, tag_id: 1, rank: 10},
        {user_id: jane_id, tag_id: 3, rank: 40}
      ])
    end

    it 'does not write anything when the members did not change' do
      changes = assoc.replace([{id: jane_id}], [{id: 1, rank: 999}, {id: 2, rank: 999}])

      expect(changes).to eql(added: [], removed: [])
      expect(jane_tags).to eql([
        {user_id: jane_id, tag_id: 1, rank: 10},
        {user_id: jane_id, tag_id: 2, rank: 20}
      ])
    end

    it 'can remove every member' do
      changes = assoc.replace([{id: jane_id}], [])

      expect(changes).to eql(
        added: [],
        removed: [
          {user_id: jane_id, tag_id: 1, rank: 10},
          {user_id: jane_id, tag_id: 2, rank: 20}
        ]
      )
      expect(jane_tags).to eql([])
    end

    it 'keeps the same member only once' do
      changes = assoc.replace([{id: jane_id}], [{id: 3, rank: 40}, {id: 3, rank: 41}])

      expect(changes).to eql(
        added: [{user_id: jane_id, tag_id: 3, rank: 40}],
        removed: [
          {user_id: jane_id, tag_id: 1, rank: 10},
          {user_id: jane_id, tag_id: 2, rank: 20}
        ]
      )
    end

    it 'replaces members of several sources at once' do
      changes = assoc.replace([{id: jane_id}, {id: joe_id}], [{id: 2, rank: 50}])

      expect(changes).to eql([
        {
          added: [],
          removed: [{user_id: jane_id, tag_id: 1, rank: 10}]
        },
        {
          added: [{user_id: joe_id, tag_id: 2, rank: 50}],
          removed: [{user_id: joe_id, tag_id: 1, rank: 30}]
        }
      ])

      expect(jane_tags).to eql([{user_id: jane_id, tag_id: 2, rank: 20}])
      expect(joe_tags).to eql([{user_id: joe_id, tag_id: 2, rank: 50}])
    end

    it 'stops the change when a new edge is rejected' do
      expect {
        assoc.replace([{id: jane_id}], [{id: 1}, {id: 3, rank: 10}])
      }.to raise_error(ROM::SQL::UniqueConstraintError)
    end

    it 'keeps the previous members and their join attributes when a new edge is rejected' do
      expect {
        assoc.replace([{id: jane_id}], [{id: 1}, {id: 3, rank: 10}])
      }.to raise_error(ROM::SQL::UniqueConstraintError)

      expect(jane_tags).to eql([
        {user_id: jane_id, tag_id: 1, rank: 10},
        {user_id: jane_id, tag_id: 2, rank: 20}
      ])
    end

    it 'never touches the other owner of the same target when a change fails' do
      expect {
        assoc.replace([{id: jane_id}], [{id: 1}, {id: 3, rank: 10}])
      }.to raise_error(ROM::SQL::UniqueConstraintError)

      expect(joe_tags).to eql([{user_id: joe_id, tag_id: 1, rank: 30}])
      expect(users.by_pk(joe_id).combine(:tags).one[:tags]).to eql([
        {id: 1, name: 'shared', user_id: joe_id}
      ])
    end

    it 'rolls back only the replacement when a transaction is already open' do
      conn.transaction do
        user_tags.where(user_id: jane_id, tag_id: 2).delete

        begin
          # tag 2 is gone now, so it would be inserted with rank 10 which is
          # already taken by tag 1
          assoc.replace([{id: jane_id}], [{id: 1, rank: 10}, {id: 2, rank: 10}])
        rescue ROM::SQL::UniqueConstraintError
          # expected, the outer transaction goes on
        end

        user_tags.insert(user_id: jane_id, tag_id: 3, rank: 40)
      end

      expect(jane_tags).to eql([
        {user_id: jane_id, tag_id: 1, rank: 10},
        {user_id: jane_id, tag_id: 3, rank: 40}
      ])
    end

    it 'raises when a member has no primary key' do
      expect { assoc.replace([{id: jane_id}], [{name: 'third'}]) }
        .to raise_error(KeyError, /id/)
    end
  end
end
