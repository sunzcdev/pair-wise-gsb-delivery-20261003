# frozen_string_literal: true

require "digest/sha2"

module ActiveRecord
  module ConnectionAdapters
    module SQLite3
      # Works out what a prepared statement depends on so the statement pool can
      # be invalidated one schema object at a time.
      #
      # SQLite silently recompiles a cached prepared statement when the schema
      # changes, but the statement keeps reporting the column names it was first
      # compiled with (sqlite3-ruby memoizes them). A cached `SELECT * FROM beta`
      # therefore returns the value of a newly added column while still
      # advertising the old column list. Rebuilding the whole pool on every
      # schema change is correct but expensive: a migration on one table would
      # also throw away every other table's statements.
      #
      # Instead, every prepared statement is compiled with SQLite's authorizer
      # installed so the tables it resolved can be recorded, together with a
      # fingerprint of each table's definition. The pool then validates a
      # statement lazily, right before it is reused:
      #
      # * if the schema versions the statement watches did not move, reuse it
      #   untouched;
      # * if they moved, compare the live fingerprints of the tables the
      #   statement reads. When they still match, the change was elsewhere and
      #   the statement is kept (only the recorded versions are refreshed);
      # * otherwise the statement is deallocated and prepared again, picking up
      #   the new column list.
      #
      # Schema versions are read through the connection, so DDL committed by a
      # different connection is noticed the next time a statement is reused.
      # DDL run inside a transaction also moves the version, and a rollback moves
      # it back; because the fingerprints are compared against the live schema,
      # a statement prepared against a rolled back schema is rebuilt again for
      # the reverted one.
      #
      # Some statements can't be attributed to particular tables: PRAGMAs,
      # reads of sqlite_master, table valued pragma functions, virtual tables
      # and anything else the authorizer reports without a followable name. They
      # are marked un-explainable and rebuilt whenever any watched schema
      # version moves, trading reuse for correctness.
      module StatementDependencies
        # https://www.sqlite.org/c3ref/c_alter_table.html
        SQLITE_CREATE_INDEX        = 1
        SQLITE_CREATE_TABLE        = 2
        SQLITE_CREATE_TEMP_INDEX   = 3
        SQLITE_CREATE_TEMP_TABLE   = 4
        SQLITE_CREATE_TEMP_TRIGGER = 5
        SQLITE_CREATE_TEMP_VIEW    = 6
        SQLITE_CREATE_TRIGGER      = 7
        SQLITE_CREATE_VIEW         = 8
        SQLITE_DELETE              = 9
        SQLITE_DROP_INDEX          = 10
        SQLITE_DROP_TABLE          = 11
        SQLITE_DROP_TEMP_INDEX     = 12
        SQLITE_DROP_TEMP_TABLE     = 13
        SQLITE_DROP_TEMP_TRIGGER   = 14
        SQLITE_DROP_TEMP_VIEW      = 15
        SQLITE_DROP_TRIGGER        = 16
        SQLITE_DROP_VIEW           = 17
        SQLITE_INSERT              = 18
        SQLITE_PRAGMA              = 19
        SQLITE_READ                = 20
        SQLITE_SELECT              = 21
        SQLITE_TRANSACTION         = 22
        SQLITE_UPDATE              = 23
        SQLITE_ATTACH              = 24
        SQLITE_DETACH              = 25
        SQLITE_ALTER_TABLE         = 26
        SQLITE_REINDEX             = 27
        SQLITE_ANALYZE             = 28
        SQLITE_CREATE_VTABLE       = 29
        SQLITE_DROP_VTABLE         = 30
        SQLITE_FUNCTION            = 31
        SQLITE_SAVEPOINT           = 32
        SQLITE_RECURSIVE           = 33

        # Actions that name a table whose definition we can follow.
        TABLE_ACTIONS = [SQLITE_READ, SQLITE_INSERT, SQLITE_UPDATE, SQLITE_DELETE].freeze

        # Actions that don't describe a schema object we could follow and don't
        # stop a statement being attributed to the tables it does read.
        NEUTRAL_ACTIONS = [
          SQLITE_SELECT, SQLITE_FUNCTION, SQLITE_RECURSIVE,
          SQLITE_TRANSACTION, SQLITE_SAVEPOINT,
        ].freeze

        # Objects whose name can't be tied to a single `sqlite_master` row: the
        # catalog itself and the `pragma_*` table valued functions.
        RESERVED_PREFIXES = %w[sqlite_ pragma_].freeze

        SCHEMAS = %w[main temp].freeze

        # An unqualified table name resolves to `temp` before `main`.
        RESOLUTION_ORDER = %w[temp main].freeze

        # What a cached statement depends on, plus the schema versions recorded
        # when it was prepared.
        class Entry # :nodoc:
          attr_reader :tables, :fingerprints
          attr_accessor :schema_versions

          def initialize(tables:, fingerprints:, schema_versions:, explainable:)
            @tables = tables
            @fingerprints = fingerprints
            @schema_versions = schema_versions
            @explainable = explainable
          end

          def explainable?
            @explainable
          end

          def inspect
            "#<#{self.class.name} tables=#{tables.inspect} explainable=#{explainable?}>"
          end
        end

        def clear_cache!(new_connection: false) # :nodoc:
          # A new raw connection has its own schema versions and needs its own
          # probe statements, so drop the old ones first.
          invalidate_schema_version_probes if new_connection
          super
        end

        private
          # Returns `[statement, rebuilt]`, where +statement+ is reset and ready
          # to bind. +rebuilt+ is true when a previously cached statement was
          # deallocated and prepared again because its dependencies changed.
          def cached_prepared_statement(raw_connection, sql)
            statement = @statements[sql]
            entry = @statements.metadata(sql)

            if statement && entry && statement_dependencies_valid?(raw_connection, entry)
              return [statement.reset!, false]
            end

            rebuilt = !statement.nil?
            @statements.delete(sql) if statement

            statement, entry = prepare_statement_with_dependencies(raw_connection, sql)
            @statements.store(sql, statement, entry)
            [statement.reset!, rebuilt]
          end

          def prepare_statement_with_dependencies(raw_connection, sql)
            # Without an authorizer the dependencies can't be attributed, but the
            # statement still has to work. Record an empty, explainable entry so
            # a non-schema-aware driver keeps reusing it.
            unless raw_connection.respond_to?(:authorizer=)
              statement = raw_connection.prepare(sql)
              return [statement, Entry.new(tables: [], fingerprints: {}, schema_versions: {}, explainable: true)]
            end

            tables = []
            explainable = true

            raw_connection.authorizer do |action, arg1, _arg2, schema, _trigger|
              if TABLE_ACTIONS.include?(action)
                # The authorizer hands back ASCII-8BIT strings, which SQLite
                # would bind as BLOBs and never match against sqlite_master.
                name = utf8(arg1)
                schema = utf8(schema)
                schema = nil if schema && schema.empty?

                if name.nil? || name.empty? || reserved_object_name?(name)
                  explainable = false
                else
                  tables << [schema, name]
                end
              elsif !NEUTRAL_ACTIONS.include?(action)
                explainable = false
              end

              ::SQLite3::Constants::ErrorCode::OK
            end

            begin
              statement = raw_connection.prepare(sql)
            ensure
              raw_connection.authorizer = nil
            end

            tables.uniq!

            [statement, build_entry(raw_connection, tables, explainable)]
          end

          def build_entry(raw_connection, tables, explainable)
            tables, resolved = resolve_table_schemas(raw_connection, tables)
            explainable &&= resolved

            objects = lookup_schema_objects(raw_connection, tables)

            if objects.any? { |_table, object| object.nil? || virtual_table?(object) }
              explainable = false
            end

            Entry.new(
              tables: tables,
              fingerprints: fingerprint_objects(objects),
              schema_versions: current_schema_versions(raw_connection, tables, explainable),
              explainable: explainable
            )
          rescue ::SQLite3::Exception
            # The statement itself prepared fine; if we can't describe what it
            # depends on, treat it as un-explainable rather than failing here.
            # An empty version map never matches the live one, so it will be
            # rebuilt next time it is used.
            Entry.new(tables: [], fingerprints: {}, schema_versions: {}, explainable: false)
          end

          # A few statements make SQLite report a read without naming the schema,
          # even though it does name the table. Unqualified names resolve to
          # `temp` first and then `main`, so use the catalog to pin the schema
          # down. When the name can't be found anywhere the statement is marked
          # as not fully resolved and handled conservatively.
          def resolve_table_schemas(raw_connection, tables)
            resolved = true

            resolved_tables = tables.map do |schema, name|
              next [schema, name] if schema

              found = RESOLUTION_ORDER.find { |candidate| schema_object(raw_connection, candidate, name) }

              if found
                [found, name]
              else
                resolved = false
                [schema, name]
              end
            end

            [resolved_tables.uniq, resolved]
          end

          def statement_dependencies_valid?(raw_connection, entry)
            versions = current_schema_versions(raw_connection, entry.tables, entry.explainable?)
            return true if versions == entry.schema_versions

            # A version we couldn't read tells us nothing, so rebuild to be safe.
            return false if versions.value?(nil)

            # The schema moved. If the statement couldn't be attributed to
            # particular tables we can't tell whether it is affected, so rebuild.
            return false unless entry.explainable?

            live = fingerprint_objects(lookup_schema_objects(raw_connection, entry.tables))

            if live == entry.fingerprints
              # Only unrelated objects changed; keep the statement and move the
              # recorded schema versions forward.
              entry.schema_versions = versions
              true
            else
              false
            end
          end

          # The schema versions a statement has to watch. An explainable
          # statement only watches the schemas of the tables it reads, and one
          # that reads no tables watches nothing at all. An un-explainable
          # statement has to watch every schema.
          def current_schema_versions(raw_connection, tables, explainable)
            schemas = explainable ? tables.map(&:first).compact.uniq : SCHEMAS

            schemas.each_with_object({}) do |schema, versions|
              versions[schema] = read_schema_version(raw_connection, schema)
            end
          end

          # Reads a schema version through a statement prepared once per raw
          # connection. Preparing a throwaway statement on every check would
          # cost roughly as much as re-running the query being validated.
          def read_schema_version(raw_connection, schema)
            statement = schema_version_probe(raw_connection, schema)
            statement.reset!

            row = statement.step
            statement.step until statement.done?

            row.is_a?(Hash) ? row.values.first : row&.first
          rescue ::SQLite3::Exception
            nil
          end

          def schema_version_probe(raw_connection, schema)
            probes = schema_version_probes

            if probes[:connection] && !probes[:connection].equal?(raw_connection)
              invalidate_schema_version_probes
              probes = schema_version_probes
            end

            probes[:connection] = raw_connection
            probes[schema] ||= raw_connection.prepare("PRAGMA #{schema}.schema_version")
          end

          # Keyed by pid like the statement pool itself, so a forked child
          # doesn't try to reuse its parent's statements.
          def schema_version_probes
            @schema_version_probes ||= {}
            @schema_version_probes[Process.pid] ||= {}
          end

          def invalidate_schema_version_probes
            probes = schema_version_probes
            return if probes.empty?

            probes.each do |key, value|
              next if key == :connection
              value.close unless value.closed?
            rescue ::SQLite3::Exception
            end

            probes.clear
          end

          # Looks the given `[schema, table]` pairs up in the catalog so their
          # definitions can be fingerprinted. Pairs whose schema could not be
          # resolved are left out and simply have no fingerprint.
          def lookup_schema_objects(raw_connection, tables)
            tables.reject { |schema, _name| schema.nil? }
              .group_by(&:first).each_with_object({}) do |(schema, pairs), objects|
                names = pairs.map(&:last)
                placeholders = Array.new(names.size, "?").join(", ")

                rows = raw_connection.execute(
                  "SELECT name, type, sql FROM #{schema}.sqlite_master WHERE name IN (#{placeholders})",
                  names
                )
                found = rows.to_h { |row| [row["name"], [row["type"], row["sql"]]] }

                pairs.each { |table| objects[table] = found[table.last] }
              end
          end

          def schema_object(raw_connection, schema, name)
            raw_connection.execute(
              "SELECT name, type, sql FROM #{schema}.sqlite_master WHERE name = ?", [name]
            ).first
          end

          def fingerprint_objects(objects)
            objects.transform_values { |object| object && digest_schema_object(object) }
          end

          def digest_schema_object(object)
            ::Digest::SHA256.hexdigest(object.map(&:to_s).join("\0"))
          end

          def virtual_table?(object)
            object[0] == "table" && object[1].to_s.lstrip.start_with?("CREATE VIRTUAL")
          end

          def reserved_object_name?(name)
            name.start_with?(*RESERVED_PREFIXES)
          end

          # The authorizer hands back ASCII-8BIT strings, which SQLite would
          # bind as BLOBs and never match against sqlite_master.
          def utf8(value)
            value && value.dup.force_encoding(Encoding::UTF_8)
          end

          # Flags the query so a rebuild can be observed from instrumentation.
          def mark_statement_rebuilt(intent)
            intent.notification_payload[:statement_rebuilt] = true if intent.notification_payload
          end
      end
    end
  end
end
