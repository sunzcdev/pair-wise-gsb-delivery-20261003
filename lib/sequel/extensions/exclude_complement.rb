# frozen_string_literal: true
#
# The exclude_complement extension adds Dataset#exclude_complement and
# Dataset#exclude_complement_having.  These methods exclude the rows that
# match the given condition using set complement semantics, which differs
# from Dataset#exclude and Dataset#exclude_having.
#
# Dataset#exclude inverts conditions using SQL 3-valued logic.  For example:
#
#   DB[:table].exclude(column: 1)
#   # SELECT * FROM table WHERE (column != 1)
#
# Because +NULL != 1+ evaluates to +NULL+ (not +true+), rows where +column+
# is +NULL+ are not returned by that query.  The same is true for plain SQL
# +NOT+:
#
#   DB[:table].where(Sequel.~(column: 1))
#   # SELECT * FROM table WHERE (column != 1)
#
# exclude_complement instead computes the relational complement of the
# matching row set, so rows where the condition is +NULL+ are kept (they do
# not match the condition and therefore belong to its complement):
#
#   DB[:table].extension(:exclude_complement).exclude_complement(column: 1)
#   # SELECT * FROM table WHERE ((column != 1) OR (column IS NULL))
#
# This is implemented by recursively rewriting a fixed set of supported
# expression nodes instead of wrapping an arbitrary expression in +NOT+:
#
# * +AND+ becomes +OR+ (and vice versa) with each branch complemented,
#   following De Morgan's laws.
# * Field comparisons (+=+, +!=+, +<+, +>+, +<=+, +>=+, +LIKE+, +NOT LIKE+,
#   +ILIKE+, +NOT ILIKE+ and the regular expression operators) are inverted
#   and combined with an +OR column IS NULL+ for every column reference that
#   can be +NULL+.
# * +IN+ / +NOT IN+, including subquery value lists, are inverted and
#   combined with an +OR column IS NULL+.
# * +IS+ / +IS NOT+, +NOT+, +NOOP+, and the +TRUE+, +FALSE+, +NULL+, and
#   +NOT NULL+ boolean constants are handled directly.
#
# Parameters are still bound; the rewrite only rearranges the abstract
# syntax tree and never interpolates values.
#
# Because the complement cannot be inferred safely for every expression node,
# the compiler raises an +Sequel::Error+ for unsupported nodes (such as raw
# SQL fragments, SQL functions used as boolean predicates, +CASE+
# expressions, and +EXISTS+ subqueries) instead of guessing an +IS NULL+
# clause for them.
#
# You can load this extension into specific datasets:
#
#   ds = DB[:table]
#   ds = ds.extension(:exclude_complement)
#
# Or you can load it into all of a database's datasets:
#
#   DB.extension(:exclude_complement)
#
# Related module: Sequel::ExcludeComplement

#
module Sequel
  module ExcludeComplement
    # Excludes rows matching the given condition using set complement
    # semantics, adding the resulting expression to the WHERE clause.
    # Rows where the condition evaluates to +NULL+ are kept, because they
    # do not belong to the set of rows matching the condition:
    #
    #   DB[:items].exclude_complement(category: 'software')
    #   # SELECT * FROM items WHERE ((category != 'software') OR (category IS NULL))
    #
    #   DB[:items].exclude_complement(category: 'software', id: 3)
    #   # SELECT * FROM items WHERE (((category != 'software') OR (category IS NULL))
    #   #   OR ((id != 3) OR (id IS NULL)))
    def exclude_complement(*cond, &block)
      add_filter(:where, cond, :complement, &block)
    end

    # The same as #exclude_complement, but affecting the HAVING clause
    # instead of the WHERE clause.
    #
    #   DB[:items].select_group(:name).
    #     exclude_complement_having(name: 'unknown')
    #   # SELECT name FROM items GROUP BY name
    #   # HAVING ((name != 'unknown') OR (name IS NULL))
    def exclude_complement_having(*cond, &block)
      add_filter(:having, cond, :complement, &block)
    end

    private

    # Recognize the :complement value for invert, returning an expression
    # for the relational complement of the condition.
    def _invert_filter(cond, invert)
      if invert == :complement
        complement_expression(cond)
      else
        super
      end
    end

    # Recursively return the set complement of the given boolean expression.
    # Raises Sequel::Error for expression nodes whose complement cannot be
    # determined without guessing.
    def complement_expression(expr)
      case expr
      when SQL::BooleanExpression
        case op = expr.op
        when :AND, :OR
          SQL::BooleanExpression.new(
            SQL::ComplexExpression::OPERATOR_INVERSIONS[op],
            *expr.args.map { |arg| complement_expression(arg) }
          )
        when :IN, :"NOT IN"
          complement_in_expression(expr)
        when :IS, :"IS NOT"
          SQL::BooleanExpression.new(SQL::ComplexExpression::OPERATOR_INVERSIONS[op], *expr.args.dup)
        when :NOT
          # Complement of NOT(x) is the set where x is not FALSE (TRUE or
          # NULL), so the NULL rows of the inner predicate are kept.
          complement_negated_expression(expr.args.first)
        when :NOOP
          complement_expression(expr.args.first)
        when :'=', :'!=', :<, :>, :<=, :>=,
             :LIKE, :'NOT LIKE', :ILIKE, :'NOT ILIKE',
             :~, :'!~', :'~*', :'!~*'
          complement_comparison(expr)
        else
          raise(Sequel::Error, "cannot compute set complement for expression with #{op.inspect} operator: #{expr.inspect}")
        end
      when TrueClass
        SQL::Constants::FALSE
      when FalseClass
        SQL::Constants::TRUE
      when SQL::BooleanConstant
        case expr.constant
        when true
          SQL::Constants::FALSE
        when false
          SQL::Constants::TRUE
        when nil
          SQL::Constants::NOTNULL
        else
          raise(Sequel::Error, "cannot compute set complement for #{expr.inspect}")
        end
      when SQL::NegativeBooleanConstant
        case expr.constant
        when nil
          SQL::Constants::NULL
        else
          raise(Sequel::Error, "cannot compute set complement for #{expr.inspect}")
        end
      when Symbol, SQL::Identifier, SQL::QualifiedIdentifier
        if placeholder_symbol?(expr) || (expr.is_a?(SQL::Identifier) && placeholder_symbol?(expr.value))
          raise(Sequel::Error, "cannot compute set complement for unsupported expression: #{expr.inspect}")
        end

        # A bare boolean column/predicate: its complement is false or NULL.
        SQL::BooleanExpression.new(:OR,
          SQL::BooleanExpression.new(:NOT, expr),
          SQL::BooleanExpression.new(:IS, expr, nil)
        )
      else
        raise(Sequel::Error, "cannot compute set complement for unsupported expression: #{expr.inspect}")
      end
    end

    # Complement of an IN/NOT IN expression, defined as the rows where the
    # membership test is not TRUE.  NULL entries in an array or set value
    # list are handled explicitly, because SQL's 3-valued logic makes
    # +NOT IN (..., NULL, ...)+ never TRUE.  Subquery value lists cannot be
    # inspected, so their NULL rows are handled with an explicit IS NULL.
    def complement_in_expression(expr)
      column, values = expr.args
      c = SQL::BooleanExpression

      case values
      when ::Array, ::Set
        contains_null = values.any?(&:nil?)
        non_null_values = values.reject(&:nil?).freeze

        if expr.op == :IN
          if non_null_values.empty?
            # IN of a NULL-only or empty list is never TRUE, so keep every row.
            return c.new(:NOOP, true)
          end

          # Rows not in the (non-NULL) list, together with NULL rows.  NULL
          # entries inside the list do not change which rows IN matches.
          c.new(:OR,
            c.new(:"NOT IN", column, non_null_values),
            c.new(:IS, column, nil)
          )
        else
          # NOT IN with any NULL in the list is never TRUE for any row, so
          # its complement keeps every row.
          return c.new(:NOOP, true) if contains_null

          if non_null_values.empty?
            # NOT IN of an empty list is always TRUE, so its complement
            # matches no rows.
            return c.new(:NOOP, false)
          end

          # NOT IN is false for listed rows and NULL (not TRUE) for NULL
          # rows, so its complement is the listed rows or the NULL rows.
          c.new(:OR,
            c.new(:IN, column, non_null_values),
            c.new(:IS, column, nil)
          )
        end
      else
        # Subquery or other value expression whose contents are not known at
        # compile time.  Both IN and NOT IN are not TRUE for NULL rows, so
        # the complement keeps the NULL rows explicitly in both cases.
        c.new(:OR,
          c.new(SQL::ComplexExpression::OPERATOR_INVERSIONS[expr.op], column, values),
          c.new(:IS, column, nil)
        )
      end
    end

    # Complement of a NOT expression (+NOT inner+), i.e. the rows where the
    # inner predicate is not FALSE (it is TRUE or NULL).  This is the dual of
    # #complement_expression, which keeps rows where the predicate is not TRUE.
    def complement_negated_expression(expr)
      case expr
      when SQL::BooleanExpression
        case op = expr.op
        when :AND, :OR
          SQL::BooleanExpression.new(
            SQL::ComplexExpression::OPERATOR_INVERSIONS[op],
            *expr.args.map { |arg| complement_negated_expression(arg) }
          )
        when :IN, :"NOT IN"
          negated_in_expression(expr)
        when :IS, :"IS NOT"
          # NOT(IS x) and NOT(IS NOT x) are never NULL.  The outer NOT is
          # absorbed here, so the complement keeps the same operator instead
          # of inverting it a second time.
          SQL::BooleanExpression.new(op, *expr.args.dup)
        when :NOT
          # NOT NOT x == x; complement collapses back to the regular one.
          complement_expression(expr.args.first)
        when :NOOP
          complement_negated_expression(expr.args.first)
        when :'=', :'!=', :<, :>, :<=, :>=,
             :LIKE, :'NOT LIKE', :ILIKE, :'NOT ILIKE',
             :~, :'!~', :'~*', :'!~*'
          negated_comparison(expr)
        else
          raise(Sequel::Error, "cannot compute set complement for expression with #{op.inspect} operator: #{expr.inspect}")
        end
      when TrueClass
        SQL::Constants::TRUE
      when FalseClass
        SQL::Constants::FALSE
      when SQL::BooleanConstant
        case expr.constant
        when true
          SQL::Constants::TRUE
        when false
          SQL::Constants::FALSE
        when nil
          # NOT NULL is never TRUE, so its complement matches everything.
          SQL::BooleanExpression.new(:NOOP, true)
        else
          raise(Sequel::Error, "cannot compute set complement for #{expr.inspect}")
        end
      when SQL::NegativeBooleanConstant
        case expr.constant
        when nil
          # NOT (NOT NULL) == NULL, which is never TRUE, so keep everything.
          SQL::BooleanExpression.new(:NOOP, true)
        else
          raise(Sequel::Error, "cannot compute set complement for #{expr.inspect}")
        end
      when Symbol, SQL::Identifier, SQL::QualifiedIdentifier
        if placeholder_symbol?(expr) || (expr.is_a?(SQL::Identifier) && placeholder_symbol?(expr.value))
          raise(Sequel::Error, "cannot compute set complement for unsupported expression: #{expr.inspect}")
        end

        # NOT column is TRUE when the column is FALSE; the complement keeps
        # rows where the column is TRUE or NULL.
        SQL::BooleanExpression.new(:OR, expr, SQL::BooleanExpression.new(:IS, expr, nil))
      else
        raise(Sequel::Error, "cannot compute set complement for unsupported expression: #{expr.inspect}")
      end
    end

    # Dual of #complement_in_expression for the inner predicate of a NOT.
    # Given +NOT(inner)+, returns the rows where +NOT(inner)+ is not TRUE,
    # i.e. the rows where +inner+ is TRUE or NULL.
    def negated_in_expression(expr)
      column, values = expr.args
      c = SQL::BooleanExpression

      case values
      when ::Array, ::Set
        contains_null = values.any?(&:nil?)
        non_null_values = values.reject(&:nil?).freeze

        if expr.op == :IN
          # Keep rows where a IN list is TRUE or NULL.  When the list itself
          # contains a NULL, every non-listed non-NULL row makes IN NULL too,
          # so in that case every row is kept.
          if contains_null
            c.new(:NOOP, true)
          elsif non_null_values.empty?
            # IN of an empty list is never TRUE, so only the NULL rows remain.
            c.new(:IS, column, nil)
          else
            c.new(:OR,
              c.new(:IN, column, non_null_values),
              c.new(:IS, column, nil)
            )
          end
        else
          # Inner is a NOT IN.  Keep the rows where a NOT IN list is TRUE or
          # NULL (it is never TRUE when the list contains a NULL, but the
          # resulting expression still describes exactly the surviving rows).
          if non_null_values.empty? && !contains_null
            # NOT IN of an empty list is always TRUE, so NOT(...) is always
            # FALSE and no row belongs to the complement.
            c.new(:NOOP, false)
          else
            c.new(:OR,
              c.new(:"NOT IN", column, non_null_values),
              c.new(:IS, column, nil)
            )
          end
        end
      else
        # Subquery whose contents are unknown: keep the rows where the inner
        # test is TRUE or NULL, expressed as the unchanged inner test plus an
        # explicit IS NULL for the column.
        c.new(:OR,
          c.new(expr.op, column, values),
          c.new(:IS, column, nil)
        )
      end
    end

    # Dual of #complement_comparison for the inner predicate of a NOT.
    # Returns the rows where the comparison is not FALSE.
    def negated_comparison(expr)
      validate_comparison_operands!(expr)
      lhs, rhs = expr.args

      null_conditions = []
      null_conditions << SQL::BooleanExpression.new(:IS, lhs, nil) if column_operand?(lhs)
      null_conditions << SQL::BooleanExpression.new(:IS, rhs, nil) if column_operand?(rhs)

      if null_conditions.empty?
        SQL::BooleanExpression.new(expr.op, lhs, rhs)
      else
        SQL::BooleanExpression.new(:OR,
          SQL::BooleanExpression.new(expr.op, lhs, rhs), *null_conditions)
      end
    end

    # Complement of a two-argument comparison, keeping rows where either
    # side of the comparison is NULL (the comparison itself is then NULL).
    def complement_comparison(expr)
      validate_comparison_operands!(expr)
      lhs, rhs = expr.args
      inverted = SQL::BooleanExpression.new(
        SQL::ComplexExpression::OPERATOR_INVERSIONS[expr.op], lhs, rhs
      )

      null_conditions = []
      null_conditions << SQL::BooleanExpression.new(:IS, lhs, nil) if column_operand?(lhs)
      null_conditions << SQL::BooleanExpression.new(:IS, rhs, nil) if column_operand?(rhs)

      if null_conditions.empty?
        inverted
      else
        SQL::BooleanExpression.new(:OR, inverted, *null_conditions)
      end
    end

    # Whether the operand is a column reference whose value can be NULL.
    # Symbols starting with +$+ are prepared statement placeholders, not
    # columns, so they are not treated as nullable.
    def column_operand?(operand)
      case operand
      when Symbol
        !placeholder_symbol?(operand)
      when SQL::Identifier
        !placeholder_symbol?(operand.value)
      when SQL::QualifiedIdentifier
        true
      else
        false
      end
    end

    # Whether the given value is a prepared statement placeholder symbol
    # (a symbol whose name starts with +$+).
    def placeholder_symbol?(value)
      value.is_a?(::Symbol) && value.to_s.start_with?("$")
    end

    # Validate that every operand of a comparison is either a column
    # reference or a scalar literal whose NULL-ness is known.  Expressions
    # such as SQL functions or CASE expressions are rejected instead of
    # guessing how their NULL values should be handled.
    def validate_comparison_operands!(expr)
      expr.args.each do |operand|
        next if column_operand?(operand)
        next if operand.nil? || operand.is_a?(::Numeric) || operand.is_a?(::String) ||
                operand.is_a?(::Symbol) || operand.is_a?(::Date) ||
                operand.is_a?(::Time) || operand.is_a?(::DateTime) ||
                operand.is_a?(::TrueClass) || operand.is_a?(::FalseClass)

        raise(Sequel::Error, "cannot compute set complement for comparison with unsupported operand: #{operand.inspect}")
      end
    end
  end

  Dataset.register_extension(:exclude_complement, ExcludeComplement)
end

