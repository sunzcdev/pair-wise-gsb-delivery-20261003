# frozen_string_literal: true
require "spec_helper"
require "graphql/pagination/enumerator_connection"

describe GraphQL::Pagination::EnumeratorConnection do
  # A single-pass, forward-only data source, like the result of a streamed or
  # cursor-based backend query.
  #
  # - `#each` is implemented with `#next`, and every pull is counted.
  # - `#rewind` is forbidden (the results can't be re-read).
  # - `closed?` reports whether the enumeration's resource-cleanup convention
  #   (the `ensure` block) was honored when iteration stopped or failed.
  class SinglePassSource
    include Enumerable

    class RewindNotAllowedError < StandardError; end
    class EnumerationFailedError < GraphQL::ExecutionError; end

    attr_reader :next_count

    def initialize(items, fail_at: nil)
      @items = items
      @fail_at = fail_at
      @next_count = 0
      @index = 0
      @closed = false
    end

    def closed?
      @closed
    end

    def each
      return enum_for(:each) unless block_given?
      begin
        loop do
          yield self.next
        end
      ensure
        @closed = true
      end
      self
    end

    def next
      if @index >= @items.length
        raise StopIteration
      end

      item = @items[@index]
      @index += 1
      @next_count += 1
      if @fail_at && @next_count == @fail_at
        raise EnumerationFailedError, "enumeration failed at item #{item.inspect}"
      end

      item
    end

    def rewind
      raise RewindNotAllowedError, "this single-pass source can't be rewound"
    end
  end

  NAMES = ConnectionAssertions::NAMES
  NAME_ITEMS = NAMES.map { |n| { name: n } }

  def build_schema(get_items:, max_page_size: 6, default_page_size: 4)
    items_type = Class.new(GraphQL::Schema::Object) do
      graphql_name "EnumeratorItem"
      field :name, String, null: false, hash_key: :name
    end

    query_type = Class.new(GraphQL::Schema::Object) do
      graphql_name "EnumeratorQuery"
      field :items, items_type.connection_type, null: false do
        argument :fail_at, Integer, required: false
      end

      define_method(:items) do |fail_at: nil|
        get_items.call(fail_at)
      end
    end

    Class.new(GraphQL::Schema) do
      default_max_page_size max_page_size
      default_page_size default_page_size
      cursor_encoder(ConnectionAssertions::NonceEnabledEncoder)
      connections.add(SinglePassSource, GraphQL::Pagination::EnumeratorConnection)
      query(query_type)
    end
  end

  def new_connection(source, **kwargs)
    schema = build_schema(get_items: ->(_) { source })
    context = GraphQL::Query.new(schema, "{ __typename }").context
    GraphQL::Pagination::EnumeratorConnection.new(
      source,
      context: context,
      max_page_size: 6,
      default_page_size: 4,
      **kwargs
    )
  end

  def encode_cursor(value)
    ConnectionAssertions::NonceEnabledEncoder.encode(value, nonce: true)
  end

  def decode_cursor(value)
    ConnectionAssertions::NonceEnabledEncoder.decode(value, nonce: true)
  end

  describe "forward pagination" do
    it "paginates with first and after, looking at only one extra item" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 3)

      assert_equal NAMES.first(3), conn.nodes.map { |n| n[:name] }
      assert_equal true, conn.has_next_page
      assert_equal false, conn.has_previous_page
      # 3 items + one extra peek to determine hasNextPage
      assert_equal 4, source.next_count
      assert_equal true, source.closed?

      end_cursor = conn.end_cursor
      page2 = new_connection(SinglePassSource.new(NAME_ITEMS), first: 3, after: end_cursor)
      assert_equal NAMES[3, 3], page2.nodes.map { |n| n[:name] }
      assert_equal true, page2.has_next_page
      assert_equal true, page2.has_previous_page
      # skips 3 after-cursor items, returns 3, peeks 1
      assert_equal 7, page2.items.next_count
    end

    it "doesn't peek past the end of the last page" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 6)
      assert_equal NAMES.first(6), conn.nodes.map { |n| n[:name] }
      assert_equal true, conn.has_next_page
      # 6 items + one extra peek
      assert_equal 7, source.next_count

      source2 = SinglePassSource.new(NAME_ITEMS)
      conn2 = new_connection(source2, first: 3)
      conn2.nodes
      cursor = conn2.end_cursor
      last_page = new_connection(SinglePassSource.new(NAME_ITEMS), first: 10, max_page_size: nil, default_page_size: nil, after: cursor)
      assert_equal NAMES[3..-1], last_page.nodes.map { |n| n[:name] }
      assert_equal false, last_page.has_next_page
      # skips 3 + reads the remaining 7, no peek beyond the end
      assert_equal 10, last_page.items.next_count
    end

    it "uses the clamped default/max page sizes" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source)
      assert_equal NAMES.first(4), conn.nodes.map { |n| n[:name] }
      assert_equal true, conn.has_next_page
      # default_page_size 4 + 1 peek
      assert_equal 5, source.next_count

      source2 = SinglePassSource.new(NAME_ITEMS)
      conn2 = new_connection(source2, first: 10)
      assert_equal NAMES.first(6), conn2.nodes.map { |n| n[:name] }
      assert_equal true, conn2.has_next_page
      # clamped to max_page_size 6 + 1 peek
      assert_equal 7, source2.next_count
    end

    it "handles first: 0 by peeking only at the first item" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 0)
      assert_equal [], conn.nodes
      assert_equal true, conn.has_next_page
      assert_equal false, conn.has_previous_page
      # zero nodes, but one item is examined to determine hasNextPage
      assert_equal 1, source.next_count
    end

    it "treats a blank after cursor as nil" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 3, after: "")
      assert_equal NAMES.first(3), conn.nodes.map { |n| n[:name] }
      assert_equal false, conn.has_previous_page
      assert_equal 4, source.next_count
    end

    it "serves an unbounded page by reading to the end" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, max_page_size: nil, default_page_size: nil)
      assert_equal NAMES, conn.nodes.map { |n| n[:name] }
      assert_equal false, conn.has_next_page
      assert_equal 10, source.next_count
      assert_equal true, source.closed?
    end

    it "returns an empty page for a cursor past the end" do
      all = new_connection(SinglePassSource.new(NAME_ITEMS), max_page_size: nil, default_page_size: nil)
      all.nodes
      beyond_cursor = all.cursor_for(NAME_ITEMS.last)
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 3, after: beyond_cursor)
      assert_equal [], conn.nodes
      assert_equal false, conn.has_next_page
      assert_equal true, conn.has_previous_page
      assert_nil conn.start_cursor
      assert_nil conn.end_cursor
      assert_equal 10, source.next_count
    end
  end

  describe "reusing one page" do
    it "memoizes nodes, edges, and pageInfo from a single enumeration" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 3)

      assert_same conn.nodes, conn.nodes
      assert_equal conn.nodes, conn.edges.map(&:node)
      conn.has_next_page
      conn.has_previous_page
      conn.start_cursor
      conn.end_cursor
      # Reading nodes/edges/pageInfo repeatedly never re-iterates the source.
      assert_equal 4, source.next_count
      assert_equal 3, conn.edges.size
      assert_equal conn.end_cursor, conn.edges.last.cursor
    end

    it "builds edges with stable cursors for the current page" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 2)
      edges = conn.edges
      assert_equal "1", decode_cursor(edges[0].cursor)
      assert_equal "2", decode_cursor(edges[1].cursor)
      assert_equal "1", decode_cursor(conn.start_cursor)
      assert_equal "2", decode_cursor(conn.end_cursor)
      assert_equal 3, source.next_count
    end

    it "cursors duplicated items by their offset in the list" do
      items = [:a, :b, :b, :c]
      conn = new_connection(SinglePassSource.new(items), first: 3, max_page_size: nil)
      conn.nodes
      assert_equal "1", decode_cursor(conn.cursor_for(:a))
      # The first matching position wins, like ArrayConnection.
      assert_equal "2", decode_cursor(conn.cursor_for(:b))
    end
  end

  describe "strict cursor validation" do
    it "rejects malformed cursors without reading the source" do
      ["-1", "0", "abc", "1e10", "0x1", "1.5"].each do |bad_cursor|
        source = SinglePassSource.new(NAME_ITEMS)
        encoded = encode_cursor(bad_cursor)
        err = assert_raises(GraphQL::ExecutionError) do
          new_connection(source, first: 3, after: encoded).nodes
        end
        assert_includes err.message, "Invalid cursor"
        assert_equal 0, source.next_count, "bad cursor #{bad_cursor.inspect} must not iterate"
        # No page was materialized, so the source was never touched.
        assert_equal false, source.closed?
      end
    end

    it "accepts a well-formed cursor that happens to be beyond the data" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 3, after: encode_cursor("100"))
      assert_equal [], conn.nodes
      assert_equal 10, source.next_count
      assert_equal true, source.closed?
    end
  end

  describe "backward pagination is rejected" do
    it "raises for last" do
      err = assert_raises(GraphQL::ExecutionError) do
        new_connection(SinglePassSource.new(NAME_ITEMS), last: 3).nodes
      end
      assert_includes err.message, "last"
    end

    it "raises for before" do
      err = assert_raises(GraphQL::ExecutionError) do
        new_connection(SinglePassSource.new(NAME_ITEMS), before: encode_cursor("5")).nodes
      end
      assert_includes err.message, "before"
    end

    it "raises for last combined with before even though a forward page was also requested" do
      err = assert_raises(GraphQL::ExecutionError) do
        new_connection(SinglePassSource.new(NAME_ITEMS), first: 3, last: 3, before: encode_cursor("5")).nodes
      end
      assert_includes err.message, "last"
    end
  end

  describe "resource lifecycle" do
    it "stops iteration early and honors the closing convention" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 2)
      conn.nodes
      assert_equal true, source.closed?
      assert_equal 3, source.next_count
    end

    it "honors the closing convention when enumeration raises" do
      source = SinglePassSource.new(NAME_ITEMS, fail_at: 3)
      conn = new_connection(source, first: 10)
      err = assert_raises(SinglePassSource::EnumerationFailedError) do
        conn.nodes
      end
      assert_includes err.message, "enumeration failed"
      assert_equal true, source.closed?
    end

    it "never calls rewind" do
      source = SinglePassSource.new(NAME_ITEMS)
      conn = new_connection(source, first: 3)
      conn.nodes
      conn.has_next_page
      conn.edges
      # The source raises from rewind; nothing here should have invoked it.
      assert_raises(SinglePassSource::RewindNotAllowedError) { source.rewind }
      assert_equal 4, source.next_count
    end
  end

  describe "GraphQL execution" do
    let(:schema) {
      build_schema(get_items: ->(fail_at) { SinglePassSource.new(NAME_ITEMS, fail_at: fail_at) })
    }

    it "paginates forward through a query" do
      query = <<~GRAPHQL
        query($first: Int, $after: String) {
          items(first: $first, after: $after) {
            nodes { name }
            edges { cursor node { name } }
            pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
          }
        }
      GRAPHQL

      res = schema.execute(query, variables: { "first" => 3 })
      page = res.dig("data", "items")
      assert_equal NAMES.first(3), page["nodes"].map { |n| n["name"] }
      assert_equal true, page["pageInfo"]["hasNextPage"]
      assert_equal false, page["pageInfo"]["hasPreviousPage"]
      assert_equal page["edges"].last["cursor"], page["pageInfo"]["endCursor"]
      after_cursor = page["pageInfo"]["endCursor"]

      res2 = schema.execute(query, variables: { "first" => 3, "after" => after_cursor })
      page2 = res2.dig("data", "items")
      assert_equal NAMES[3, 3], page2["nodes"].map { |n| n["name"] }
      assert_equal true, page2["pageInfo"]["hasNextPage"]
      assert_equal true, page2["pageInfo"]["hasPreviousPage"]
    end

    it "rejects invalid cursors as GraphQL errors without consuming the source" do
      query = <<~GRAPHQL
        query($after: String!) {
          items(first: 3, after: $after) { nodes { name } }
        }
      GRAPHQL
      encoded = ConnectionAssertions::NonceEnabledEncoder.encode("not-an-int")
      res = schema.execute(query, variables: { "after" => encoded })
      assert_nil res.dig("data", "items", "nodes")
      assert_includes res["errors"].first["message"], "Invalid cursor"
    end

    it "surfaces source enumeration errors and closes the source" do
      res = schema.execute("{ items(first: 10, failAt: 3) { nodes { name } } }")
      assert_includes res["errors"].first["message"], "enumeration failed"
    end

    it "closes the source when execution stops on a raised error" do
      shared_source = SinglePassSource.new(NAME_ITEMS, fail_at: 3)
      local_schema = build_schema(get_items: ->(_) { shared_source })
      res = local_schema.execute("{ items(first: 10) { nodes { name } } }")
      assert res["errors"].any?
      assert_includes res["errors"].first["message"], "enumeration failed"
      assert_equal true, shared_source.closed?
    end

    it "automatically wraps plain Enumerators" do
      enum_schema = build_schema(get_items: ->(_) { NAME_ITEMS.each })
      res = enum_schema.execute("{ items(first: 2) { nodes { name } pageInfo { hasNextPage } } }")
      page = res.dig("data", "items")
      assert_equal NAMES.first(2), page["nodes"].map { |n| n["name"] }
      assert_equal true, page["pageInfo"]["hasNextPage"]
    end
  end
end
