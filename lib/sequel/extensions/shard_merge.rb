# frozen-string-literal: true
#
# The shard_merge extension adds Dataset#shard_merge, an explicit entry point
# for merging rows from multiple shards that all use the same ordering, such
# as a read-only "top N" query that spans shards:
#
#   DB.extension :shard_merge
#   merge = DB[:items].where(active: true).
#     shard_merge([:shard1, :shard2, :shard3], order: [:created_at, :id], limit: 10)
#   merge.each{|row| ... }
#
# shard_merge returns a lazy, single-pass Enumerable (Sequel::ShardMerge::Merger).
# Rows are pulled from each shard in pages (keyset pagination, not OFFSET), and
# a k-way merge yields rows in global order.  At most one page per shard is held
# in memory at any time; shards are never read into memory in full.
#
# The :order option is required, and its final entry must be a unique column
# (such as the primary key) so that the ordering is total and ties are broken
# deterministically.  Without a unique final tiebreaker, keyset pagination can
# skip or duplicate rows.  Order entries must be column names, or
# Sequel.asc/Sequel.desc of column names:
#
#   shard_merge(shards, order: [Sequel.desc(:created_at), :id])
#
# Rows with equal sort keys on different shards are all returned; ties are
# broken deterministically by shard position in the shard list.
#
# Each page read is an independent query on its own shard.  Transaction
# semantics are limited to each shard itself: no cross-shard transaction is
# used and no cross-shard snapshot is taken, so rows inserted or deleted on a
# shard while the merge is running may or may not be reflected in the results.
#
# Use the block form of +each+ (or Enumerable methods such as +first+/+take+)
# so that all shard sources are deterministically closed when the block exits,
# whether it exits normally, via +break+, or via an exception.  If a shard read
# raises an error, the error is propagated to the caller and all shard sources
# are closed.  A Merger is single-pass; attempting to read from it after it has
# been closed raises Sequel::Error.
#
# Related module: Sequel::ShardMerge

#
module Sequel
  module ShardMerge
    # Raised for invalid shard_merge arguments and reads from closed sources.
    class Error < Sequel::Error
    end

    # Normalize an order array into an array of [column, descending] pairs,
    # validating that the order is usable for a deterministic merge.  The
    # final column is the tiebreaker and must be unique (this cannot be
    # checked here; it is a requirement on the caller).
    def self.normalize_order(order)
      if order.nil? || order.empty?
        raise Error, "shard_merge requires an explicit :order whose final entry is a unique tiebreaker column (e.g. the primary key)"
      end

      seen = {}
      normalized = order.map do |entry|
        column, descending =
          case entry
          when Symbol
            [entry, false]
          when Sequel::SQL::OrderedExpression
            unless entry.expression.is_a?(Symbol)
              raise Error, "shard_merge order entries must be columns (Symbol or Sequel.asc/Sequel.desc of a Symbol): #{entry.inspect}"
            end
            if entry.nulls
              raise Error, "shard_merge does not support explicit NULLS FIRST/LAST ordering: #{entry.inspect}"
            end
            [entry.expression, entry.descending]
          else
            raise Error, "shard_merge order entries must be columns (Symbol or Sequel.asc/Sequel.desc of a Symbol): #{entry.inspect}"
          end

        if seen[column]
          raise Error, "shard_merge order contains duplicate column: #{column}"
        end
        seen[column] = true

        [column, descending]
      end

      normalized.freeze
    end

    # Compare two rows according to the normalized order.  Returns a negative
    # number if +a+ sorts before +b+, 0 if they are equal on all order
    # columns, and a positive number if +a+ sorts after +b+.  NULL values sort
    # before all other values; sort columns should generally be NOT NULL.
    def self.compare_rows(order, a, b)
      order.each do |column, descending|
        av = a[column]
        bv = b[column]
        cmp =
          if av.nil?
            bv.nil? ? 0 : -1
          elsif bv.nil?
            1
          else
            av <=> bv
          end
        next if cmp == 0
        return descending ? -cmp : cmp
      end
      0
    end

    # A paged source of rows from a single shard.  Uses keyset pagination:
    # each page after the first filters on rows sorting after the last row of
    # the previous page, so no OFFSET is used and no server-side cursor or
    # transaction is held open between pages.
    class Source
      # The shard/server name this source reads from.
      attr_reader :shard

      def initialize(shard, dataset, order, page_size)
        @shard = shard
        @dataset = dataset
        @order = order
        @page_size = page_size
        @last_row = nil
        @exhausted = false
        @closed = false
      end

      # Whether the source has been closed.
      def closed?
        @closed
      end

      # Whether the shard has no more rows to return.
      def exhausted?
        @exhausted
      end

      # Return the next page of rows as an array (in sort order), or an empty
      # array if the shard is exhausted.  Raises Sequel::Error if the source
      # has been closed.
      def next_page
        raise Error, "cannot read from closed shard_merge source for shard #{@shard.inspect}" if @closed
        return [] if @exhausted

        ds = @dataset.limit(@page_size)
        ds = ds.where(keyset_condition(@last_row)) if @last_row
        rows = ds.all

        @exhausted = true if rows.length < @page_size
        @last_row = rows.last unless rows.empty?
        rows
      end

      # Close the source.  After close, next_page raises Sequel::Error.
      # Since keyset pagination holds no server-side resources, this only
      # releases the local state, but it is always called when a merge
      # finishes so callers have deterministic cleanup semantics.
      def close
        @closed = true
        @dataset = nil
        @last_row = nil
        nil
      end

      private

      # Condition matching rows that sort strictly after +row+, built as an
      # OR of ANDs over the order columns: (c1 > v1) OR (c1 = v1 AND c2 > v2)
      # OR ..., using < instead of > for descending columns.
      def keyset_condition(row)
        condition = nil
        equality = nil
        @order.each do |column, descending|
          value = row[column]
          comparison = descending ? Sequel[column] < value : Sequel[column] > value
          branch = equality ? Sequel.&(equality, comparison) : comparison
          condition = condition ? Sequel.|(condition, branch) : branch
          equality = equality ? Sequel.&(equality, column=>value) : {column=>value}
        end
        condition
      end
    end

    # A lazy, single-pass k-way merge over paged shard sources.  Includes
    # Enumerable; each yields rows in global sort order.  At most one page
    # per source is buffered in memory at a time.
    #
    # Sources must respond to +next_page+ (returning an array of rows in sort
    # order, empty when the source is exhausted) and +close+.
    class Merger
      include Enumerable

      def initialize(sources, order, limit=nil)
        @sources = sources
        @order = order
        @limit = limit
      end

      # Yield rows in global sort order, at most +limit+ rows if a limit was
      # given.  All sources are closed when iteration finishes for any reason
      # (normal completion, +break+, or an exception raised by a source or by
      # the block).  Returns an Enumerator if no block is given; prefer the
      # block form so sources are closed deterministically.
      def each
        return to_enum(:each) unless block_given?

        sources = @sources
        buffers = sources.map { [] }
        exhausted = sources.map { false }
        emitted = 0

        begin
          loop do
            break if @limit && emitted >= @limit

            best = nil
            sources.each_with_index do |source, i|
              if buffers[i].empty? && !exhausted[i]
                page = source.next_page
                if page.empty?
                  exhausted[i] = true
                else
                  buffers[i] = page
                end
              end
              next if buffers[i].empty?

              if best.nil? || ShardMerge.compare_rows(@order, buffers[i][0], buffers[best][0]) < 0
                best = i
              end
            end
            break unless best

            yield buffers[best].shift
            emitted += 1
          end
        ensure
          sources.each(&:close)
        end

        self
      end
    end

    # Dataset methods added by the shard_merge extension.
    module DatasetMethods
      # Return a lazy, single-pass Sequel::ShardMerge::Merger merging rows
      # from the given shards/servers, all using the same order.  Options:
      #
      # :order :: Required array of order entries.  Each entry must be a
      #           column Symbol or Sequel.asc/Sequel.desc of a column Symbol.
      #           The final entry must be a unique column (e.g. the primary
      #           key) so the ordering is total and deterministic.
      # :limit :: Maximum number of rows to yield across all shards (nil for
      #           no limit).
      # :page_size :: Number of rows to read per shard per query (default
      #               1000).  At most one page per shard is held in memory.
      #
      # Example:
      #
      #   DB[:items].shard_merge([:s1, :s2], order: [:created_at, :id], limit: 10).
      #     each{|row| ... }
      def shard_merge(servers, order:, limit: nil, page_size: 1000)
        normalized = ShardMerge.normalize_order(order)
        page_size = limit if limit && limit < page_size
        sql_order = normalized.map{|column, descending| descending ? Sequel.desc(column) : Sequel.asc(column)}
        sources = servers.map do |server|
          ShardMerge::Source.new(server, server(server).order(*sql_order), normalized, page_size)
        end
        ShardMerge::Merger.new(sources, normalized, limit)
      end
    end
  end

  Dataset.register_extension(:shard_merge, ShardMerge::DatasetMethods)
end
