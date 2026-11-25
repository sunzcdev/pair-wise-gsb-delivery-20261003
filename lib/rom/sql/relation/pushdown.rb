# frozen_string_literal: true

module ROM
  module SQL
    class Relation < ROM::Relation
      # Preferred-SQL-pushdown with bounded local fallback
      #
      # A relation expression is first offered to the *real* connection of
      # the gateway the relation is currently allocated to. When the
      # connection cannot execute it in SQL, the expression is evaluated
      # locally against at most `fallback_limit` fetched tuples. Every
      # decision is recorded in an audit trail so materialized results can
      # be explained.
      #
      # Capability decisions never consult global state or the gateway that
      # happened to be created last - they always ask the connection of the
      # gateway owning this relation's dataset.
      #
      # @api public
      module Pushdown
        # One recorded planning decision
        #
        # @!attribute [r] feature
        #   @return [Symbol] capability the expression needs
        #
        # @!attribute [r] connection
        #   @return [Object] the real connection that was asked
        #
        # @!attribute [r] decision
        #   @return [Symbol] +:pushed_down+, +:fallback+ or +:rejected+
        #
        # @!attribute [r] relation
        #   @return [Symbol] name of the executed relation
        #
        # @!attribute [r] reason
        #   @return [Symbol, nil] why a pushdown was not possible
        #
        # @!attribute [r] fetched
        #   @return [Integer] number of tuples buffered for a fallback
        #
        # @api public
        class ExecutionEntry
          attr_reader :feature
          attr_reader :connection
          attr_reader :decision
          attr_reader :relation
          attr_reader :reason
          attr_reader :fetched

          # @api private
          def initialize(**fields)
            @feature = fields.fetch(:feature)
            @connection = fields.fetch(:connection)
            @decision = fields.fetch(:decision)
            @relation = fields.fetch(:relation)
            @reason = fields.fetch(:reason, nil)
            @fetched = fields.fetch(:fetched, 0)
            freeze
          end

          # @return [Boolean]
          #
          # @api public
          def pushed_down? = decision.equal?(:pushed_down)

          # @return [Boolean]
          #
          # @api public
          def fallback? = decision.equal?(:fallback)

          # @return [Boolean]
          #
          # @api public
          def rejected? = decision.equal?(:rejected)

          # @return [Hash]
          #
          # @api public
          def to_h
            {
              feature: feature,
              connection: connection,
              decision: decision,
              relation: relation,
              reason: reason,
              fetched: fetched
            }
          end
        end

        # Thread-local audit trail of pushdown decisions
        #
        # @api public
        module ExecutionLog
          KEY = :rom_sql_pushdown_execution_log

          # @return [Array<ExecutionEntry>]
          #
          # @api public
          def self.entries
            Thread.current[KEY] ||= []
          end

          # @param [ExecutionEntry] entry
          #
          # @return [Array<ExecutionEntry>]
          #
          # @api public
          def self.record(entry)
            entries << entry
          end

          # Run a block with an isolated audit trail
          #
          # @example
          #   entries = ROM::SQL::Relation::Pushdown::ExecutionLog.recording do
          #     users.with_sql_pushdown(:window_functions).to_a
          #   end
          #
          # @yield block that executes relation expressions
          #
          # @return [Array<ExecutionEntry>] decisions recorded while yielding,
          #   including entries written before a block raised an error
          #
          # @api public
          def self.recording
            previous = Thread.current[KEY]
            current = []
            Thread.current[KEY] = current
            yield
            current.dup
          ensure
            # Preserve recorded entries even when the block raised, keeping
            # them on the current thread for later inspection
            Thread.current[KEY] = previous.nil? ? current : previous.concat(current)
          end

          # Forget all decisions recorded on this thread
          #
          # @return [Array<ExecutionEntry>] the removed entries
          #
          # @api public
          def self.clear
            entries.dup
          ensure
            Thread.current[KEY] = []
          end
        end

        # Default cap for local fallback buffering
        DEFAULT_FALLBACK_LIMIT = 1000

        # Return a copy of the relation bound to a pushdown plan
        #
        # The plan is evaluated lazily on materialization, it does not run
        # any query by itself.
        #
        # @example push an expression down when the connection supports it
        #   users.with_sql_pushdown(:window_functions) { |r|
        #     r.select_append(r[:age].row_number.over(partition: :name))
        #   }
        #
        # @example allow a bounded in-memory fallback
        #   users.with_sql_pushdown(:window_functions, fallback_limit: 500) { |r| ... }
        #
        # @example forbid fallback entirely
        #   users.with_sql_pushdown(:window_functions, fallback_limit: 0) { |r| ... }
        #
        # @param [Symbol] feature SQL capability the expression needs
        # @param [Integer] fallback_limit max tuples buffered when the
        #   connection lacks the feature; use 0 to forbid fallback
        # @yieldparam relation [SQL::Relation] the current relation
        # @yieldreturn [SQL::Relation] the expression to evaluate
        #
        # @return [SQL::Relation]
        #
        # @api public
        def with_sql_pushdown(feature, fallback_limit: DEFAULT_FALLBACK_LIMIT, &expression)
          new(dataset, pushdown_plan: Plan.new(
            feature: feature,
            fallback_limit: fallback_limit,
            expression: expression,
            relation_name: name.relation
          ))
        end

        # Materialize the relation honoring its pushdown plan, if any
        #
        # Without a plan this behaves exactly like the regular relation.
        # With a plan the real connection of the owning gateway is asked
        # first; unsupported features fall back to a bounded in-memory
        # evaluation or raise {FallbackExceededError}.
        #
        # @return [Array<Hash>]
        #
        # @raise [FallbackExceededError] when fallback is disabled or the
        #   buffered tuples would exceed the declared limit
        #
        # @api public
        def to_a
          return super unless options[:pushdown_plan]

          options[:pushdown_plan].call(self)
        end

        # The gateway this relation's dataset is physically bound to
        #
        # Capability checks use this object's connection rather than any
        # gateway created later in the same process.
        #
        # @return [SQL::Gateway]
        #
        # @api public
        def source_gateway
          dataset.db.rom_sql_gateway
        end

        # A single, reusable pushdown declaration
        #
        # @api private
        class Plan
          attr_reader :feature
          attr_reader :fallback_limit
          attr_reader :expression
          attr_reader :relation_name

          def initialize(feature:, fallback_limit:, expression:, relation_name:)
            @feature = feature
            @fallback_limit = fallback_limit
            @expression = expression
            @relation_name = relation_name
            freeze
          end

          # @return [Array<Hash>]
          #
          # @api private
          def call(relation)
            gateway = relation.source_gateway
            connection = gateway.connection
            base = plan_free_base(relation)

            if gateway.supports_sql_feature?(feature)
              record(connection, :pushed_down)
              expression.call(base).to_a
            elsif fallback_limit.to_i.zero?
              record_rejection(connection, :fallback_disabled, 0)
              raise FallbackExceededError, rejection_message(relation, 0)
            else
              run_fallback(base, connection)
            end
          end

          private

          # Build a copy of the relation without this plan attached, so the
          # expression is evaluated from a clean base and plans nested
          # inside it are judged on their own merits.
          #
          # @api private
          def plan_free_base(relation)
            relation.class.new(relation.dataset, **relation.options.except(:pushdown_plan))
          end

          # @api private
          def run_fallback(base, connection)
            planned = expression.call(base)
            buffered = buffer_source_tuples(planned)

            if buffered.size > fallback_limit
              record_rejection(connection, :fallback_limit_exceeded, buffered.size)
              raise FallbackExceededError, rejection_message(base, buffered.size)
            end

            tuples = LocalEvaluation.new(planned, buffered).call
            record(connection, :fallback, fetched: tuples.size)
            tuples
          end

          # @api private
          def buffer_source_tuples(planned)
            from = Array(planned.dataset.opts[:from])

            if from.empty?
              # Datasets without a table source (e.g. raw SQL reads) cannot
              # be re-scoped; evaluate them directly and enforce the limit
              return planned.dataset.limit(fallback_limit + 1).to_a
            end

            planned.dataset.db[*from].limit(fallback_limit + 1).to_a
          end

          # @api private
          def record_rejection(connection, reason, fetched)
            record(connection, :rejected, reason: reason, fetched: fetched)
          end

          # @api private
          def record(connection, decision, reason: nil, fetched: 0)
            ExecutionLog.record(
              ExecutionEntry.new(
                feature: feature,
                connection: connection,
                decision: decision,
                relation: relation_name,
                reason: reason,
                fetched: fetched
              )
            )
          end

          # @api private
          def rejection_message(relation, size)
            "#{relation.name.relation} expression needs #{feature.inspect} but the " \
              "connection of gateway #{relation.source_gateway.database_type.inspect} " \
              'does not support it and the local fallback limit ' \
              "(#{fallback_limit}) was exceeded by #{size} buffered tuples"
          end
        end

        # Re-evaluates the clauses already declared on a planned dataset
        # against tuples fetched from the real connection, preserving the
        # selected conditions, ordering, limit/offset and projection
        # without pulling an unbounded result set.
        #
        # @api private
        class LocalEvaluation
          # @return [SQL::Relation]
          attr_reader :planned

          # @return [Array<Hash>]
          attr_reader :rows

          # @api private
          def initialize(planned, rows)
            @planned = planned
            @rows = rows
          end

          # @return [Array<Hash>]
          #
          # @api private
          def call
            apply_clauses(rows).map { |tuple| planned.output_schema[tuple] }
          end

          private

          # @api private
          def apply_clauses(tuples)
            tuples = apply_restrictions(tuples)
            tuples = apply_pagination(tuples)
            apply_projection(tuples)
          end

          # @api private
          def apply_restrictions(tuples)
            tuples = filter(tuples, dataset.opts[:where]) if dataset.opts.key?(:where)
            tuples = sort(tuples, dataset.opts[:order]) if dataset.opts.key?(:order)
            tuples
          end

          # @api private
          def apply_pagination(tuples)
            apply_limit(apply_offset(tuples))
          end

          # @api private
          def apply_projection(tuples)
            return tuples unless dataset.opts.key?(:select)

            project(tuples, dataset.opts[:select])
          end

          # @api private
          def apply_offset(tuples)
            return tuples unless dataset.opts.key?(:offset)

            tuples.drop(dataset.opts[:offset].to_i)
          end

          # @api private
          def apply_limit(tuples)
            return tuples unless bounded_limit?

            tuples.take(dataset.opts[:limit])
          end

          # @api private
          def dataset
            planned.dataset
          end

          # @api private
          def bounded_limit?
            dataset.opts.key?(:limit) && !dataset.opts[:limit].nil?
          end

          # @api private
          def filter(rows, expression)
            case expression
            when ::Hash
              filter_hash(rows, expression)
            when ROM::SQL::Attribute, ::Sequel::SQL::BooleanExpression
              predicate = TuplePredicate.new(expression)
              rows.select { |tuple| predicate.matches?(tuple) }
            else
              rows
            end
          end

          # @api private
          def filter_hash(rows, conditions)
            rows.select do |tuple|
              conditions.all? { |column, value| tuple[column_name(column)] == value }
            end
          end

          # @api private
          def sort(rows, order)
            order.reverse.reduce(rows) do |memo, clause|
              column, descending = order_clause(clause)
              sorted = memo.sort_by { |tuple| tuple[column] }
              descending ? sorted.reverse : sorted
            end
          end

          # @api private
          def order_clause(clause)
            case clause
            when ::Sequel::SQL::OrderedExpression
              [column_name(clause.expression), clause.descending]
            else
              [column_name(clause), false]
            end
          end

          # @api private
          def project(rows, select)
            columns = select.map { |column| column_name(column) }
            rows.map { |tuple| tuple.slice(*columns) }
          end

          # @api private
          def column_name(column)
            return column.name if column.respond_to?(:name) && column.name.is_a?(Symbol)
            return column.column if column.respond_to?(:column)

            column
          end
        end

        # Evaluates Sequel boolean expressions against individual tuples,
        # used to re-apply WHERE clauses during bounded local fallback
        #
        # @api private
        class TuplePredicate
          COMPARISONS = {
            :'=' => :==,
            :'!=' => :!=,
            :> => :>,
            :< => :<,
            :>= => :>=,
            :<= => :<=,
            :IS => :==
          }.freeze

          # @api private
          def initialize(expression)
            @expression = expression
          end

          # @return [Boolean]
          #
          # @api private
          def matches?(tuple)
            evaluate(tuple, unwrap(@expression))
          end

          private

          # @api private
          def unwrap(expression)
            if expression.is_a?(ROM::SQL::Attribute)
              wrapped = expression.meta[:sql_expr]
              return wrapped if wrapped.is_a?(::Sequel::SQL::Expression)
            end

            expression
          end

          # @api private
          def evaluate(tuple, expression)
            case expression.op
            when :AND then expression.args.all? { |arg| evaluate(tuple, unwrap(arg)) }
            when :OR then expression.args.any? { |arg| evaluate(tuple, unwrap(arg)) }
            when :IN then includes?(tuple, expression)
            when :LIKE then like?(tuple, expression)
            else compare(tuple, expression)
            end
          end

          # @api private
          def includes?(tuple, expression)
            left, right = operands(tuple, expression)
            Array(right).include?(left)
          end

          # @api private
          def like?(tuple, expression)
            value, pattern = operands(tuple, expression)
            return false if value.nil?

            regexp = Regexp.new(
              "\\A#{Regexp.escape(pattern.to_s).gsub("%", ".*").gsub("_", ".")}\\z"
            )
            regexp.match?(value.to_s)
          end

          # @api private
          def compare(tuple, expression)
            method_name = COMPARISONS.fetch(expression.op) do
              raise UnsupportedSQLError,
                    "cannot evaluate #{expression.op.inspect} during local fallback"
            end
            left, right = operands(tuple, expression)

            if expression.op.equal?(:IS)
              left.nil? ? right.nil? : left.public_send(method_name, right)
            else
              left.public_send(method_name, right)
            end
          end

          # @api private
          def operands(tuple, expression)
            [
              resolve(tuple, expression.args[0]),
              resolve(tuple, expression.args[1])
            ]
          end

          # @api private
          def resolve(tuple, value)
            case value
            when ROM::SQL::Attribute then tuple[value.name]
            when ::Sequel::SQL::QualifiedIdentifier, ::Sequel::SQL::Identifier
              tuple[value.column]
            when Symbol then tuple[value]
            else value
            end
          end
        end
      end
    end
  end
end
