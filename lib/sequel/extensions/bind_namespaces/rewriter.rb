# frozen-string-literal: true
#
# The bind_namespaces extension provides explicit parameter namespaces for
# datasets that use bound variable placeholders (symbols such as :$id).
#
# Two reusable datasets can legitimately declare placeholders with the same
# name while meaning different values:
#
#   orders   = DB[:orders].where(id: :$id)
#   refunds  = DB[:refunds].where(id: :$id)
#
# Combining them into a single query cannot silently share one value.  This
# extension lets each dataset be assigned an isolated parameter namespace, so
# colliding names are rewritten into distinct final bind names, or an explicit
# shared namespace when the values are intentionally the same:
#
#   DB.extension :bind_namespaces
#
#   a = orders.bind_namespace(:orders,  id: 1)
#   b = refunds.bind_namespace(:refunds, id: 2)
#
#   combined = DB[:t].with(:o, a).with(:r, b)
#   cq = combined.compile_bindings
#   cq.sql
#   # WITH o AS (SELECT * FROM orders WHERE (id = ?)),
#   #      r AS (SELECT * FROM refunds WHERE (id = ?)) SELECT * FROM t
#   cq.bindings
#   # {:orders__id=>1, :refunds__id=>2}
#   cq.bind_arguments
#   # [1, 2]
#
# Intentionally sharing a value uses #shared_bind_namespace.  Reusing a shared
# name with values of incompatible types raises an error at combination time
# instead of letting the last merged Hash value silently win:
#
#   a = orders.shared_bind_namespace(:common, id: 1)
#   b = refunds.shared_bind_namespace(:common, id: "two")
#   a.merge_bind_namespaces(b) # raises Sequel::Error
#
# Placeholders are handled separately wherever they occur: in nested CTEs, in
# subqueries, and in the arguments of literal SQL (Sequel.lit).  Text inside
# string constants is never rewritten, so a value such as ":$id" inside a
# quoted string is left untouched.
#
# Related module: Sequel::BindNamespaces

module Sequel
  # Namespace for the bind_namespaces extension.
  module BindNamespaces
    # The string used to separate a namespace name from a placeholder name.
    NAMESPACE_SEPARATOR = "__".freeze

    # Dataset options that hold SQL expressions or nested datasets which may
    # contain placeholders.  Only these whitelisted options are traversed, so
    # internal bookkeeping options (bind variables, caches, recorders) and plain
    # strings are never mistaken for placeholder expressions.
    TRAVERSED_DATASET_OPTS = %i'
      with select distinct from join where group having compounds order
      limit offset lock values returning sql merge_using merge_when
    '.freeze

    # Matches a bound variable placeholder symbol, the same shape recognized by
    # Sequel's prepared statement support (:$name).  The captured name must be
    # non-empty.
    PLACEHOLDER_SYMBOL_RE = /\A\$(.+)\z/.freeze

    class << self
      # Rewrite every unqualified placeholder reachable from +obj+ so that it
      # belongs to +namespace+, returning the rewritten value.  +registry+ maps
      # final qualified placeholder names to their bound values.  When
      # +shared+ is true, registrations of the same qualified name are checked
      # for type compatibility rather than being overwritten.
      def rewrite(obj, namespace, registry, shared)
        Rewriter.new(namespace, registry, shared).rewrite(obj)
      end

      # Return true if the two values registered under one shared placeholder
      # name are type compatible.  Classes must match, except that different
      # numeric classes are treated as compatible with each other.
      def compatible_types?(a, b)
        return true if a.class == b.class
        a.is_a?(Numeric) && b.is_a?(Numeric)
      end
    end

    # The result of compiling a dataset's bound variable namespaces.  Returned
    # by Sequel::Dataset#compile_bindings.
    class CompiledQuery
      # The final SQL string, with positional +?+ placeholders in the order
      # given by #bind_arguments.
      attr_reader :sql

      # Frozen hash mapping final qualified placeholder names to their values,
      # in the order each placeholder is first encountered in the query.
      attr_reader :bindings

      # Frozen array of bind values ordered to match the +?+ placeholders in
      # #sql (a value used more than once appears more than once).
      attr_reader :bind_arguments

      # The rewritten dataset from which the SQL was compiled.
      attr_reader :dataset

      def initialize(sql, bindings, bind_arguments, dataset)
        @sql = sql
        @bindings = bindings.freeze
        @bind_arguments = bind_arguments.freeze
        @dataset = dataset
        freeze
      end

      # Human readable representation of the compiled query and its bindings.
      def inspect
        "#<#{self.class} #{sql.inspect} bindings=#{bindings.inspect} arguments=#{bind_arguments.inspect}>"
      end
    end

    # Depth-first rewriter that prefixes placeholder symbols with a namespace.
    # Plain strings (including string constants and the template text of
    # literal SQL) are returned unchanged; only the real placeholder objects in
    # expression trees, placeholder argument lists and nested datasets are
    # visited.
    class Rewriter
      def initialize(namespace, registry, shared)
        @namespace = namespace
        @registry = registry
        @shared = shared
      end

      # Entry point for arbitrary values found in the query tree.
      def rewrite(obj)
        case obj
        when Sequel::Dataset
          rewrite_dataset(obj)
        when Sequel::SQL::Expression
          rewrite_expression(obj)
        when ::Array
          rewrite_array(obj)
        when ::Hash
          rewrite_hash(obj)
        when ::Set
          obj.class.new(obj.map { |v| rewrite(v) })
        when Symbol
          rewrite_symbol(obj)
        else
          # Plain strings, numerics, nil, true/false, classes, times, etc.
          # contain no placeholders and must not be altered; in particular the
          # contents of string constants are preserved verbatim.
          obj
        end
      end

      private

      # Prefix an unqualified :$name placeholder with the namespace.  Symbols
      # that are already qualified (present in the registry) are left as-is, so
      # rewriting a dataset more than once is idempotent.  Non-placeholder
      # symbols (column references etc.) are untouched.
      def rewrite_symbol(sym)
        return sym unless (m = PLACEHOLDER_SYMBOL_RE.match(sym.to_s))
        name = m[1]
        # Keep the leading "$" so the result is still a recognized bind
        # placeholder (e.g. :$id becomes :$orders__id).
        qualified = :"$#{@namespace}#{NAMESPACE_SEPARATOR}#{name}"
        return qualified if @registry.key?(qualified) || @registry.key?(sym)
        register(qualified, nil)
        qualified
      end

      def register(qualified, value)
        if @shared && @registry.key?(qualified)
          existing = @registry.fetch(qualified)
          if !value.nil? && !existing.nil? && !BindNamespaces.compatible_types?(existing, value)
            raise Sequel::Error,
              "incompatible bind types for shared placeholder #{qualified.inspect}: " \
              "#{existing.class} (#{existing.inspect}) and #{value.class} (#{value.inspect})"
          end
          # Keep the first registered value; never let a later hash silently
          # overwrite an earlier source for the same name.
          @registry[qualified] = value if existing.nil? && !value.nil?
        else
          @registry[qualified] = value unless @registry.key?(qualified)
        end
      end

      def rewrite_array(array)
        array.map { |v| rewrite(v) }
      end

      def rewrite_hash(hash)
        # Only hash *values* are expressions; keys are identifiers/options and
        # must never be treated as placeholders.
        hash.each_with_object({}) do |(k, v), h|
          h[k] = rewrite(v)
        end
      end

      # Reconstruct SQL expression value objects with rewritten children.  Each
      # expression class is rebuilt through its own constructor so the frozen
      # value-object semantics are preserved.
      def rewrite_expression(e)
        case e
        when Sequel::SQL::ComplexExpression
          e.class.new(e.op, *rewrite(Array(e.args)))
        when Sequel::SQL::CaseExpression
          conditions = rewrite(Array(e.conditions)).map { |c, r| [c, r] }
          Sequel::SQL::CaseExpression.new(conditions, rewrite(e.default), e.expression? ? rewrite(e.expression) : nil)
        when Sequel::SQL::Cast
          Sequel::SQL::Cast.new(rewrite(e.expr), e.type)
        when Sequel::SQL::Function
          opts = rewrite_function_opts(e.opts)
          if opts.equal?(e.opts)
            Sequel::SQL::Function.new!(e.name, rewrite(Array(e.args)), e.opts)
          else
            Sequel::SQL::Function.new!(e.name, rewrite(Array(e.args)), opts)
          end
        when Sequel::SQL::Subscript
          Sequel::SQL::Subscript.new(rewrite(e.expression), rewrite(Array(e.sub)))
        when Sequel::SQL::Window
          Sequel::SQL::Window.new(rewrite_hash(e.opts))
        when Sequel::SQL::Wrapper
          Sequel::SQL::Wrapper.new(rewrite(e.value))
        when Sequel::SQL::Identifier
          Sequel::SQL::Identifier.new(rewrite(e.value))
        when Sequel::SQL::QualifiedIdentifier
          Sequel::SQL::QualifiedIdentifier.new(e.table, rewrite(e.column))
        when Sequel::SQL::ColumnAll
          Sequel::SQL::ColumnAll.new(e.table)
        when Sequel::SQL::OrderedExpression
          Sequel::SQL::OrderedExpression.new(rewrite(e.expression), e.descending, nulls: e.nulls)
        when Sequel::SQL::AliasedExpression
          Sequel::SQL::AliasedExpression.new(rewrite(e.expression), e.alias, e.columns)
        when Sequel::SQL::JoinOnClause
          Sequel::SQL::JoinOnClause.new(rewrite(e.on), e.join_type, rewrite(e.table_expr))
        when Sequel::SQL::JoinUsingClause
          Sequel::SQL::JoinUsingClause.new(rewrite(Array(e.using)), e.join_type, rewrite(e.table_expr))
        when Sequel::SQL::JoinClause
          Sequel::SQL::JoinClause.new(e.join_type, rewrite(e.table_expr))
        when Sequel::SQL::PlaceholderLiteralString
          # The template string (e.str) is literal SQL text and is deliberately
          # not touched: only its real placeholder arguments are rewritten.
          # Named (:key) argument hashes are rewritten by value only.
          Sequel::SQL::PlaceholderLiteralString.new(e.str, rewrite(e.args), e.parens)
        when Sequel::SQL::DelayedEvaluation
          e
        when Sequel::SQL::Constant, Sequel::SQL::BooleanConstant,
             Sequel::SQL::NegativeBooleanConstant, Sequel::SQL::Blob,
             Sequel::LiteralString
          e
        else
          # Unknown expression types expose no placeholder-bearing children to
          # the extension's supported surface.
          e
        end
      end

      # Function option hashes hold nested expressions (filters, windows,
      # ordering).  Return the same frozen hash when nothing changed so callers
      # can avoid rebuilding needlessly.
      def rewrite_function_opts(opts)
        return opts if opts.nil? || opts.empty?
        rewrite_hash(opts)
      end

      # Rewrite a nested dataset in place within its own namespace boundary.
      # Datasets already carrying this rewriter's namespace keep their stored
      # registry entries; otherwise the whole option tree is rewritten with the
      # current namespace.
      def rewrite_dataset(ds)
        stored = ds.opts[:bind_namespace_registry]
        if stored
          # A dataset previously namespaced: merge its registry into ours
          # (with shared-type checks) and keep its already-rewritten tree.
          merge_stored_registry(stored)
          ds
        else
          opts = {}
          TRAVERSED_DATASET_OPTS.each do |key|
            next unless ds.opts.key?(key)
            opts[key] = rewrite(ds.opts[key])
          end
          opts.empty? ? ds : ds.clone(opts)
        end
      end

      def merge_stored_registry(stored)
        stored.each do |name, value|
          if @shared && @registry.key?(name)
            existing = @registry.fetch(name)
            if !value.nil? && !existing.nil? && !BindNamespaces.compatible_types?(existing, value)
              raise Sequel::Error,
                "incompatible bind types for shared placeholder #{name.inspect}: " \
                "#{existing.class} (#{existing.inspect}) and #{value.class} (#{value.inspect})"
            end
            @registry[name] = value if existing.nil? && !value.nil?
          else
            @registry[name] = value unless @registry.key?(name)
          end
        end
      end
    end
  end
end
