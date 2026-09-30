# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Pagination::EnumeratorConnection do
  # A single-pass data source like a streaming/batched export backend:
  # - records how many times `#next` was called (so tests can prove the
  #   enumerator was never walked to the end),
  # - refuses `#rewind` (going backwards is impossible),
  # - honors a `#close` resource-cleanup contract.
  class SinglePassSource
    attr_reader :next_count, :close_count, :rewind_count

    def initialize(items, raise_after: nil, close_raises: false)
      @items = items
      @index = 0
      @next_count = 0
      @close_count = 0
      @rewind_count = 0
      @raise_after = raise_after
      @close_raises = close_raises
    end

    def next
      raise IOError, "can't read from a closed source" if closed?
      raise StopIteration, "iteration reached an end" if @index >= @items.size
      value = @items[@index]
      @index += 1
      @next_count += 1
      raise "boom: export failed mid-stream" if @raise_after && @next_count > @raise_after
      value
    end

    def rewind
      @rewind_count += 1
      raise "this source can't be rewound"
    end

    def close
      @close_count += 1
      raise "close failed" if @close_raises
      @closed = true
    end

    def closed?
      @closed == true
    end
  end

  def build_schema
    PaginationTestSchema
  end

  class PaginationItem < GraphQL::Schema::Object
    graphql_name "EnumeratorItem"
    field :name, String, null: false
  end

  class PaginationQuery < GraphQL::Schema::Object
    graphql_name "EnumeratorQuery"
    # Connection fields automatically expose `first`, `after`, `last` and `before`.
    field :items, PaginationItem.connection_type, null: false, resolve_static: true

    def self.items(context, first: nil, after: nil, last: nil, before: nil)
      source = SinglePassSource.new(ConnectionAssertions::NAMES.map { |n| { name: n } })
      GraphQL::Pagination::EnumeratorConnection.new(
        source,
        context: context,
        first: first,
        after: after,
        last: last,
        before: before
      )
    end

    def items(**kwargs)
      self.class.items(context, **kwargs)
    end
  end

  class PaginationTestSchema < GraphQL::Schema
    default_max_page_size ConnectionAssertions::MAX_PAGE_SIZE
    default_page_size ConnectionAssertions::DEFAULT_PAGE_SIZE
    query(PaginationQuery)
  end

  class UnboundedTestSchema < GraphQL::Schema
    default_max_page_size nil
    default_page_size nil
  end

  let(:schema) { build_schema }
  let(:names) { ConnectionAssertions::NAMES }

  def context_for(schema)
    GraphQL::Query.new(schema, "{ __typename }").context
  end

  def connection_for(source, schema, **kwargs)
    GraphQL::Pagination::EnumeratorConnection.new(source, context: context_for(schema), **kwargs)
  end

  def cursor(schema, value)
    schema.cursor_encoder.encode(value.to_s, nonce: true)
  end

  describe "forward pagination" do
    it "paginates with first and after" do
      source = SinglePassSource.new((1..10).to_a)
      conn = connection_for(source, schema, first: 3)

      assert_equal [1, 2, 3], conn.nodes
      assert_equal true, conn.has_next_page
      assert_equal false, conn.has_previous_page

      after_cursor = conn.end_cursor

      source2 = SinglePassSource.new((1..10).to_a)
      conn2 = connection_for(source2, schema, first: 3, after: after_cursor)
      assert_equal [4, 5, 6], conn2.nodes
      assert_equal true, conn2.has_next_page
      assert_equal true, conn2.has_previous_page
      assert_equal cursor(schema, 4), conn2.start_cursor
      assert_equal cursor(schema, 6), conn2.end_cursor

      source3 = SinglePassSource.new((1..10).to_a)
      conn3 = connection_for(source3, schema, first: 30, after: conn2.end_cursor)
      assert_equal [7, 8, 9, 10], conn3.nodes
      assert_equal false, conn3.has_next_page
      assert_equal true, conn3.has_previous_page
    end

    it "applies default_page_size and max_page_size" do
      source = SinglePassSource.new((1..10).to_a)
      conn = connection_for(source, schema)
      assert_equal [1, 2, 3, 4], conn.nodes
      assert_equal true, conn.has_next_page

      source2 = SinglePassSource.new((1..10).to_a)
      conn2 = connection_for(source2, schema, first: 100)
      assert_equal [1, 2, 3, 4, 5, 6], conn2.nodes
      assert_equal true, conn2.has_next_page
    end

    it "returns an empty page after the last item" do
      source = SinglePassSource.new((1..10).to_a)
      conn = connection_for(source, schema, first: 3, after: cursor(schema, 10))
      assert_equal [], conn.nodes
      assert_equal false, conn.has_next_page
      assert_equal true, conn.has_previous_page
      assert_nil conn.start_cursor
      assert_nil conn.end_cursor
    end

    it "handles an empty source" do
      source = SinglePassSource.new([])
      conn = connection_for(source, schema, first: 3)
      assert_equal [], conn.nodes
      assert_equal false, conn.has_next_page
      assert_equal false, conn.has_previous_page
    end

    it "handles first: 0" do
      source = SinglePassSource.new((1..10).to_a)
      conn = connection_for(source, schema, first: 0)
      assert_equal [], conn.nodes
      assert_equal true, conn.has_next_page
      assert_equal false, conn.has_previous_page
    end

    it "reads the entire remainder when unbounded" do
      source = SinglePassSource.new((1..10).to_a)
      conn = GraphQL::Pagination::EnumeratorConnection.new(source, context: context_for(UnboundedTestSchema))
      assert_equal (1..10).to_a, conn.nodes
      assert_equal false, conn.has_next_page
      assert_equal 10, source.next_count
    end

    it "exposes edges with cursors" do
      source = SinglePassSource.new((1..3).to_a)
      conn = connection_for(source, schema, first: 3)
      edges = conn.edges
      assert_equal [1, 2, 3], edges.map(&:node)
      assert_equal [cursor(schema, 1), cursor(schema, 2), cursor(schema, 3)], edges.map(&:cursor)
    end
  end

  describe "single-pass traversal" do
    it "only pulls first + 1 items (plus skipped items) — never to the end" do
      source = SinglePassSource.new((1..100).to_a)
      conn = connection_for(source, schema, first: 3)
      conn.nodes
      # 3 items for the page + 1 peeked item to detect a next page
      assert_equal 4, source.next_count
      assert_equal 0, source.rewind_count

      source2 = SinglePassSource.new((1..100).to_a)
      conn2 = connection_for(source2, schema, first: 5, after: cursor(schema, 20))
      conn2.nodes
      # 20 skipped + 5 page items + 1 peek
      assert_equal 26, source2.next_count
      assert_equal 0, source2.rewind_count

      # A short final page: after skipping 20 there are only 2 left, so the page
      # fill consumes the whole source (20 skipped + 2 items) and no separate
      # peek is needed to know there is no next page.
      source3 = SinglePassSource.new((1..22).to_a)
      conn3 = connection_for(source3, schema, first: 5, after: cursor(schema, 20))
      assert_equal [21, 22], conn3.nodes
      assert_equal 22, source3.next_count
      assert_equal false, conn3.has_next_page
      assert_equal 22, source3.next_count
    end

    it "reuses the same page when nodes, edges and pageInfo are read multiple times" do
      source = SinglePassSource.new((1..10).to_a)
      conn = connection_for(source, schema, first: 3)

      3.times do
        assert_equal [1, 2, 3], conn.nodes
        assert_equal 3, conn.edges.size
        assert_equal true, conn.page_info.has_next_page
        assert_equal false, conn.page_info.has_previous_page
        assert_equal cursor(schema, 1), conn.page_info.start_cursor
        assert_equal cursor(schema, 3), conn.page_info.end_cursor
      end
      assert_equal 4, source.next_count
      assert_equal 1, source.close_count
    end

    it "validates bad cursors without pulling a single item" do
      ["-1", "0", "abc", "1e10", ""].each do |bad_cursor|
        source = SinglePassSource.new((1..100).to_a)
        encoded = if bad_cursor.empty?
          ""
        else
          schema.cursor_encoder.encode(bad_cursor, nonce: true)
        end
        conn = connection_for(source, schema, first: 3, after: encoded)
        if bad_cursor.empty?
          # An empty cursor is treated like nil
          assert_equal [1, 2, 3], conn.nodes
        else
          err = assert_raises(GraphQL::ExecutionError) { conn.nodes }
          assert_includes err.message, "Invalid cursor"
        end
        assert_equal 0, source.next_count unless bad_cursor.empty?
      end
    end

    it "doesn't accept items without a #next method" do
      conn = connection_for([1, 2, 3], schema, first: 2)
      err = assert_raises(GraphQL::Pagination::Connection::PaginationImplementationMissingError) do
        conn.nodes
      end
      assert_includes err.message, "`#next`"
    end
  end

  describe "rejecting backward pagination" do
    it "rejects last before touching the source" do
      source = SinglePassSource.new((1..10).to_a)
      conn = connection_for(source, schema, last: 2)
      err = assert_raises(GraphQL::Pagination::EnumeratorConnection::BackwardPaginationUnsupportedError) do
        conn.nodes
      end
      assert_includes err.message, "forward pagination"
      assert_equal 0, source.next_count

      # last combined with first is also rejected
      source2 = SinglePassSource.new((1..10).to_a)
      conn2 = connection_for(source2, schema, first: 2, last: 2)
      assert_raises(GraphQL::Pagination::EnumeratorConnection::BackwardPaginationUnsupportedError) do
        conn2.nodes
      end
      assert_equal 0, source2.next_count
    end

    it "rejects before before touching the source" do
      source = SinglePassSource.new((1..10).to_a)
      conn = connection_for(source, schema, first: 2, before: cursor(schema, 5))
      assert_raises(GraphQL::Pagination::EnumeratorConnection::BackwardPaginationUnsupportedError) do
        conn.nodes
      end
      assert_equal 0, source.next_count
    end
  end

  describe "resource closing" do
    it "closes the source after materializing a full page early" do
      source = SinglePassSource.new((1..100).to_a)
      conn = connection_for(source, schema, first: 3)
      conn.nodes
      assert_equal 1, source.close_count
      assert source.closed?
      # Reading the page again must not touch the source again.
      conn.has_next_page
      conn.edges
      assert_equal 1, source.close_count
    end

    it "closes the source when the enumerator ends early" do
      source = SinglePassSource.new((1..2).to_a)
      conn = connection_for(source, schema, first: 10)
      assert_equal [1, 2], conn.nodes
      assert_equal false, conn.has_next_page
      assert_equal 1, source.close_count
    end

    it "closes the source when an error is raised while skipping items" do
      source = SinglePassSource.new((1..10).to_a, raise_after: 2)
      conn = connection_for(source, schema, first: 3, after: cursor(schema, 5))
      err = assert_raises(RuntimeError) { conn.nodes }
      assert_equal "boom: export failed mid-stream", err.message
      assert source.closed?
    end

    it "closes the source when an error is raised while reading page items" do
      source = SinglePassSource.new((1..10).to_a, raise_after: 2)
      conn = connection_for(source, schema, first: 3)
      assert_raises(RuntimeError) { conn.nodes }
      assert source.closed?
    end

    it "never calls rewind" do
      source = SinglePassSource.new((1..10).to_a)
      conn = connection_for(source, schema, first: 3)
      conn.nodes
      conn.has_next_page
      assert_equal 0, source.rewind_count
    end

    it "doesn't mask page results or errors when closing fails" do
      source = SinglePassSource.new((1..10).to_a, close_raises: true)
      conn = connection_for(source, schema, first: 3)
      assert_equal [1, 2, 3], conn.nodes
      assert_equal 1, source.close_count

      source2 = SinglePassSource.new((1..10).to_a, raise_after: 2, close_raises: true)
      conn2 = connection_for(source2, schema, first: 3)
      err = assert_raises(RuntimeError) { conn2.nodes }
      assert_equal "boom: export failed mid-stream", err.message
    end
  end

  describe "GraphQL execution" do
    class AutoEnumQuery < GraphQL::Schema::Object
      graphql_name "AutoEnumQuery"
      field :stream, GraphQL::Types::String.connection_type, null: false, resolve_static: true

      def self.stream(context)
        %w[a b c d e].each
      end

      def stream
        self.class.stream(context)
      end
    end

    class AutoEnumSchema < GraphQL::Schema
      default_max_page_size 2
      query(AutoEnumQuery)
    end

    def execute(query, variables = {})
      schema.execute(query, variables: variables)
    end

    let(:query_str) do
      <<~GRAPHQL
        query($first: Int, $after: String, $last: Int, $before: String) {
          items(first: $first, after: $after, last: $last, before: $before) {
            nodes { name }
            edges { node { name } cursor }
            pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
          }
        }
      GRAPHQL
    end

    it "paginates end to end through the schema" do
      res = execute(query_str, { first: 3 })
      page = res.dig("data", "items")
      assert_equal names[0, 3], page["nodes"].map { |n| n["name"] }
      assert_equal true, page["pageInfo"]["hasNextPage"]
      assert_equal false, page["pageInfo"]["hasPreviousPage"]
      after_cursor = page["pageInfo"]["endCursor"]

      res2 = execute(query_str, { first: 3, after: after_cursor })
      page2 = res2.dig("data", "items")
      assert_equal names[3, 3], page2["nodes"].map { |n| n["name"] }
      assert_equal true, page2["pageInfo"]["hasNextPage"]
      assert_equal true, page2["pageInfo"]["hasPreviousPage"]
      assert_equal page2["nodes"].map { |n| n["name"] }, page2["edges"].map { |e| e["node"]["name"] }

      res3 = execute(query_str, { first: 30, after: page2["pageInfo"]["endCursor"] })
      page3 = res3.dig("data", "items")
      assert_equal names[6, 4], page3["nodes"].map { |n| n["name"] }
      assert_equal false, page3["pageInfo"]["hasNextPage"]
      assert_equal true, page3["pageInfo"]["hasPreviousPage"]
    end

    it "rejects last and before with an error" do
      res = execute(query_str, { last: 3 })
      assert_nil res.dig("data", "items")
      assert_includes res["errors"].first["message"], "forward pagination"

      res2 = execute(query_str, { first: 3, before: cursor(schema, 2) })
      assert_nil res2.dig("data", "items")
      assert_includes res2["errors"].first["message"], "forward pagination"
    end

    it "rejects malformed cursors without exhausting the source" do
      ["-1", "0", "abc", "1e10"].each do |bad|
        encoded = schema.cursor_encoder.encode(bad, nonce: true)
        res = execute(query_str, { first: 3, after: encoded })
        assert_nil res.dig("data", "items", "nodes")
        assert_includes res["errors"].first["message"], "Invalid cursor"
      end
    end

    it "automatically wraps Enumerator return values" do
      res = AutoEnumSchema.execute("{ stream(first: 2) { nodes pageInfo { hasNextPage hasPreviousPage } } }")
      assert_equal %w[a b], res.dig("data", "stream", "nodes")
      assert_equal true, res.dig("data", "stream", "pageInfo", "hasNextPage")
      assert_equal false, res.dig("data", "stream", "pageInfo", "hasPreviousPage")
    end
  end
end
