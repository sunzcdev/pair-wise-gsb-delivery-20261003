# frozen_string_literal: true
require "graphql/pagination/connection"

module GraphQL
  module Pagination
    # A connection for single-pass, forward-only enumerables -- like the result
    # of a streamed or cursor-based database query that can only be read once.
    #
    # Unlike {ArrayConnection}, this connection never converts `items` into an
    # array: it only reads as many items as required to serve the requested
    # page (plus one extra item to determine {has_next_page}), then stops
    # iterating. As a result, only forward pagination (`first` / `after`) is
    # supported; providing `last` or `before` raises an error because serving
    # those arguments would require either rewinding the enumerator or reading
    # it all the way to the end.
    #
    # The wrapped object only has to respond to `#each` (i.e. it may be an
    # Enumerable). When the iteration is stopped early, or when it raises an
    # error, the enumerable's own resource-cleanup convention (the `ensure`
    # block / block-closing semantics of `#each`) is honored.
    class EnumeratorConnection < Pagination::Connection
      # @return [Array<Object>] A single page of {items}, constrained by `first`/`after`
      def nodes
        load_page
        @nodes
      end

      # @return [Boolean] True when an `after` cursor was provided, i.e. there are items before this page.
      def has_previous_page
        load_page
        @has_previous_page
      end

      # @return [Boolean] True if at least one item followed the items in this page.
      def has_next_page
        load_page
        @has_next_page
      end

      def cursor_for(item)
        load_page
        node_offset = @nodes.index(item)
        if node_offset.nil?
          raise PaginationImplementationMissingError, "Couldn't find a cursor for #{item.inspect}, it isn't part of the current page"
        end

        encode((@after_index + node_offset + 1).to_s)
      end

      private

      def index_from_cursor(cursor)
        index = Integer(decode(cursor), 10, exception: false)
        if index.nil? || index <= 0
          # Raise before iterating so that a bad cursor never triggers a
          # pointless read of the underlying enumeration.
          raise GraphQL::ExecutionError, "Invalid cursor: #{cursor.inspect}"
        end

        index
      end

      # Validate arguments and pull one page out of {items}, exactly once.
      # Subsequent access to {nodes}/{edges}/page info reuses this page.
      def load_page
        @nodes ||= begin
          if last
            raise GraphQL::ExecutionError, "#{self.class.name} can't be paginated backwards, `last:` isn't supported (use `first:`)"
          end

          if before
            raise GraphQL::ExecutionError, "#{self.class.name} can't be paginated backwards, `before:` isn't supported (use `after:`)"
          end

          # Strictly validate the cursor before touching the enumerable.
          after_index = after ? index_from_cursor(after) : 0

          nodes = []
          has_next_page = false
          global_index = 0

          items.each do |item|
            global_index += 1
            # Cursors are 1-based, so the item at `after_index` itself is skipped.
            # (Skipped items must still be consumed since the enumeration is
            # single-pass, but nothing else is done with them.)
            next if global_index <= after_index

            if first && nodes.length >= first
              # Once the page is full, this is the one extra item examined to
              # answer hasNextPage. Stop immediately -- never drain the rest.
              has_next_page = true
              break
            end

            nodes << item
          end

          @after_index = after_index
          @has_next_page = has_next_page
          @has_previous_page = !after.nil?
          nodes
        end
      end
    end
  end
end
