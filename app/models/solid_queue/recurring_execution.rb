# frozen_string_literal: true

module SolidQueue
  class RecurringExecution < Execution
    class AlreadyRecorded < StandardError; end

    # A folded execution is a single row standing for a run of consecutive
    # occurrences: +run_at+ is the first one, +folded_through+ the last, and
    # +folded_interval+ the number of seconds between them. Keeping the
    # arithmetic progression lets us answer "did this occurrence already run?"
    # exactly, without one row per occurrence, and without ever claiming the
    # occurrences we never saw (holes) as run.
    #
    # Rows are detached rather than deleted when their job goes away (+job_id+
    # becomes NULL), so the dedupe evidence survives finished-job cleanup.
    # Detaching itself goes through +update_all+ or the foreign key, both of
    # which skip validation, so the inherited required +belongs_to :job+ still
    # guards every row Active Record creates — and those always have a job.
    scope :clearable, -> { where.missing(:job) }
    scope :folded, -> { where.not(folded_through: nil) }

    class << self
      # The folding columns ship as an optional migration in Solid Queue 1.x and
      # become part of the base schema in 2.0. Until an existing installation
      # runs it, cleanup and job detachment keep the pre-folding behaviour.
      def supports_folding?
        if @supports_folding.nil?
          @supports_folding = column_names.include?("folded_through") && column_names.include?("folded_interval")
        end
        @supports_folding
      end

      def detachable?
        supports_folding? && columns_hash["job_id"]&.null
      end

      def reset_supports_folding!
        @supports_folding = nil
      end

      def create_or_insert!(**attributes)
        if supports_insert_conflict_target?
          # PostgreSQL fails and aborts the current transaction when it hits a duplicate key conflict
          # during two concurrent INSERTs for the same value of an unique index. We need to explicitly
          # indicate unique_by to ignore duplicate rows by this value when inserting
          unless insert(attributes, unique_by: [ :task_key, :run_at ]).any?
            raise AlreadyRecorded
          end
        else
          create!(**attributes)
        end
      rescue ActiveRecord::RecordNotUnique
        raise AlreadyRecorded
      end

      # Records +run_at+ for +task_key+ in the same transaction that enqueues the
      # job. Returns the enqueued Active Job, or +false+ when this occurrence is
      # already accounted for — exactly or through a folded run that compacted
      # its row away — which is what stops a scheduler from sending an
      # already-run instant a second time after its history has been folded.
      #
      # Two schedulers reaching the same hole at once both get past the check,
      # but only one insert wins the unique index on (task_key, run_at); the
      # loser's transaction rolls back its job here, so exactly one new job is
      # enqueued.
      def record(task_key, run_at, &block)
        transaction do
          raise AlreadyRecorded if recorded?(task_key, run_at)

          block.call.tap do |active_job|
            if active_job && active_job.successfully_enqueued?
              create_or_insert!(job_id: active_job.provider_job_id, task_key: task_key, run_at: run_at)
            end
          end
        end
      rescue AlreadyRecorded
        false
      end

      # Whether +run_at+ is already accounted for: an exact row exists, or a
      # folded run covers that instant.
      def recorded?(task_key, run_at)
        where(task_key: task_key, run_at: run_at).exists? || folded_run_covers?(task_key, run_at)
      end

      def folded_run_covers?(task_key, run_at)
        return false unless supports_folding?

        folded.where(task_key: task_key)
          .where(run_at: ..run_at, folded_through: run_at..)
          .any? { |execution| execution.covers?(run_at) }
      end

      # Compacts old, job-less history into folded runs, keeping the evidence
      # needed to reject duplicates while leaving holes recordable. Rows younger
      # than +retain+ stay one per occurrence, so a cleanup never seals a hole
      # that might still be backfilled. Pass +retain: nil+ to fold every age.
      #
      # Work is bounded: rows are read and merged a page at a time, so a task
      # with years of minutely history doesn't have to fit in memory, and each
      # page commits on its own.
      def clear_in_batches(batch_size: 500, retain: SolidQueue.clear_recurring_executions_after)
        if supports_folding?
          cutoff = retain && Time.current - retain
          foldable_task_keys(cutoff).each do |task_key|
            fold_task(task_key, cutoff: cutoff, batch_size: batch_size)
          end
        else
          loop do
            records_deleted = clearable.limit(batch_size).delete_all
            break if records_deleted == 0
          end
        end
      end

      def foldable_task_keys(cutoff)
        scope = clearable
        scope = scope.where(run_at: ...cutoff) if cutoff
        scope.distinct.pluck(:task_key)
      end

      private
        Unit = Struct.new(:row, :start, :finish, :interval, :absorbed, keyword_init: true)

        # Walks one task's old, job-less history in run_at order and folds
        # consecutive occurrences with constant spacing into one row.
        #
        # Whether two rows belong to the same run is only known once a third
        # one confirms the spacing, so a second row is held provisionally
        # (+pending+) until that's decided. The open run and the provisional row
        # carry across page boundaries, so a run is a single row no matter how
        # long its history is, while each page commits what it merged.
        def fold_task(task_key, cutoff:, batch_size:)
          cursor = nil
          run = nil
          pending = nil

          loop do
            rows = old_rows(task_key, cutoff).order(:run_at, :id).limit(batch_size)
            rows = rows.where(after_cursor(cursor)) if cursor
            rows = rows.to_a
            break if rows.empty?

            rows.each do |row|
              next_run, next_pending = fold_unit(run, pending, unit_for(row))

              # A run that gets replaced is complete: commit the rows it absorbed.
              flush(run) if run && !run.equal?(next_run)

              run, pending = next_run, next_pending
            end

            flush(run)

            cursor = [ rows.last.run_at, rows.last.id ]
            break if rows.size < batch_size
          end

          flush(run)
        end

        def old_rows(task_key, cutoff)
          scope = clearable.where(task_key: task_key)
          scope = scope.where(run_at: ...cutoff) if cutoff
          scope
        end

        # Keyset pagination over (run_at, id): a page can absorb the rows after
        # it, so the cursor must be a pair of values, not a row reference.
        def after_cursor((run_at, id))
          table = arel_table
          table[:run_at].gt(run_at).or(table[:run_at].eq(run_at).and(table[:id].gt(id)))
        end

        def unit_for(row)
          Unit.new(row: row, start: row.run_at, finish: row.folded_through || row.run_at, interval: row.folded_interval, absorbed: [])
        end

        # Returns the [run, pending] state after considering +unit+.
        #
        # +run+ is the run being built: its +finish+ is the last row it has
        # confirmed and, once +interval+ is set, it also knows the spacing it
        # must keep. +pending+ is a lone row whose spacing to the run isn't
        # confirmed yet; a third row that matches decides it either joins the
        # run or becomes a run of its own.
        def fold_unit(run, pending, unit)
          return [ unit, nil ] if run.nil?

          if run.interval.nil?
            if pending
              # Three points in a row: the spacing is only a run if the gap
              # before and after the provisional row agree, and neither of the
              # rows being joined already knows a different spacing.
              first_gap = pending.start.to_i - run.finish.to_i
              second_gap = unit.start.to_i - pending.finish.to_i
              agrees = [ run.interval, pending.interval, unit.interval ].compact.all? { |interval| interval == first_gap }

              if first_gap > 0 && first_gap == second_gap && agrees
                run.interval = first_gap
                absorb(run, pending)
                absorb(run, unit)
                [ run, nil ]
              else
                # The second row started a run of its own; keep the third on hold.
                [ pending, unit ]
              end
            else
              gap = unit.start.to_i - run.finish.to_i

              if gap > 0 && unit.interval == gap
                run.interval = gap
                absorb(run, unit)
                [ run, nil ]
              else
                [ run, unit ]
              end
            end
          elsif (gap = unit.start.to_i - run.finish.to_i) == run.interval &&
              (unit.interval.nil? || unit.interval == run.interval)
            absorb(run, unit)
            [ run, nil ]
          else
            [ unit, nil ]
          end
        end

        def absorb(run, unit)
          run.finish = unit.finish
          run.absorbed.concat([ unit ]).concat(unit.absorbed)
        end

        # Writes the run's extent onto its first row and drops the rows it
        # absorbed, in one transaction so the run is never half-applied. A run
        # that never absorbed anything stays a plain single row.
        def flush(run)
          return if run.nil? || run.absorbed.empty?

          absorbed_ids = run.absorbed.map { |absorbed| absorbed.row.id }

          transaction do
            run.row.update_columns(folded_through: run.finish, folded_interval: run.interval)
            where(id: absorbed_ids).delete_all
          end

          run.absorbed.clear
        end
    end

    def folded?
      self.class.supports_folding? && folded_through.present?
    end

    # Whether this row accounts for +time+: the exact +run_at+ for a plain row,
    # or any occurrence on the folded run's arithmetic progression.
    def covers?(time)
      return run_at.to_i == time.to_i unless folded?
      return run_at.to_i == time.to_i if folded_interval.to_i <= 0

      time >= run_at && time <= folded_through &&
        (time.to_i - run_at.to_i) % folded_interval.to_i == 0
    end
  end
end
