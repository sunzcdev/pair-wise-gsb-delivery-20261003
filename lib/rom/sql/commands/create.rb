# frozen_string_literal: true

require 'rom/sql/commands/error_wrapper'

module ROM
  module SQL
    module Commands
      # Value returned in place of an inserted tuple when its contents cannot be
      # read back from the database
      #
      # It is used instead of guessing values in Ruby, so that callers can tell
      # "the database stored this" apart from "we don't know what it stored".
      #
      # @api public
      class Unreadable
        # Raised when tuple contents are requested from an unreadable result
        Error = Class.new(StandardError)

        # @api private
        def inspect = '#<ROM::SQL::Commands::Unreadable>'
        alias_method :to_s, :inspect

        # @raise [Error] always, because the contents are unknown
        #
        # @api public
        def to_h
          raise Error, 'inserted tuple could not be read back from the database'
        end

        # @api public
        def ==(other) = other.is_a?(self.class)
        alias_method :eql?, :==

        # @api private
        def hash = self.class.hash
      end

      # SQL create command
      #
      # @api public
      class Create < ROM::Commands::Create
        # Returned per-tuple when the inserted row can not be read back
        #
        # @return [Unreadable]
        #
        # @api public
        UNREADABLE = Unreadable.new.freeze

        adapter :sql

        include ErrorWrapper

        use :associates
        use :schema

        after :finalize

        # Build a command with input based on the relation's create input schema
        #
        # Unlike the generic `:schema` plugin, this uses
        # {ROM::SQL::Schema#to_create_input_hash}, which doesn't resolve
        # schema-level defaults for omitted attributes.
        #
        # @api public
        def self.build(relation, **options)
          if relation.schema? && !options.key?(:input)
            relation_input = relation_input_schema(relation)
            command_input = input

            composed_input =
              if command_input.equal?(ROM::Command.input)
                relation_input
              else
                -> tuple { relation_input[command_input[tuple]] }
              end

            super(relation, **options, input: composed_input)
          else
            super
          end
        end

        # @api private
        def self.relation_input_schema(relation)
          if relation.schema.respond_to?(:to_create_input_hash)
            relation.schema.to_create_input_hash
          else
            relation.input_schema
          end
        end
        private_class_method :relation_input_schema

        # Inserts provided tuples into the database table
        #
        # Attributes omitted by the caller are not filled in with schema-level
        # defaults - they are left to the database, so that column defaults
        # (including expressions evaluated server-side) can take effect. An
        # explicit `nil` is preserved and goes through regular column
        # constraint checks.
        #
        # @api public
        def execute(tuples)
          insert_tuples = with_input_tuples(tuples) do |tuple|
            attributes = input[tuple]
            attributes.to_h
          end

          if insert_tuples.length > 1
            multi_insert(insert_tuples)
          else
            insert(insert_tuples)
          end
        end

        private

        # @api private
        def finalize(tuples, *)
          tuples.map { |t| t.equal?(UNREADABLE) ? t : relation.output_schema[t] }
        end

        # Executes insert statement and returns inserted tuples
        #
        # The inserted row is read back from the database, so that the returned
        # tuple reflects what was actually stored, including values generated
        # server-side. When the row cannot be read back, {UNREADABLE} is
        # returned in its place instead of values guessed in Ruby.
        #
        # @api private
        def insert(tuples)
          return insert_with_returning(tuples) if returning?

          pks = tuples.map { |tuple| relation.insert(tuple) }
          read_back(tuples, pks)
        end

        # Executes multi_insert statement and returns inserted tuples
        #
        # Tuples are batched by the set of columns they provide, so that a
        # tuple omitting a value still gets the column default applied by the
        # database, rather than an explicit NULL caused by another tuple that
        # happens to provide that column.
        #
        # Batches keep the original tuple order, so the returned tuples
        # correspond to the input tuples one by one.
        #
        # The whole series of inserts runs in a transaction, so a failure
        # doesn't leave a partially inserted batch behind.
        #
        # @api private
        def multi_insert(tuples)
          relation.transaction do
            if returning?
              insert_with_returning(tuples)
            else
              pks = tuple_batches(tuples).flat_map { |batch|
                relation.multi_insert(batch, return: :primary_key)
              }

              read_back(tuples, pks)
            end
          end
        end

        # Split tuples into consecutive batches sharing the same column set
        #
        # @api private
        def tuple_batches(tuples)
          tuples.chunk_while { |tuple, other| tuple.keys == other.keys }.to_a
        end

        # Insert tuples one by one, returning rows read back by the database
        #
        # Inserting per record is what makes it possible to delegate column
        # defaults to the database, because each record may provide a
        # different set of columns.
        #
        # @api private
        def insert_with_returning(tuples)
          tuples.flat_map { |tuple| returning_dataset.insert(tuple) }
        end

        # Check if inserted rows can be read back as part of the insert
        #
        # @api private
        def returning?
          dataset = relation.dataset

          dataset.respond_to?(:supports_returning?) && dataset.supports_returning?(:insert)
        end

        # Dataset selecting all schema attributes, used with RETURNING
        #
        # @api private
        def returning_dataset
          relation.dataset.returning(*relation.schema.map(&:qualified_projection))
        end

        # Read back inserted rows, falling back to an explicit status
        #
        # When rows can't be attributed to the inserted tuples one by one, all
        # of them are reported as unreadable.
        #
        # @api private
        def read_back(tuples, pks)
          rows = fetch_inserted_rows(tuples, pks)

          rows && rows.length == tuples.length ? rows : Array.new(tuples.length, UNREADABLE)
        end

        # Fetch inserted rows by primary key, preserving the input order
        #
        # Rows are identified by the values reported by the insert, or - when
        # those aren't usable - by the primary key values provided in the
        # tuples themselves (which covers composite primary keys).
        #
        # Returns `nil` when the inserted rows cannot be identified, in which
        # case their actual contents are unknown.
        #
        # @api private
        def fetch_inserted_rows(tuples, pks)
          pk_attrs = relation.schema.primary_key

          return if pk_attrs.empty?

          keys = insert_keys(tuples, pks, pk_attrs)

          return unless keys

          rows = rows_by_key(keys, pk_attrs)

          keys.map { |key| rows[key] } if keys.all? { |key| rows.key?(key) }
        end

        # Return rows indexed by their primary key values
        #
        # @api private
        def rows_by_key(keys, pk_attrs)
          key_index = pk_attrs.map(&:key)

          relation.dataset.where(pk_attrs.map(&:name) => keys).to_a.to_h { |row|
            [key_index.map { |key| row[key] }, row]
          }
        end

        # Return primary key values identifying each inserted tuple
        #
        # @api private
        def insert_keys(tuples, pks, pk_attrs)
          return pks.map { |pk| [pk] } if inserted_key_values?(tuples, pks, pk_attrs)

          keys = tuples.map { |tuple| pk_attrs.map { |attr| tuple[attr.name] } }

          keys if keys.none? { |key| key.include?(nil) }
        end

        # Check if inserted primary key values can identify all inserted tuples
        #
        # @api private
        def inserted_key_values?(tuples, pks, pk_attrs)
          pk_attrs.size == 1 && pks.length == tuples.length && pks.none?(&:nil?)
        end

        # Yields tuples for insertion or return an enumerator
        #
        # @api private
        def with_input_tuples(tuples, &)
          input_tuples = [tuples].flatten(1).map
          return input_tuples unless block_given?

          input_tuples.each(&)
        end
      end
    end
  end
end
