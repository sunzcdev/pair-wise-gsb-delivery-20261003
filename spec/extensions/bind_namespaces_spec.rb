# frozen_string_literal: true
require_relative "spec_helper"

describe "bind_namespaces extension" do
  before do
    @db = Sequel.mock
    @db.extension :bind_namespaces
    @db.extend_datasets do
      def supports_cte?; true end
      def supports_cte_in_subqueries?; true end
    end
  end

  describe "#bind_namespace" do
    it "rewrites unqualified placeholders into an isolated namespace" do
      ds = @db[:orders].where(id: :$id).bind_namespace(:orders, id: 1)
      ds.opts[:where].args.last.must_equal :"$orders__id"
      ds.bind_namespace_registry.must_equal(:"$orders__id" => 1)
    end

    it "keeps the leading $ so the symbol remains a recognized placeholder" do
      ds = @db[:items].where(a: :$a).bind_namespace(:ns, a: 1)
      ds.sql.must_equal "SELECT * FROM items WHERE (a = $ns__a)"
    end

    it "isolates two datasets that declared the same placeholder name" do
      orders  = @db[:orders].where(id: :$id).bind_namespace(:orders, id: 1)
      refunds = @db[:refunds].where(id: :$id).bind_namespace(:refunds, id: 2)
      combined = @db[:t].with(:o, orders).with(:r, refunds)
      cq = combined.compile_bindings

      cq.sql.must_equal "WITH o AS (SELECT * FROM orders WHERE (id = ?)), " \
                        "r AS (SELECT * FROM refunds WHERE (id = ?)) SELECT * FROM t"
      cq.bindings.must_equal(:"$orders__id" => 1, :"$refunds__id" => 2)
      cq.bind_arguments.must_equal [1, 2]
    end

    it "is idempotent when applied more than once" do
      ds = @db[:t].where(a: :$a).bind_namespace(:ns, a: 1)
      again = ds.bind_namespace(:ns, a: 1)
      again.opts[:where].must_equal ds.opts[:where]
      again.bind_namespace_registry.must_equal ds.bind_namespace_registry
    end

    it "preserves the registry through further query methods" do
      ds = @db[:t].where(a: :$a).bind_namespace(:ns, a: 1).where(b: 2)
      ds.bind_namespace_registry.must_equal(:"$ns__a" => 1)
    end
  end

  describe "#shared_bind_namespace" do
    it "maps datasets sharing a namespace to one bind name and value" do
      a = @db[:orders].where(id: :$id).shared_bind_namespace(:common, id: 5)
      b = @db[:refunds].where(id: :$id).shared_bind_namespace(:common, id: 5)
      cq = @db[:t].with(:o, a).with(:r, b).compile_bindings

      cq.bindings.must_equal(:"$common__id" => 5)
      cq.bind_arguments.must_equal [5, 5]
    end

    it "raises when the same shared name gets values of incompatible types" do
      a = @db[:orders].where(id: :$id).shared_bind_namespace(:common, id: 1)
      b = @db[:refunds].where(id: :$id).shared_bind_namespace(:common, id: "two")
      proc { a.merge_bind_namespaces(b) }.must_raise Sequel::Error
      begin
        a.merge_bind_namespaces(b)
      rescue Sequel::Error => e
        e.message.must_include "incompatible bind types"
        e.message.must_include ":$common__id"
      end
    end

    it "does not let a later hash value overwrite an earlier one" do
      a = @db[:orders].where(id: :$id).shared_bind_namespace(:common, id: 1)
      b = @db[:refunds].where(id: :$id).shared_bind_namespace(:common, id: 1)
      a.merge_bind_namespaces(b).bind_namespace_registry.must_equal(:"$common__id" => 1)
    end

    it "treats different numeric classes as compatible" do
      a = @db[:orders].where(id: :$id).shared_bind_namespace(:n, id: 1)
      b = @db[:refunds].where(id: :$id).shared_bind_namespace(:n, id: 2.5)
      a.merge_bind_namespaces(b).bind_namespace_registry.must_equal(:"$n__id" => 1)
    end

    it "detects incompatible shared types nested inside CTEs at compile time" do
      a = @db[:a].where(id: :$id).shared_bind_namespace(:s, id: 1)
      b = @db[:b].where(id: :$id).shared_bind_namespace(:s, id: "str")
      proc { @db[:t].with(:x, a).with(:y, b).compile_bindings }.must_raise Sequel::Error
    end
  end

  describe "#compile_bindings" do
    it "handles placeholders in nested CTEs, subqueries and literal SQL separately" do
      inner = @db[:i].where(a: :$a).where(Sequel.lit("b = ?", :$b))
      outer = @db[:o].with(:ic, inner).
                where(Sequel.lit("c IN (?)", @db[:sub].where(d: :$d)))
      cq = outer.bind_namespace(:ns, a: 1, b: "two", d: Date.new(2020, 1, 2)).
                 compile_bindings

      cq.sql.must_equal "WITH ic AS (SELECT * FROM i WHERE ((a = ?) AND (b = ?))) " \
                        "SELECT * FROM o WHERE (c IN ((SELECT * FROM sub WHERE (d = ?))))"
      cq.bindings.must_equal(:"$ns__a" => 1, :"$ns__b" => "two",
                             :"$ns__d" => Date.new(2020, 1, 2))
      cq.bind_arguments.must_equal [1, "two", Date.new(2020, 1, 2)]
    end

    it "does not rewrite placeholder-looking text inside string constants" do
      ds = @db[:t].where(text: "the id is :$ns__a and ':$b' text")
      cq = ds.bind_namespace(:ns).compile_bindings
      cq.sql.must_equal "SELECT * FROM t WHERE (text = 'the id is :$ns__a and '':$b'' text')"
      cq.bind_arguments.must_equal []
    end

    it "leaves raw literal SQL without placeholder args untouched" do
      ds = @db[:t].where(Sequel.lit("raw = 123 AND x = ':$x'"))
      cq = ds.bind_namespace(:l).compile_bindings
      cq.sql.must_equal "SELECT * FROM t WHERE (raw = 123 AND x = ':$x')"
      cq.bind_arguments.must_equal []
    end

    it "handles a placeholder used more than once, repeating the argument" do
      ds = @db[:t].where(Sequel.lit("x = ? OR y = ?", :$v, :$v))
      cq = ds.bind_namespace(:ns, v: 9).compile_bindings
      cq.sql.must_equal "SELECT * FROM t WHERE (x = ? OR y = ?)"
      cq.bindings.must_equal(:"$ns__v" => 9)
      cq.bind_arguments.must_equal [9, 9]
    end

    it "handles named placeholder hashes in PlaceholderLiteralString" do
      pls = Sequel::SQL::PlaceholderLiteralString.new("a = :v OR b = :v", v: :$v)
      cq = @db[:t].where(pls).bind_namespace(:h, v: 42).compile_bindings
      cq.sql.must_equal "SELECT * FROM t WHERE (a = ? OR b = ?)"
      cq.bind_arguments.must_equal [42, 42]
    end

    it "handles placeholders in JOIN ON clauses" do
      ds = @db[:a].join(:b, Sequel.lit("a.id = b.aid AND b.kind = ?", :$kind))
      cq = ds.bind_namespace(:j, kind: "vip").compile_bindings
      cq.sql.must_equal "SELECT * FROM a INNER JOIN b ON (a.id = b.aid AND b.kind = ?)"
      cq.bind_arguments.must_equal ["vip"]
    end

    it "handles placeholders in compound (UNION) queries" do
      u = @db[:t].where(x: :$x).union(@db[:t].where(y: :$y), from_self: false)
      cq = u.bind_namespace(:u, x: 1, y: 2).compile_bindings
      cq.sql.must_equal "SELECT * FROM t WHERE (x = ?) UNION SELECT * FROM t WHERE (y = ?)"
      cq.bind_arguments.must_equal [1, 2]
    end

    it "handles recursive CTEs" do
      base = @db[:nodes].where(id: :$root)
      rec  = @db[:nodes].join(:tree, id: :parent_id)
      rcte = @db[:nodes].
               with_recursive(:tree, base.bind_namespace(:rc, root: 1),
                              rec.bind_namespace(:rc, root: 1)).from(:tree)
      cq = rcte.compile_bindings
      cq.sql.must_equal "WITH tree AS (SELECT * FROM nodes WHERE (id = ?) UNION ALL " \
                        "SELECT * FROM nodes INNER JOIN tree ON (tree.id = nodes.parent_id)) " \
                        "SELECT * FROM tree"
      cq.bind_arguments.must_equal [1]
    end

    it "handles deeply nested from_self subqueries" do
      ds = @db[:a].where(x: :$x).from_self.where(y: :$y)
      cq = ds.bind_namespace(:d, x: 1, y: 2).compile_bindings
      cq.sql.must_equal "SELECT * FROM (SELECT * FROM a WHERE (x = ?)) AS t1 WHERE (y = ?)"
      cq.bind_arguments.must_equal [1, 2]
    end

    it "handles placeholders in select, group, order and limit clauses" do
      ds = @db[:t].select(Sequel.lit("COALESCE(?, 0)", :$amt)).
                   group(Sequel.lit("(?)", :$g)).order(Sequel.lit("(?)", :$o)).limit(:$lim)
      cq = ds.bind_namespace(:q, amt: 100, g: "x", o: "y", lim: 5).compile_bindings
      cq.sql.must_equal "SELECT COALESCE(?, 0) FROM t GROUP BY (?) ORDER BY (?) LIMIT ?"
      cq.bind_arguments.must_equal [100, "x", "y", 5]
    end

    it "returns an empty binding for a query with no placeholders" do
      cq = @db[:t].where(a: 1).bind_namespace(:p).compile_bindings
      cq.sql.must_equal "SELECT * FROM t WHERE (a = 1)"
      cq.bindings.must_equal({})
      cq.bind_arguments.must_equal []
    end

    it "raises when a placeholder has no registered value" do
      ds = @db[:t].where(a: :$a).bind_namespace(:ns)
      proc { ds.compile_bindings }.must_raise Sequel::Error
    end

    it "raises when an unqualified placeholder is not in a namespace" do
      proc { @db[:t].where(a: :$a).compile_bindings }.must_raise Sequel::Error
    end

    it "fills namespaced placeholders from Dataset#bind by trailing name" do
      ds = @db[:t].where(a: :$a).bind_namespace(:ns).bind(a: 77)
      cq = ds.compile_bindings
      cq.bindings.must_equal(:"$ns__a" => 77)
      cq.bind_arguments.must_equal [77]
    end

    it "freezes the returned binding map and arguments" do
      cq = @db[:t].where(a: :$a).bind_namespace(:ns, a: 1).compile_bindings
      cq.bindings.frozen?.must_equal true
      cq.bind_arguments.frozen?.must_equal true
    end

    it "produces SQL that can be executed by the database" do
      ds = @db[:t].where(a: :$a).bind_namespace(:ns, a: 1)
      cq = ds.compile_bindings
      @db.run(cq.sql)
      @db.sqls.must_equal ["SELECT * FROM t WHERE (a = ?)"]
    end

    it "compiles insert statements with a type and values" do
      ds = @db[:t].bind_namespace(:ns, v: "hello")
      cq = ds.compile_bindings(:insert, x: :"$ns__v")
      cq.sql.must_equal "INSERT INTO t (x) VALUES (?)"
      cq.bind_arguments.must_equal ["hello"]
    end

    it "compiles update statements with a type and values" do
      ds = @db[:t].where(id: :$id).bind_namespace(:ns, id: 3, v: "x")
      cq = ds.compile_bindings(:update, {x: :"$ns__v"})
      cq.sql.must_equal "UPDATE t SET x = ? WHERE (id = ?)"
      cq.bind_arguments.must_equal ["x", 3]
    end

    it "compiles delete statements with a type" do
      ds = @db[:t].where(id: :$id).bind_namespace(:ns, id: 3)
      cq = ds.compile_bindings(:delete)
      cq.sql.must_equal "DELETE FROM t WHERE (id = ?)"
      cq.bind_arguments.must_equal [3]
    end

    it "handles placeholders inside an IN value list" do
      ds = @db[:t].where(Sequel[:x] => [1, :$v, 3]).bind_namespace(:ns, v: 2)
      cq = ds.compile_bindings
      cq.sql.must_equal "SELECT * FROM t WHERE (x IN (1, ?, 3))"
      cq.bind_arguments.must_equal [2]
    end

    it "leaves ordinary symbol column references untouched" do
      ds = @db[:t].where(col: :other_col).bind_namespace(:ns)
      ds.sql.must_equal "SELECT * FROM t WHERE (col = other_col)"
    end
  end

  describe "#merge_bind_namespaces" do
    it "combines registries from separately built datasets" do
      a = @db[:a].where(x: :$x).bind_namespace(:a, x: 1)
      b = @db[:b].where(y: :$y).bind_namespace(:b, y: 2)
      merged = a.merge_bind_namespaces(b)
      merged.bind_namespace_registry.must_equal(:"$a__x" => 1, :"$b__y" => 2)
    end

    it "raises on incompatible duplicate names instead of overwriting" do
      a = @db[:a].where(x: :$x).shared_bind_namespace(:s, x: 1)
      b = @db[:b].where(x: :$x).shared_bind_namespace(:s, x: "no")
      proc { a.merge_bind_namespaces(b) }.must_raise Sequel::Error
    end
  end

  describe "loading" do
    it "can be loaded into a single dataset" do
      ds = @db[:t].extension(:bind_namespaces).where(a: :$a).bind_namespace(:ns, a: 1)
      ds.compile_bindings.bind_arguments.must_equal [1]
    end
  end
end
