# frozen_string_literal: true

require "dry/struct"

require "rom/sql/types"
require "rom/sql/migration/checkpoint_runner"

module ROM
  module SQL
    module Migration
      # A single executable statement extracted from a `.sql` migration
      # section, paired with the source line where it begins. The line is
      # used to annotate backtraces when execution fails.
      #
      # @api private
      Statement = Data.define(:sql, :line)

      # The verification boundary declared with
      # `-- @migrate checkpoint verify="..."`. A checkpoint completes a
      # step: its `verify` query must return at least one row for the
      # preceding step to be treated as safely repeatable on resume.
      #
      # @api private
      Checkpoint = Data.define(:verify, :line)

      # A resumable unit of a migration: one or more statements executed
      # outside any transaction, optionally followed by a checkpoint whose
      # verification guards resumption. Steps are numbered from zero in
      # source order, independently for each direction.
      #
      # @api private
      Step = Data.define(:index, :statements, :checkpoint) do
        # @return [Boolean] whether the step declares a verification query
        #
        # @api private
        def checkpoint? = !checkpoint.nil?
      end

      # A migration translated from a `.sql` file by `SQLParser`.
      #
      # @api private
      class SQLMigration < Dry::Struct
        Filename   = Types::Coercible::String
        Settings   = Types::Strict::Hash.default { {} }
        Direction  = Types::Strict::Symbol.enum(:up, :down)
        Statements = Types::Array.of(Types.Instance(Statement))
        Steps      = Types::Array.of(Types.Instance(Step))

        # Matches `${NAME}` substitution references in statement bodies when the
        # `env=true` pragma is set. Only the braced form is recognized, leaving
        # `$$`-quoted bodies and `$1` positional params untouched.
        SUBSTITUTION = /\$\{(?<name>[A-Za-z_]\w*)\}/

        attribute :filename, Filename
        attribute :settings, Settings

        # Flat statement lists retained for directions without checkpoints
        # and for backwards-compatible access in tests.
        attribute :up, Statements
        attribute? :down, Statements

        # Per-direction resumable steps, present only when the direction
        # declares at least one checkpoint.
        attribute? :steps, Types::Strict::Hash.map(Direction, Steps).default { {} }

        # Return a copy with additional settings merged in.
        #
        # @api private
        def with(**settings)
          new(settings: self.settings.merge(settings))
        end

        # Whether to run inside a transaction.
        #
        # @return [true, false, nil] nil leaves the choice to the database default
        #
        # @api private
        def use_transactions
          settings[:transaction]
        end

        # Whether any direction declares a verifiable step boundary.
        #
        # @api private
        def checkpoints?
          steps.values.flatten.any?(&:checkpoint?)
        end

        # Execute the statements for the given direction against the database.
        #
        # A direction with checkpoints runs step-by-step through
        # {CheckpointRunner} so an interrupted, non-transactional migration
        # can resume from the first step whose verification does not hold.
        # Other directions execute their statements directly, preserving
        # the original single-call behavior.
        #
        # @param db [Sequel::Database]
        # @param direction [:up, :down]
        #
        # @api private
        def apply(db, direction)
          unless Direction.valid?(direction)
            raise ArgumentError, "Invalid migration direction specified (#{direction.inspect})"
          end

          return if public_send(direction).nil? && steps[direction].nil?

          if steps[direction]
            CheckpointRunner.new(db).(self, direction)
          else
            apply_statements(db, direction)
          end
        end

        # Re-raise a database error annotated with the failing statement's
        # source location. Used both by the direct path and by
        # {CheckpointRunner}.
        #
        # @api private
        def annotate_error(error, statement, direction)
          frame = "#{filename}:#{statement.line}:in '#{direction}'"
          trace = error.backtrace || []
          error.set_backtrace([frame, *trace])
          error
        end

        # Apply environment-variable substitution to a statement (or any
        # SQL fragment) according to the `env` pragma. Without the pragma
        # the input is returned unchanged. Shared by the direct execution
        # path and {CheckpointRunner}, so both honor `${NAME}` identically.
        #
        # @api private
        def resolve_sql(sql, line)
          return sql unless settings[:env]

          sql.gsub(SUBSTITUTION) do
            name = Regexp.last_match[:name]
            ENV.fetch(name) do
              raise SQLEnvError,
                    "Undefined environment variable ${#{name}} in #{filename}:#{line}"
            end
          end
        end

        private

        # @api private
        def apply_statements(db, direction)
          statements = public_send(direction) or return

          # Resolve every statement before running any, so a missing environment
          # variable aborts before producing side effects.
          finalized = statements.map { |statement| apply_env(statement) }

          finalized.each do |statement|
            db.run(statement.sql)
          rescue Sequel::DatabaseError => e
            raise annotate_error(e, statement, direction)
          end
        end

        # Return the statement with `${NAME}` environment-variable substitution
        # applied to its SQL when the `env` pragma is set. Raises `SQLEnvError`
        # for a referenced variable that is not present in `ENV`.
        #
        # @api private
        def apply_env(statement)
          statement.with(sql: resolve_sql(statement.sql, statement.line))
        end
      end
    end
  end
end
