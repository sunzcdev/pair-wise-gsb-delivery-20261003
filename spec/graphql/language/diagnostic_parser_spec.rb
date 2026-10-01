# frozen_string_literal: true
require "spec_helper"

describe "diagnostic parse mode" do
  describe "GraphQL.parse_with_diagnostics" do
    it "parses a valid document with no errors" do
      doc = GraphQL.parse_with_diagnostics("query A { a }\nquery B { b }")
      assert_equal 2, doc.definitions.size
      assert_empty doc.parse_errors
      assert doc.valid?
    end

    it "does not change the fast-fail behavior of GraphQL.parse" do
      assert_raises(GraphQL::ParseError) do
        GraphQL.parse("query A { a { b }")
      end
    end

    it "reports two unrelated errors in a single document" do
      doc = GraphQL.parse_with_diagnostics(<<~GRAPHQL)
        query A { a(x: ) { y } }
        query B { b { c
      GRAPHQL

      assert_equal 2, doc.parse_errors.size
      refute doc.valid?
      assert_equal 2, doc.definitions.size
      assert_kind_of GraphQL::Language::Nodes::OperationDefinition, doc.definitions.first
      assert_kind_of GraphQL::Language::Nodes::OperationDefinition, doc.definitions.last
    end

    it "recovers all following definitions after a dropped closing brace" do
      doc = GraphQL.parse_with_diagnostics(<<~GRAPHQL)
        query A { a { x
        query B($id: ID!) { b(id: $id) }
      GRAPHQL

      assert_equal 1, doc.parse_errors.size
      assert_equal 2, doc.definitions.size
      assert_equal %w[A B], doc.definitions.map(&:name)
    end

    it "keeps the parsed structure when the document is truncated" do
      doc = GraphQL.parse_with_diagnostics("{ a { b { c")

      assert_equal 1, doc.parse_errors.size
      assert_equal [1, 11], [doc.parse_errors.first.line, doc.parse_errors.first.col]
      refute doc.valid?

      op = doc.definitions.first
      assert_kind_of GraphQL::Language::Nodes::OperationDefinition, op
      assert_equal ["a"], op.selections.map(&:name)
      assert_equal ["b"], op.selections.first.selections.map(&:name)
    end

    it "doesn't use brackets inside ordinary strings as recovery points" do
      doc = GraphQL.parse_with_diagnostics('{ a(note: "ignore } these ( braces") { ok } }')

      assert_empty doc.parse_errors
      assert doc.valid?
    end

    it "doesn't use brackets inside block strings as recovery points" do
      src = <<~GRAPHQL
        query A {
          q(note: """
          block string } with { braces ( inside
          """) { ok }
        }
        query B { good }
      GRAPHQL
      doc = GraphQL.parse_with_diagnostics(src)

      assert_empty doc.parse_errors
      assert doc.valid?
      assert_equal 2, doc.definitions.size
    end

    it "handles multiline input containing Chinese" do
      src = <<~GRAPHQL
        query 第一个查询 {
          用户(编号: 123) { 姓名 }
        }
        query 第二个查询 {
          文章 { 标题 }
        }
      GRAPHQL
      doc = GraphQL.parse_with_diagnostics(src)

      assert_empty doc.parse_errors
      assert doc.valid?
      assert_equal "用户", doc.definitions.first.selections.first.name
      assert_equal "标题", doc.definitions.last.selections.first.selections.first.name
    end

    it "reports independent errors in multiline Chinese input" do
      src = <<~GRAPHQL
        query 第一个查询 {
          用户(编号: ) { 姓名 }
        }
        query 第二个查询 {
          文章 { 标题
        }
      GRAPHQL
      doc = GraphQL.parse_with_diagnostics(src)

      assert_equal 2, doc.parse_errors.size
      assert_equal 2, doc.definitions.size
    end

    it "doesn't turn a single missing bracket into dozens of duplicate diagnostics" do
      doc = GraphQL.parse_with_diagnostics("{ a { b { c } d } { e }")

      assert_operator doc.parse_errors.size, :<=, 3
      refute doc.valid?
    end

    it "reports the same error position only once" do
      positions = doc_with_error("{ a { b { c").parse_errors.map { |e| [e.line, e.col] }
      assert_equal positions.uniq, positions
    end

    it "reports an unterminated string as a lex error" do
      # A newline is illegal inside an ordinary string, so an unterminated
      # quote consumes the remainder of the source; the lex error is recorded.
      doc = GraphQL.parse_with_diagnostics("query A { a(x: \"oops) }")

      refute doc.valid?
      assert_equal 1, doc.parse_errors.size
      assert_includes doc.parse_errors.first.message, "malformed"
    end

    it "recovers independently inside argument lists" do
      doc = GraphQL.parse_with_diagnostics("{ a(x: 1, y: ?, z: 2) { ok } }")

      refute doc.valid?
      field = doc.definitions.first.selections.first
      assert_includes field.arguments.map(&:name), "z"
    end

    it "recovers independently inside list and input object values" do
      doc = GraphQL.parse_with_diagnostics("{ a(list: [1, ?, 3], obj: {x: 1, y: }) { ok } }")

      refute doc.valid?
      assert_kind_of GraphQL::Language::Nodes::Field, doc.definitions.first.selections.first
    end

    it "marks unrecoverable definitions explicitly in the AST" do
      doc = GraphQL.parse_with_diagnostics("query A { a }\nquery B 123 { b }")

      assert_kind_of GraphQL::Language::Nodes::UnrecoverableDefinition, doc.definitions.last
      refute doc.valid?
    end

    it "marks unrecoverable selections explicitly in the AST" do
      doc = GraphQL.parse_with_diagnostics("{ good @skip(if: ) { also } }")

      assert doc.definitions.first.selections.any? { |s| s.is_a?(GraphQL::Language::Nodes::UnrecoverableSelection) }
      refute doc.valid?
    end

    it "prints a recovered document without crashing" do
      doc = GraphQL.parse_with_diagnostics("query A { a(x: ) }\nquery B { b { c")
      assert_kind_of String, doc.to_query_string
    end

    it "visits a recovered document with the modifying visitor without crashing" do
      doc = GraphQL.parse_with_diagnostics("query A { a(x: ) }\nquery B { b }")
      visited = []
      visitor = Class.new(GraphQL::Language::Visitor) do
        define_method(:on_field) do |node, parent|
          visited << node.name
          super(node, parent)
        end
      end
      visitor.new(doc).visit
      assert_includes visited, "b"
    end

    it "visits a recovered document with the static visitor without crashing" do
      doc = GraphQL.parse_with_diagnostics("query A { a(x: ) }\nquery B { b }")
      count = 0
      visitor = Class.new(GraphQL::Language::StaticVisitor) do
        define_method(:on_field) { |_node, _parent| count += 1 }
      end
      visitor.new(doc).visit
      assert_equal 1, count
    end

    it "rejects a document containing only comments" do
      doc = GraphQL.parse_with_diagnostics("# only a comment\n")
      assert_equal 1, doc.parse_errors.size
      refute doc.valid?
    end

    it "raises when no query string is present" do
      assert_raises(GraphQL::ParseError) do
        GraphQL.parse_with_diagnostics(nil)
      end
    end

    it "reports invalid UTF-8 as a single error instead of crashing" do
      bad_string = "{ foo(query: \"\xBF\") }"
      doc = GraphQL.parse_with_diagnostics(bad_string)

      assert_equal 1, doc.parse_errors.size
      assert_includes doc.parse_errors.first.message, "bad Unicode"
      refute doc.valid?
    end

    it "reports a dangling closing brace without dropping the document" do
      doc = GraphQL.parse_with_diagnostics("{ foo } }")

      refute doc.valid?
      assert doc.parse_errors.size >= 1
      assert_kind_of GraphQL::Language::Nodes::OperationDefinition, doc.definitions.first
    end

    it "recovers from a malformed directive argument and keeps surrounding fields" do
      doc = GraphQL.parse_with_diagnostics("{ before field(arg: @) after }")

      names = doc.definitions.first.selections.flat_map { |s| s.respond_to?(:name) ? [s.name] : [] }
      assert_includes names, "before"
      assert_includes names, "after"
      refute doc.valid?
    end

    def doc_with_error(source)
      GraphQL.parse_with_diagnostics(source)
    end
  end

  describe "execution of recovered documents" do
    let(:schema) do
      Class.new(GraphQL::Schema) do
        query_type = Class.new(GraphQL::Schema::Object) do
          graphql_name "Query"
          field :good, String
          define_method(:good) { "ok" }
        end
        query(query_type)
      end
    end

    it "refuses to execute an invalid document and surfaces the parse error" do
      doc = GraphQL.parse_with_diagnostics("{ good }\n{ broken {")
      refute doc.valid?

      result = schema.execute(document: doc)
      assert_equal 1, result["errors"].size
      assert result["errors"].first["message"].include?("Expected")
      assert_nil result["data"]
    end

    it "executes a valid document produced by the diagnostic parser" do
      doc = GraphQL.parse_with_diagnostics("{ good }")
      result = schema.execute(document: doc)
      assert_equal({ "good" => "ok" }, result.to_h["data"])
    end
  end
end
