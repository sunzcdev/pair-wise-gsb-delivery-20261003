# frozen_string_literal: true

require "digest"

require "rom/initializer"
require "rom/sql/migration/sql_migration"

module ROM
  module SQL
    module Migration
      # Runs a non-transactional SQL migration as a sequence of verifiable
      # steps, persisting progress so an interrupted migration can resume by
      # re-executing only the statements that did not complete.
      #
      # Progress is tracked at statement granularity: every successfully
      # executed statement gets its own checkpoint row, so a failure partway
      # through a multi-statement step never replays its predecessors. A
      # step's optional verification query is evaluated on resume before the
      # step is treated as safely skippable, proving the target state still
      # matches what the recorded actions claim.
      #
      # The persisted checksum binds progress rows to the exact file content
      # that produced them, so a migration edited after a failure cannot be
      # resumed silently.
      #
      # @api private
      class CheckpointRunner
        # The table holding resumption state for non-transactional SQL
        # migrations. It is independent of Sequel's `schema_migrations`
        # bookkeeping, which still marks the whole migration applied.
        TABLE = :rom_sql_migration_checkpoints

        extend Initializer

        param :db

        # @param migration [SQLMigration]
        # @param direction [:up, :down]
        #
        # @api private
        def call(migration, direction)
          steps = migration.steps.fetch(direction) { return }
          return if steps.empty?

          ensure_table

          checksum = checksum_for(migration)
          guard_unchanged(migration, direction, checksum)

          steps.each do |step|
            resume_step(migration, direction, step, checksum)
          end
        end

        private

        # Resume one step. A fully recorded step is only skipped when its
        # verification query still holds. A partially executed step replays
        # just the statements whose own checkpoint rows are missing.
        #
        # @api private
        def resume_step(migration, direction, step, checksum)
          if fully_recorded?(migration, direction, step, checksum)
            verify_or_raise(migration, step)
            return
          end

          step.statements.each_with_index do |statement, offset|
            next if recorded?(migration, direction, step, offset, checksum)

            sql = migration.resolve_sql(statement.sql, statement.line)

            begin
              db.run(sql)
            rescue Sequel::DatabaseError => e
              raise migration.annotate_error(e, statement, direction)
            end

            record(migration, direction, step, offset, checksum)
          end

          # Once every statement of the step is durable, prove the combined
          # effect matches the declared target before moving on.
          verify_or_raise(migration, step)
        end

        # @api private
        def verify_or_raise(migration, step)
          checkpoint = step.checkpoint
          return unless checkpoint

          sql = migration.resolve_sql(checkpoint.verify, checkpoint.line)

          result =
            begin
              db.fetch(sql).first
            rescue Sequel::DatabaseError => e
              raise MigrationCheckpointError,
                    "Cannot resume #{migration.filename}: verification " \
                    "query for step #{step.index} failed (#{e.message}): " \
                    "#{checkpoint.verify}"
            end

          return if result

          raise MigrationCheckpointError,
                "Cannot resume #{migration.filename}: step #{step.index} " \
                "is recorded as complete but its verification query " \
                "returned no rows: #{checkpoint.verify}"
        end

        # Create the checkpoint table on first use. A schema check rather
        # than `IF NOT EXISTS` keeps the generated DDL portable across
        # SQLite, PostgreSQL and MySQL.
        #
        # @api private
        def ensure_table
          return if db.table_exists?(TABLE)

          db.create_table(TABLE) do
            String :migration, null: false
            String :direction, null: false
            Integer :step_index, null: false
            Integer :statement_offset, null: false
            String :checksum, null: false
            Time :created_at, null: false, default: Sequel::CURRENT_TIMESTAMP

            index %i[migration direction step_index statement_offset],
                  unique: true,
                  name: :rom_sql_migration_checkpoints_pk
          end
        end

        # Refuse to resume when the file on disk no longer matches the
        # content that produced the persisted progress. A migration whose
        # steps have never run has no rows and therefore nothing to guard.
        #
        # @api private
        def guard_unchanged(migration, direction, checksum)
          row = dataset
                  .where(migration: migration.filename, direction: direction.to_s)
                  .first
          return unless row
          return if row[:checksum] == checksum

          raise MigrationResumeError,
                "Cannot resume #{migration.filename}: the migration file " \
                "has changed since its interrupted run (stored checksum " \
                "#{row[:checksum]}, current checksum #{checksum}). " \
                "Resolve the partial migration manually before retrying."
        end

        # @api private
        def fully_recorded?(migration, direction, step, checksum)
          step.statements.each_with_index.all? do |_, offset|
            recorded?(migration, direction, step, offset, checksum)
          end
        end

        # @api private
        def recorded?(migration, direction, step, offset, checksum)
          dataset.where(
            migration: migration.filename,
            direction: direction.to_s,
            step_index: step.index,
            statement_offset: offset,
            checksum: checksum
          ).count.positive?
        end

        # @api private
        def record(migration, direction, step, offset, checksum)
          dataset.insert(
            migration: migration.filename,
            direction: direction.to_s,
            step_index: step.index,
            statement_offset: offset,
            checksum: checksum
          )
        end

        # @api private
        def dataset
          db[TABLE]
        end

        # Checksum the raw file content, not parsed statements: any edit
        # that changes the file invalidates resume, including edits that do
        # not alter the executed SQL (comments, directives, whitespace).
        #
        # @api private
        def checksum_for(migration)
          Digest::SHA256.file(migration.filename).hexdigest
        end
      end
    end
  end
end
