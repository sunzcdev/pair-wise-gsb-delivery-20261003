# frozen-string-literal: true
#
# The shard_merge extension adds Database#shard_merge, an explicit entry
# point for merging rows from multiple shards (servers) that share the
# same schema, returning the top rows according to a single ordering
# applied to every shard:
#
#   DB.extension :shard_merge
#   reader = DB.shard_merge([:shard1, :shard2, :shard3], :events,
#     order: [Sequel.desc(:created_at), :id],
#     unique: :id,
#     limit: 100)
#   reader.each{|row| ... }
#
# The first argument is an array of shard/server names, as defined by the
# Database :servers option.  The second argument is either a table name
# or a dataset (which may be filtered); the given :order is applied to
# the dataset on each shard.
#
# Options:
#
# :order :: Required, non-empty array of order entries.  Each entry must
#           be a Symbol (ascending) or an expression such as
#           Sequel.asc(:column)/Sequel.desc(:column) wrapping a plain
#           column.  The final entry must be the unique tiebreaker
#           column named by :unique, so that the ordering is total and
#           the merge is deterministic even when rows on different
#           shards have equal values in the leading columns.
# :unique :: Required, the unique tiebreaker column (a Symbol).  Must
#            match the final :order entry, and must contain unique
#            values across all shards (a primary key is typical).
# :limit :: Maximum number of rows to yield (the "top N").  If not
#           given, all matching rows from all shards are merged.
# :page_size :: Number of rows read from a shard per query (default
#               1000).  Rows are read from each shard in pages of this
#               size as the merge advances, so shards are never loaded
#               into memory in full.
#
# shard_merge returns a Sequel::ShardMerge::Reader, which includes
# Enumerable and is lazily consumed: no queries are issued until the
# reader is iterated, and each shard is only queried for another page
# when the merge has consumed all rows buffered from that shard.  The
# reader is single pass; when iteration completes, when the caller
# stops early (e.g. via break or Enumerable#take), or when a read
# raises an exception, all shard cursors are deterministically closed.
# Readers can also be closed explicitly via Reader#close.  Exceptions
# raised while reading a shard are propagated to the caller after all
# shard cursors have been closed.
#
# Note on transaction semantics: each page is read from each shard using
# an independent query on that shard's own connection.  Transaction
# semantics are limited to each individual shard; shard_merge does not
# provide (and does not claim to provide) a consistent snapshot across
# shards, so rows merged from different shards may reflect different
# points in time.
#
# Related module: Sequel::ShardMerge

#
module Sequel
  module ShardMerge
    # Return a lazy Reader merging rows from the given shards using the
    # same ordering on each shard.  See the extension documentation for
    # details on the supported options.
    def shard_merge(shards, table, opts=OPTS)
      raise(Error, "shard_merge requires at least one shard") if shards.empty?

      order = opts[:order]
      keys = ShardMerge.order_keys(order, opts[:unique])

      limit = opts[:limit]
      if limit && (!limit.is_a?(Integer) || limit < 1)
        raise(Error, "shard_merge :limit must be a positive Integer")
      end

      page_size = opts[:page_size] || 1000
      unless page_size.is_a?(Integer) && page_size > 0
        raise(Error, "shard_merge :page_size must be a positive Integer")
      end

      dataset = table.is_a?(Dataset) ? table : self[table]
      sources = shards.map do |shard|
        ShardMerge::DatasetCursor.new(dataset.server(shard).order(*order), page_size)
      end
      ShardMerge::Reader.new(sources, keys, limit)
    end

    # Parse and validate the :order and :unique options, returning an
    # array of [column, descending] pairs.  Raises Sequel::Error unless
    # the ordering is non-empty, uses plain columns, and ends with the
    # unique tiebreaker column.
    def self.order_keys(order, unique)
      raise(Error, "shard_merge requires a non-empty :order option") if order.nil? || order.empty?

      keys = order.map do |entry|
        case entry
        when Symbol
          [entry, false]
        when SQL::OrderedExpression
          expression = entry.expression
          unless expression.is_a?(Symbol)
            raise(Error, "shard_merge :order entries must be plain columns, not #{expression.inspect}")
          end
          [expression, !!entry.descending]
        else
          raise(Error, "unsupported shard_merge :order entry: #{entry.inspect}")
        end
      end

      if unique.nil?
        raise(Error, "shard_merge requires a :unique option naming the unique tiebreaker column")
      end
      unless keys.last[0] == unique
        raise(Error, "shard_merge requires the final :order entry to be the unique tiebreaker column :#{unique}")
      end

      keys.freeze
    end

    # A lazily consumed, single pass k-way merge over per-shard cursors.
    # Includes Enumerable; all shard cursors are closed when iteration
    # finishes, when the caller stops early, or when a read raises.
    class Reader
      include Enumerable

      def initialize(sources, keys, limit)
        @sources = sources
        @keys = keys
        @limit = limit
        @closed = false
      end

      # Whether the reader has been closed, either explicitly or because
      # iteration finished or raised.
      def closed?
        @closed
      end

      # Yield rows in global order, reading pages from each shard only as
      # needed.  Closes all shard cursors before returning or raising.
      # Returns an enumerator if no block is given.  Raises Sequel::Error
      # if the reader has already been closed.
      def each
        return to_enum(:each) unless defined?(yield)
        raise(Error, "shard merge reader has been closed") if @closed

        begin
          heads = @sources.map{|source| [source, source.next_row]}
          yielded = 0
          loop do
            best = nil
            heads.each_with_index do |head, i|
              row = head[1]
              next if row.nil?
              best = i if best.nil? || compare_rows(row, heads[best][1]) < 0
            end
            break unless best

            yield heads[best][1]
            yielded += 1
            break if @limit && yielded >= @limit
            heads[best][1] = heads[best][0].next_row
          end
        ensure
          prior_error = $!
          begin
            close
          rescue
            raise unless prior_error
          end
        end
        self
      end

      # Close all shard cursors.  Idempotent.  If closing a shard cursor
      # raises, remaining cursors are still closed and the first error is
      # raised afterwards.
      def close
        return self if @closed
        @closed = true
        error = nil
        @sources.each do |source|
          begin
            source.close
          rescue => e
            error ||= e
          end
        end
        raise error if error
        self
      end

      private

      # Compare two rows according to the order keys.  Because the final
      # key is unique, this never returns 0 for distinct rows; rows from
      # earlier shards win any residual tie, keeping the merge
      # deterministic.
      def compare_rows(a, b)
        @keys.each do |column, descending|
          cmp = compare_values(a[column], b[column])
          return descending ? -cmp : cmp unless cmp == 0
        end
        0
      end

      # Compare two column values, sorting NULL values before all other
      # values.  The unique tiebreaker column should be NOT NULL so that
      # the global order matches each shard's order.
      def compare_values(a, b)
        if a.nil?
          b.nil? ? 0 : -1
        elsif b.nil?
          1
        else
          a <=> b
        end
      end
    end

    # Reads rows from a single shard's dataset in offset pages, fetching
    # the next page only after the current page has been consumed.
    class DatasetCursor
      def initialize(dataset, page_size)
        @dataset = dataset
        @page_size = page_size
        @offset = 0
        @buffer = []
        @exhausted = false
        @closed = false
      end

      # Return the next row for this shard, or nil if the shard is
      # exhausted or the cursor is closed.  Fetches the next page from
      # the shard only when the buffered page has been fully consumed.
      def next_row
        return nil if @closed
        if @buffer.empty?
          return nil if @exhausted
          page = @dataset.limit(@page_size, @offset).all
          @offset += page.size
          @exhausted = true if page.size < @page_size
          return nil if page.empty?
          @buffer = page
        end
        @buffer.shift
      end

      # Close the cursor, dropping any buffered rows.  Idempotent.
      def close
        @closed = true
        @buffer = []
        self
      end
    end
  end

  Database.register_extension(:shard_merge, ShardMerge)
end
