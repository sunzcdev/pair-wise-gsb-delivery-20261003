# frozen_string_literal: true

module ROM
  module SQL
    class Relation < ROM::Relation
      module Writing
        # Status of a single batch imported from a different gateway
        #
        # @!attribute [r] index
        #   @return [Integer] Zero-based position of the batch within the import
        #
        # @!attribute [r] offset
        #   @return [Integer] Offset of the batch inside the source relation.
        #     Use it as the cursor to resume a failed import without rewriting
        #     the tuples that were already committed
        #
        # @!attribute [r] rows
        #   @return [Integer] Number of tuples written with the batch
        #
        # @api public
        ImportBatch = ::Struct.new(:index, :offset, :rows, keyword_init: true)

        # Add upsert option (only PostgreSQL >= 9.5)
        # Uses internal Sequel implementation
        # Default - ON CONFLICT DO NOTHING
        # more options: http://sequel.jeremyevans.net/rdoc-adapters/classes/Sequel/Postgres/DatasetMethods.html#method-i-insert_conflict
        #
        # @example
        #   users.upsert({ name: 'Jane', email: 'jane@foo.com' },
        #                { target: :email, update: { name: :excluded__name } })
        #
        # @return [Integer] Number of affected rows
        #
        # @api public
        def upsert(*args, &)
          if args.size > 1 && args[-1].is_a?(Hash)
            *values, opts = args
          else
            values = args
            opts = EMPTY_HASH
          end

          dataset.insert_conflict(opts).insert(*values, &)
        end

        # Insert tuple into relation
        #
        # @example
        #   users.insert(name: 'Jane')
        #
        # @param [Hash] args
        #
        # @return [Hash] Inserted tuple
        #
        # @api public
        def insert(...)
          dataset.insert(...)
        end

        # Multi insert tuples into relation
        #
        # @example
        #   users.multi_insert([{name: 'Jane'}, {name: 'Jack'}])
        #
        # @param [Array<Hash>] args
        #
        # @return [Array<String>] A list of executed SQL statements
        #
        # @api public
        def multi_insert(...)
          dataset.multi_insert(...)
        end

        # Update tuples in the relation
        #
        # @example
        #   users.update(name: 'Jane')
        #   users.where(name: 'Jane').update(name: 'Jane Doe')
        #
        # @return [Integer] Number of updated rows
        #
        # @api public
        def update(...)
          dataset.update(...)
        end

        # Delete tuples from the relation
        #
        # @example
        #   users.delete # deletes all
        #   users.where(name: 'Jane').delete # delete tuples
        #                                      from restricted relation
        #
        # @return [Integer] Number of deleted tuples
        #
        # @api public
        def delete(...)
          dataset.delete(...)
        end

        # Insert tuples from another relation
        #
        # NOTE: The method implicitly uses a transaction
        #
        # @example
        #   users.import(new_users)
        #
        # @overload import(other_sql_relation, options)
        #   If both relations uses the same gateway
        #   the INSERT ... SELECT statement will
        #   be used for importing the data
        #
        #   @param [SQL::Relation] other_sql_relation
        #
        #   @option [Integer] :slice
        #     Split loading into batches of provided size,
        #     every batch will be processed in a separate
        #     transaction block
        #
        # @overload import(other, options)
        #   Import data from another relation. When +:slice+ is provided the
        #   source relation is never fully materialized: it is read through a
        #   count/limit/offset cursor and only one batch is kept in memory at
        #   a time. Every batch is written in its own transaction, so batches
        #   committed before a failure stay committed. Without +:slice+ the
        #   source relation is materialized before loading, as before
        #
        #   @param [Relation] other
        #
        #   @option [Integer] :slice
        #     Max number of tuples fetched and written per batch
        #
        #   @option [Integer] :offset
        #     Cursor inside the source relation. Rows before it are skipped,
        #     which allows resuming an import from the last committed batch
        #
        # @yieldparam [ROM::SQL::Relation::Writing::ImportBatch] batch
        #   Called after every successfully committed batch
        #
        # @return [Integer, Array] Number of imported tuples for the legacy
        #   paths; an array of {ImportBatch} when batching is used with a
        #   relation from a different gateway
        #
        # @api public
        def import(other, options = EMPTY_HASH, &block)
          if other.gateway.eql?(gateway)
            dataset.import(import_columns(other), other.dataset, options)
          elsif options[:slice]
            import_in_batches(other, options, &block)
          else
            dataset.import(import_columns(other), import_rows(other), options)
          end
        end

        private

        # Columns used to load tuples from another relation
        #
        # @api private
        def import_columns(other)
          other.schema.map { |attribute| attribute.alias || attribute.name }
        end

        # Materialize tuples from another relation as plain value arrays
        #
        # @api private
        def import_rows(other)
          keys = import_columns(other).map(&:to_sym)

          other.to_a.map { |record| record.to_h.values_at(*keys) }
        end

        # Stream tuples from a relation on a different gateway in bounded
        # batches using a count/limit/offset cursor. Each batch is committed
        # in its own transaction and reported through the returned array and
        # the optional block
        #
        # @api private
        def import_in_batches(other, options, &block)
          slice = options.fetch(:slice)

          unless slice.is_a?(::Integer) && slice.positive?
            raise ArgumentError, "+:slice+ must be a positive integer, #{slice.inspect} given"
          end

          offset = options.fetch(:offset, 0)

          unless offset.is_a?(::Integer) && offset >= 0
            raise ArgumentError, "+:offset+ must be a non-negative integer, #{offset.inspect} given"
          end

          insert_options = options.except(:slice, :offset)
          columns = import_columns(other)
          keys = columns.map(&:to_sym)
          batches = []
          index = 0

          loop do
            rows = import_batch_rows(other, slice, offset)

            break if rows.empty?

            values = rows.map { |record| record.to_h.values_at(*keys) }
            write_import_batch(columns, values, insert_options)

            batch = ImportBatch.new(index: index, offset: offset, rows: values.size)
            block&.call(batch)
            batches << batch

            offset += values.size
            index += 1
          end

          batches
        end

        # Fetch at most +slice+ tuples starting at +offset+. SQL relations are
        # paged in the database with a limit/offset cursor, so only one batch
        # is ever held in memory; other adapters fall back to array slicing,
        # which is still bounded by the batch size
        #
        # @api private
        def import_batch_rows(other, slice, offset)
          if other.respond_to?(:limit) && other.respond_to?(:offset)
            other.limit(slice).offset(offset).to_a
          else
            other.to_a[offset, slice].to_a
          end
        end

        # Write a single batch inside an independent transaction so a failure
        # rolls the batch back without touching the already committed ones
        #
        # @api private
        def write_import_batch(columns, values, insert_options)
          dataset.db.transaction do
            dataset.import(columns, values, insert_options)
          end
        end
      end
    end
  end
end
