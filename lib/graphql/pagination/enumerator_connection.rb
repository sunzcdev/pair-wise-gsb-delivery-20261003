# frozen_string_literal: true
require "graphql/pagination/connection"

module GraphQL
  module Pagination
    # A forward-only connection for single-pass enumerables (such as `Enumerator`s
    # returned by streaming or batched exports) which can be traversed with `#next`
    # exactly once.
    #
    # Unlike {ArrayConnection}, the underlying items are never converted to an
    # array: only `first + 1` items are pulled from the enumerator (one extra is
    # peeked at to determine {#has_next_page}), so paginating a huge or unbounded
    # result set never loads it all into memory.
    #
    # Only forward pagination (`first` / `after`) is supported. Backward
    # pagination arguments (`last` / `before`) cannot be served without walking
    # (or buffering) the whole collection, so they are rejected up front, before
    # the enumerator is touched.
    class EnumeratorConnection < Pagination::Connection
      # Raised when a backward-pagination argument (`last` or `before`) is given.
      class BackwardPaginationUnsupportedError < GraphQL::ExecutionError
        MESSAGE = "EnumeratorConnection only supports forward pagination; remove `last` and `before` and use `first` and `after` instead."
        def initialize(message = MESSAGE)
          super
        end
      end

      def nodes
        load_page
        @nodes
      end

      def has_next_page
        load_page
        @has_next_page
      end

      def has_previous_page
        load_page
        @has_previous_page
      end

      def cursor_for(item)
        load_page
        idx = @node_index_by_identity[item.object_id]
        if idx.nil?
          raise PaginationImplementationMissingError, "Couldn't find a cursor for #{item.inspect}: it isn't part of the current page and the wrapped enumerator can't be rewound to locate it."
        end
        encode((idx + 1).to_s)
      end

      private

      # Validate the pagination arguments and pull a single page out of the
      # enumerator _once_. Subsequent reads of {#nodes}, {#edges} or
      # {#page_info} reuse the cached page — the enumerator is never iterated a
      # second time.
      def load_page
        @nodes ||= begin
          if last
            raise BackwardPaginationUnsupportedError
          end

          if before
            raise BackwardPaginationUnsupportedError
          end

          unless items.respond_to?(:next)
            raise PaginationImplementationMissingError, "#{self.class} can only paginate single-pass enumerables that respond to `#next`, but got #{items.class.inspect}"
          end

          # Strictly validate the cursor _before_ pulling anything from the
          # enumerator, so a bogus cursor can never trigger a long, useless read.
          after_index = after ? index_from_cursor(after) : 0
          window = first
          @has_previous_page = after_index > 0

          nodes = []
          @node_index_by_identity = {}
          position = 0
          @has_next_page = false

          begin
            # Skip the items preceding the `after` cursor (forward only).
            while position < after_index
              item = items.next
              position += 1
            end

            # Take at most `window` items for the page (plus one below to detect
            # a next page). When `window` is nil, the whole remainder is read.
            while window.nil? || nodes.size < window
              item = items.next
              @node_index_by_identity[item.object_id] = position
              nodes << item
              position += 1
            end

            # Peek at one more item to determine whether another page exists —
            # never iterate to the end just to answer this.
            begin
              items.next
              @has_next_page = true
            rescue StopIteration
              @has_next_page = false
            end
          rescue StopIteration
            # The enumerator ended mid-page: there is nothing after this page.
            @has_next_page = false
          ensure
            # Release the underlying resource as soon as the page is materialized.
            # This also runs when the enumerator ends early or raises mid-way,
            # so the resource's close contract is always honored.
            if items.respond_to?(:close)
              close_items
            end
          end

          nodes
        end
      end

      # @param cursor [String] client-provided cursor
      # @return [Integer] the 0-based number of items to skip
      def index_from_cursor(cursor)
        index = Integer(decode(cursor), 10, exception: false)
        if index.nil? || index <= 0
          raise GraphQL::ExecutionError, "Invalid cursor: #{cursor.inspect}"
        end
        index
      end

      def close_items
        items.close
      rescue StandardError
        # Closing is best-effort cleanup; never mask the page result
        # (or an error raised while building it).
        nil
      end
    end
  end
end

