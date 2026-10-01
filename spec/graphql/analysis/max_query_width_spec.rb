# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Analysis::MaxQueryWidth do
  class MaxQueryWidthThing
    def initialize(id = 1, name = "n", email = "e", kind = "k")
      @id = id
      @name = name
      @email = email
      @kind = kind
    end
    attr_reader :id, :name, :email, :kind
  end

  class MaxQueryWidthRootObject
    def thing
      MaxQueryWidthSchema.resolver_calls << :thing
      MaxQueryWidthThing.new
    end
  end

  class MaxQueryWidthSchema < GraphQL::Schema
    class Thing < GraphQL::Schema::Object
      field :id, Integer
      field :name, String
      field :email, String
      field :kind, String
    end

    class QueryRoot < GraphQL::Schema::Object
      field :thing, Thing

      def thing
        MaxQueryWidthSchema.resolver_calls << :thing
        MaxQueryWidthThing.new
      end
    end

    class << self
      attr_accessor :resolver_calls
    end
    @resolver_calls = []

    query(QueryRoot)
    max_width(3)
  end

  def execute(query_string, **kwargs)
    # Pass a root object explicitly so the test also works under the
    # Execution::Next test profile (whose default root value is nil).
    MaxQueryWidthSchema.execute(query_string, root_value: MaxQueryWidthRootObject.new, **kwargs)
  end

  before { MaxQueryWidthSchema.resolver_calls = [] }

  describe "schema-level max_width" do
    it "rejects a query wider than the limit before resolving" do
      result = execute("{ thing { id name email kind } }")
      assert_equal(
        ["Query has response width of 4 on Thing at `thing`, which exceeds max width of 3"],
        result["errors"].map { |e| e["message"] }
      )
      assert_nil result["data"]
      assert_equal [], MaxQueryWidthSchema.resolver_calls
    end

    it "allows a query within the limit and resolves it" do
      result = execute("{ thing { id name email } }")
      assert_nil result["errors"]
      assert_equal [:thing], MaxQueryWidthSchema.resolver_calls
    end

    it "can be overridden per query" do
      query = GraphQL::Query.new(
        MaxQueryWidthSchema,
        "{ thing { id name email kind } }",
        root_value: MaxQueryWidthRootObject.new,
        max_width: 4
      )
      result = query.result
      assert_nil result["errors"]
      assert_equal [:thing], MaxQueryWidthSchema.resolver_calls
    end

    it "reports the root object without a path suffix" do
      result = execute("{ a: thing { id } b: thing { id } c: thing { id } d: thing { id } }")
      assert_equal(
        ["Query has response width of 4 on QueryRoot, which exceeds max width of 3"],
        result["errors"].map { |e| e["message"] }
      )
    end
  end

  describe "fragments vs aliases" do
    it "does not inflate width with repeated identical fragment spreads" do
      query_string = <<~GRAPHQL
        {
          thing {
            #{(1..10).map { |i| "...f#{i}" }.join(" ")}
          }
        }
        #{(1..10).map { |i| "fragment f#{i} on Thing { id }" }.join("\n")}
      GRAPHQL
      result = execute(query_string)
      assert_nil result["errors"]
      assert_equal [:thing], MaxQueryWidthSchema.resolver_calls
    end

    it "does inflate width with many distinct aliases" do
      aliases = (1..10).map { |i| "a#{i}: id" }.join(" ")
      result = execute("{ thing { #{aliases} } }")
      message = result["errors"][0]["message"]
      assert_includes message, "response width of 10 on Thing at `thing`"
      assert_equal [], MaxQueryWidthSchema.resolver_calls
    end
  end
end
