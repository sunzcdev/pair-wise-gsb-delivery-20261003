# frozen_string_literal: true

require 'rom/sql/commands/create'

RSpec.describe ROM::SQL::Commands::Create do
  subject(:command) do
    relations[:books].command(:create)
  end

  include_context 'database setup'

  with_adapters do
    after do
      conn.drop_table?(:books)
    end

    describe '#call' do
      setup_tables do
        conn.create_table :books do
          primary_key :id
          column :author, String
          column :title, String
        end
      end

      setup_relations do
        conf.relation(:books) do
          schema do
            attribute :id, ROM::SQL::Types::Serial
            attribute :title, ROM::SQL::Types::String
          end
        end
      end

      it 'returns a tuple matching custom schema' do
        expect(command.call(title: 'Hello World')).to eql(id: 1, title: 'Hello World')
      end
    end

    describe 'input processing' do
      setup_tables do
        conn.create_table :books do
          primary_key :id
          column :title, String
          column :rank, Integer, null: false, default: 1
        end
      end

      setup_relations do
        conf.relation(:books) do
          schema do
            attribute :id, ROM::SQL::Types::Serial
            attribute :title, ROM::SQL::Types::String
            attribute :rank, ROM::SQL::Types::Integer
          end
        end
      end

      it 'keeps omitted attributes out of the insert input' do
        expect(command.input.call(title: 'Hello World')).to eql(title: 'Hello World')
      end

      it 'keeps explicitly nil attributes in the insert input' do
        expect(command.input.call(title: 'Hello World', rank: nil)).to eql(
          title: 'Hello World', rank: nil
        )
      end
    end

    describe 'schema-level defaults' do
      setup_tables do
        conn.create_table :books do
          primary_key :id
          column :title, String
          column :rank, Integer, null: false, default: 1
        end
      end

      setup_relations do
        conf.relation(:books) do
          schema do
            attribute :id, ROM::SQL::Types::Serial
            attribute :title, ROM::SQL::Types::String
            attribute :rank, ROM::SQL::Types::Integer.default(42)
          end
        end
      end

      it 'does not resolve schema-level defaults for omitted attributes' do
        expect(command.input.call(title: 'Hello World')).to eql(title: 'Hello World')
      end

      it 'keeps provided values instead of schema-level defaults' do
        expect(command.input.call(title: 'Hello World', rank: 3)).to eql(
          title: 'Hello World', rank: 3
        )
      end

      it 'returns the value stored by the database for omitted attributes' do
        expect(command.call(title: 'Hello World')).to eql(id: 1, title: 'Hello World', rank: 1)
      end
    end
  end
end
