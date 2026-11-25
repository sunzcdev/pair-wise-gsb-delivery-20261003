# frozen_string_literal: true

require "dry/struct"

require "rom/sql/errors"
require "rom/sql/types"

module ROM
  module SQL
    module Migration
      # A single executable statement extracted from a `.sql` migration
      # section, paired with the source position where it begins. `line` is
      # a 1-based file line; `column` is the 1-based column of the first
      # non-whitespace character of the statement; `pos` is the matching
      # 0-based intra-line offset. The location is used to annotate
      # backtraces when execution fails.
      #
      # @api private
      Statement = Data.define(:sql, :line, :column) do
        # Accepts the historical `Statement.new(sql, line)` / keyword form as
        # well as an explicit `column`. Column defaults to 1 (first column).
        def initialize(*args, **kwargs)
          if args.any?
            sql, line, column = args
            super(sql: sql, line: line, column: column || 1)
          else
            kwargs[:column] = 1 unless kwargs.key?(:column)
            super(**kwargs)
          end
        end

        # 0-based offset of the statement start on its source line.
        #
        # @api private
        def pos = column - 1
      end

      # A migration translated from a `.sql` file by `SQLParser`.
      #
      # @api private
      class SQLMigration < Dry::Struct
        Filename   = Types::Coercible::String
        Settings   = Types::Strict::Hash.default { {} }
        Direction  = Types::Strict::Symbol.enum(:up, :down)
        Statements = Types::Array.of(Types.Instance(Statement))

        # Matches `${NAME}` substitution references in statement bodies when the
        # `env=true` pragma is set. Only the braced form is recognized, leaving
        # `$$`-quoted bodies and `$1` positional params untouched.
        SUBSTITUTION = /\$\{(?<name>[A-Za-z_]\w*)\}/

        attribute :filename, Filename
        attribute :settings, Settings

        attribute :up, Statements
        attribute? :down, Statements

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

        # Execute the statements for the given direction against the database.
        #
        # @param db [Sequel::Database]
        # @param direction [:up, :down]
        #
        # @api private
        def apply(db, direction)
          unless Direction.valid?(direction)
            raise ArgumentError, "Invalid migration direction specified (#{direction.inspect})"
          end

          statements = public_send(direction) or return

          # Resolve every statement before running any, so a missing environment
          # variable aborts before producing side effects.
          finalized = statements.map { |statement| apply_env(statement) }

          finalized.each do |statement|
            db.run(statement.sql)
          rescue Sequel::DatabaseError => e
            raise annotate(e, statement, direction)
          end
        end

        private

        # Return the statement with `${NAME}` environment-variable substitution
        # applied to its SQL when the `env` pragma is set. Raises `SQLEnvError`
        # for a referenced variable that is not present in `ENV`.
        #
        # @api private
        def apply_env(statement)
          return statement unless settings[:env]

          sql = statement.sql.gsub(SUBSTITUTION) do
            name = Regexp.last_match[:name]
            ENV.fetch(name) do
              raise SQLEnvError,
                    "Undefined environment variable ${#{name}} in " \
                    "#{filename}:#{statement.line}:#{statement.column}"
            end
          end

          statement.with(sql:)
        end

        # Prepend a synthetic frame pointing at the failing statement's source
        # location (`file:line:column:in 'direction'`), so the migration file
        # appears at the top of the backtrace. The same coordinates are also
        # exposed structurally on the error via `source_location`,
        # `source_file`, `source_line`, `source_column` and `source_pos`.
        #
        # @api private
        def annotate(error, statement, direction)
          frame = "#{filename}:#{statement.line}:#{statement.column}:in '#{direction}'"
          trace = error.backtrace || []
          error.set_backtrace([frame, *trace])

          location = [filename, statement.line, statement.column].freeze
          error.define_singleton_method(:source_location) { location }
          error.define_singleton_method(:source_file) { filename }
          error.define_singleton_method(:source_line) { statement.line }
          error.define_singleton_method(:source_column) { statement.column }
          error.define_singleton_method(:source_pos) { statement.pos }

          error
        end
      end
    end
  end
end
