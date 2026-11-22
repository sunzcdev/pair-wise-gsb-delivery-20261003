# frozen_string_literal: true

module ActiveRecord
  module Locking
    # What is \Optimistic \Locking
    # ----------------------------
    #
    # Optimistic locking allows multiple users to access the same record for edits, and assumes a minimum of
    # conflicts with the data. It does this by checking whether another process has made changes to a record since
    # it was opened, an ActiveRecord::StaleObjectError exception is thrown if that has occurred
    # and the update is ignored.
    #
    # Check out ActiveRecord::Locking::Pessimistic for an alternative.
    #
    # Usage
    # -----
    #
    # Active Record supports optimistic locking if the `lock_version` field is present. Each update to the
    # record increments the integer column `lock_version` and the locking facilities ensure that records instantiated twice
    # will let the last one saved raise a `StaleObjectError` if the first was also updated. Example:
    #
    # ```
    # p1 = Person.find(1)
    # p2 = Person.find(1)
    #
    # p1.first_name = "Michael"
    # p1.save
    #
    # p2.first_name = "should fail"
    # p2.save # Raises an ActiveRecord::StaleObjectError
    # ```
    #
    # Optimistic locking will also check for stale data when objects are destroyed. Example:
    #
    # ```
    # p1 = Person.find(1)
    # p2 = Person.find(1)
    #
    # p1.first_name = "Michael"
    # p1.save
    #
    # p2.destroy # Raises an ActiveRecord::StaleObjectError
    # ```
    #
    # You're then responsible for dealing with the conflict by rescuing the exception and either rolling back, merging,
    # or otherwise apply the business logic needed to resolve the conflict.
    #
    # When a save raises ActiveRecord::StaleObjectError, the exception carries
    # StaleObjectError#attempted_attributes: a hash of the attributes the failed
    # update was actually going to persist (the locking column is excluded).
    # #recover_from_stale_update uses that information to reload the record and
    # re-apply the unsaved edits, optionally letting a block decide which of
    # them should be kept:
    #
    # ```
    # p1 = Person.find(1)
    # p2 = Person.find(1)
    #
    # p1.first_name = "Michael"
    # p1.save!
    #
    # p2.first_name = "should fail"
    # begin
    #   p2.save! # Raises an ActiveRecord::StaleObjectError
    # rescue ActiveRecord::StaleObjectError => error
    #   p2.recover_from_stale_update(error) # Reloads and restores the edit
    #   p2.save!                            # Succeeds against the fresh lock_version
    # end
    # ```
    #
    # This locking mechanism will function inside a single Ruby process. To make it work across all
    # web requests, the recommended approach is to add `lock_version` as a hidden field to your form.
    #
    # This behavior can be turned off by setting `ActiveRecord::Base.lock_optimistically = false`.
    # To override the name of the `lock_version` column, set the `locking_column` class attribute:
    #
    # ```
    # class Person < ActiveRecord::Base
    #   self.locking_column = :lock_person
    # end
    # ```
    #
    module Optimistic
      extend ActiveSupport::Concern

      # Captures the `preserve_lock_version_on_touch` intent on the record at
      # `touch_later` time, since the actual touch is deferred until the
      # transaction commits — by which point the surrounding block has already
      # exited.
      module DeferredTouch # :nodoc:
        def touch_later(*)
          if Optimistic.preserving_lock_version_on_touch?
            @_skip_locking_column_on_touch = true
          end
          super
        end
      end

      included do
        class_attribute :lock_optimistically, instance_writer: false, default: true
        prepend DeferredTouch
      end

      def self.preserve_lock_version_on_touch # :nodoc:
        prior = ActiveSupport::IsolatedExecutionState[:active_record_preserve_lock_version_on_touch]
        ActiveSupport::IsolatedExecutionState[:active_record_preserve_lock_version_on_touch] = true
        yield
      ensure
        ActiveSupport::IsolatedExecutionState[:active_record_preserve_lock_version_on_touch] = prior
      end

      def self.preserving_lock_version_on_touch? # :nodoc:
        ActiveSupport::IsolatedExecutionState[:active_record_preserve_lock_version_on_touch]
      end

      def locking_enabled? # :nodoc:
        self.class.locking_enabled?
      end

      # Reloads the stale record and re-applies the attributes that a failed,
      # stale update intended to write, so the update can be retried against the
      # current database state.
      #
      # Pass the ActiveRecord::StaleObjectError raised by the conflicting save
      # (or directly its StaleObjectError#attempted_attributes). The record is
      # reloaded first, which refreshes the locking column and every loaded
      # attribute; attributes that were never loaded on the record are left
      # untouched and are never written back as +nil+.
      #
      # When a block is given, it receives the hash of the attributes the
      # failed update intended to write, and only the attributes it returns are
      # re-applied. This lets the caller keep part of the unsaved edits and
      # discard the rest:
      #
      # ```
      # p1 = Person.find(1)
      # p2 = Person.find(1)
      #
      # p1.first_name = "Michael"
      # p1.save!
      #
      # p2.first_name = "should fail"
      # p2.comments = "unsaved note"
      # begin
      #   p2.save!
      # rescue ActiveRecord::StaleObjectError => error
      #   p2.recover_from_stale_update(error) do |attempted_attributes|
      #     attempted_attributes.slice("comments")
      #   end
      #   p2.save! # => succeeds against the fresh lock_version
      # end
      # ```
      #
      # The record is returned, but it is not saved automatically: the caller
      # decides when (and whether) to persist the merged result. If the record
      # was deleted from the database in the meantime, ActiveRecord::RecordNotFound
      # is raised by #reload, just like a manual recovery would.
      def recover_from_stale_update(error_or_attributes)
        unless locking_enabled?
          raise ActiveRecordError, "#{self.class.name} does not have optimistic locking enabled."
        end

        attempted_attributes =
          if error_or_attributes.is_a?(ActiveRecord::StaleObjectError)
            error_or_attributes.attempted_attributes
          else
            error_or_attributes
          end

        reload

        attributes_to_apply = if block_given?
          yield(attempted_attributes)
        else
          attempted_attributes
        end

        attributes_to_apply.each do |name, value|
          name = resolve_alias(name.to_s)
          # Only re-apply attributes that exist on the (reloaded) record.
          # Columns that were never loaded must not be synthesized as nil.
          next unless @attributes.key?(name)

          _write_attribute(name, value)
        end

        self
      end

      def increment!(*, **) # :nodoc:
        super.tap do
          if locking_enabled?
            self[self.class.locking_column] += 1
            clear_attribute_change(self.class.locking_column)
          end
        end
      end

      def initialize_dup(other) # :nodoc:
        super
        _clear_locking_column if locking_enabled?
      end

      private
        def _create_record(attribute_names = self.attribute_names)
          if locking_enabled?
            # We always want to persist the locking version, even if we don't detect
            # a change from the default, since the database might have no default
            attribute_names |= [self.class.locking_column]
          end
          super
        end

        def _touch_row(attribute_names, time)
          if locking_enabled? && !_skip_locking_column_on_touch?
            @_touch_attr_names << self.class.locking_column
          end
          super
        ensure
          @_skip_locking_column_on_touch = nil
        end

        def _update_row(attribute_names, attempted_action = "update")
          return super unless locking_enabled?
          return super if attempted_action == "touch" && _skip_locking_column_on_touch?

          begin
            locking_column = self.class.locking_column
            lock_attribute_was = @attributes[locking_column]

            update_constraints = _query_constraints_hash

            attribute_names = attribute_names.dup if attribute_names.frozen?
            attribute_names << locking_column

            if self[locking_column].nil?
              raise(<<-MSG.squish)
                For optimistic locking, locking_column ('#{locking_column}') can't be nil.
                Are you missing a default value or validation on '#{locking_column}'?
              MSG
            end

            # Capture the values this update was actually going to persist,
            # excluding the locking column itself (which is managed by the
            # locking mechanism, not by the caller). They are exposed on the
            # StaleObjectError so the caller can recover from the conflict.
            attempted_attributes = attribute_names.without(locking_column).index_with do |name|
              _read_attribute(name)
            end

            self[locking_column] += 1

            affected_rows = self.class._update_record(
              attributes_with_values(attribute_names),
              update_constraints
            )

            if affected_rows != 1
              raise ActiveRecord::StaleObjectError.new(
                self,
                attempted_action,
                attempted_attributes: attempted_attributes
              )
            end

            affected_rows

          # If something went wrong, revert the locking_column value.
          rescue Exception
            @attributes[locking_column] = lock_attribute_was
            raise
          end
        end

        def _skip_locking_column_on_touch?
          @_skip_locking_column_on_touch || Optimistic.preserving_lock_version_on_touch?
        end

        def destroy_row
          affected_rows = super

          if locking_enabled? && affected_rows != 1
            raise ActiveRecord::StaleObjectError.new(self, "destroy")
          end

          affected_rows
        end

        def _lock_value_for_database(locking_column)
          if will_save_change_to_attribute?(locking_column)
            @attributes[locking_column].value_for_database
          else
            @attributes[locking_column].original_value_for_database
          end
        end

        def _clear_locking_column
          self[self.class.locking_column] = nil
          clear_attribute_change(self.class.locking_column)
        end

        def _query_constraints_hash
          return super unless locking_enabled?

          locking_column = self.class.locking_column
          super.merge(locking_column => _lock_value_for_database(locking_column))
        end

        module ClassMethods
          DEFAULT_LOCKING_COLUMN = "lock_version"

          # Returns true if the `lock_optimistically` flag is set to true
          # (which it is, by default) and the table includes the
          # `locking_column` column (defaults to `lock_version`).
          def locking_enabled?
            lock_optimistically && columns_hash[locking_column]
          end

          # Set the column to use for optimistic locking. Defaults to `lock_version`.
          def locking_column=(value)
            reload_schema_from_cache
            @locking_column = value.to_s
          end

          # The version column used for optimistic locking. Defaults to `lock_version`.
          attr_reader :locking_column

          # Reset the column used for optimistic locking back to the `lock_version` default.
          def reset_locking_column
            self.locking_column = DEFAULT_LOCKING_COLUMN
          end

          # Make sure the lock version column gets updated when counters are
          # updated.
          def update_counters(id, counters)
            counters = counters.merge(locking_column => 1) if locking_enabled?
            super
          end

          private
            def hook_attribute_type(name, cast_type)
              if lock_optimistically && name == locking_column
                cast_type = LockingType.new(cast_type)
              end

              super
            end

            def inherited(base)
              super
              base.class_eval do
                @locking_column = DEFAULT_LOCKING_COLUMN
              end
            end
        end
    end

    # In de/serialize we change `nil` to 0, so that we can allow passing
    # `nil` values to `lock_version`, and not result in `ActiveRecord::StaleObjectError`
    # during update record.
    class LockingType < ActiveSupport::Delegation::DelegateClass(Type::Value) # :nodoc:
      include Type::QueryPredicates::Decorator

      def self.new(subtype)
        self === subtype ? subtype : super
      end

      def subtype
        __getobj__
      end

      def deserialize(value)
        super.to_i
      end

      def serialize(value)
        super.to_i
      end

      def init_with(coder)
        __setobj__(coder["subtype"])
      end

      def encode_with(coder)
        coder["subtype"] = __getobj__
      end
    end
  end
end
