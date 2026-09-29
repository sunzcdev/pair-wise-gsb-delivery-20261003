# frozen-string-literal: true

module Sequel
  module Plugins
    # The serialized_edit plugin adds a block-based interface for editing
    # serialized column values, so that callers no longer need to mutate
    # the deserialized value in place and hope nothing goes wrong:
    #
    #   album.edit_serialized(:tags) do |tags|
    #     tags << 'rock'
    #   end
    #
    # The block is yielded a deep copy of the current value, completely
    # isolated from the model.  Only if the block exits normally and the
    # model's validations pass for that column is the edited value assigned
    # back to the model.  If the block raises an exception, or validation
    # adds errors for the column, the model's value is left untouched.
    #
    # The copy given to the block handles nested arrays and hashes, shared
    # subobjects (a subobject referenced from multiple places is copied once,
    # preserving the sharing inside the copy), and frozen values (the copies
    # are unfrozen, even if the current value is frozen).  When committing,
    # a separate deep copy is assigned to the model, so code retaining the
    # object yielded to the block cannot modify the model's value after the
    # block returns.
    #
    # The plugin works with the dirty, serialization_modification_detection,
    # and refresh features: committing goes through the normal column writer,
    # so changed_columns, column_changes, and original deserialized value
    # tracking all behave as if the value had been assigned directly, and
    # refresh clears any committed value as usual.
    #
    # == Example
    #
    #   class Album < Sequel::Model
    #     plugin :serialization, :json, :permissions
    #     plugin :serialized_edit
    #
    #     def validate
    #       super
    #       errors.add(:permissions, 'cannot grant admin') if permissions && permissions['role'] == 'admin'
    #     end
    #   end
    #
    #   album = Album[1]
    #   album.permissions # => {'tags'=>['a'], 'meta'=>{'level'=>1}}
    #
    #   # Commit: the block modifies the nested structure, validation passes,
    #   # and the new value is assigned to the model:
    #   album.edit_serialized(:permissions) do |perms|
    #     perms['tags'] << 'b'
    #     perms['meta']['level'] = 2
    #   end
    #   album.permissions # => {'tags'=>['a', 'b'], 'meta'=>{'level'=>2}}
    #
    #   # Rollback: validation rejects the new value, so the model keeps
    #   # its original value:
    #   album.edit_serialized(:permissions) do |perms|
    #     perms['role'] = 'admin'
    #   end # => nil
    #   album.permissions # => {'tags'=>['a', 'b'], 'meta'=>{'level'=>2}}
    #   album.errors.on(:permissions) # => ['cannot grant admin']
    module SerializedEdit
      # Load the serialization plugin automatically.
      def self.apply(model)
        model.plugin :serialization
      end

      module InstanceMethods
        # Edit the serialized column value using a block.  The block is
        # yielded a deep copy of the current value, isolated from the model.
        # If the block raises an exception, the model is not modified and
        # the exception propagates.  If the block returns normally, the
        # edited value is validated on a throwaway copy of the model; if
        # validation adds errors for the column, the errors are copied to
        # the receiver and nil is returned, leaving the receiver's value
        # untouched.  Otherwise, a deep copy of the edited value is
        # assigned to the model and returned, so later modifications of
        # the object yielded to the block cannot affect the model.
        #
        # If the block does not modify the value, the model is left
        # untouched and the current value is returned.
        #
        # Note that only errors added for the given column prevent the
        # commit; errors added for other columns are ignored.
        def edit_serialized(column)
          unless model.serialization_map.has_key?(column)
            raise(Error, "edit_serialized can only be used on serialized columns (column: #{column.inspect})")
          end
          unless block_given?
            raise(ArgumentError, "edit_serialized requires a block")
          end

          current = get_column_value(column)
          candidate = _serialized_edit_deep_copy(current, {})
          yield candidate

          # If the block did not modify the value, there is nothing to commit.
          return current if candidate == current

          # Validate the edited value on a throwaway copy of the model, so
          # that failed validations and validation hooks cannot affect the
          # receiver.
          check = dup
          check.set_column_value(:"#{column}=", _serialized_edit_deep_copy(candidate, {}))
          check.valid?

          if (column_errors = check.errors.on(column))
            column_errors.each{|message| errors.add(column, message)}
            nil
          else
            # Assign a fresh deep copy, so that code retaining the object
            # yielded to the block cannot modify the model's value.
            set_column_value(:"#{column}=", _serialized_edit_deep_copy(candidate, {}))
            get_column_value(column)
          end
        end

        private

        # Recursively copy the given value, so that modifications of the
        # copy cannot affect the original.  Handles nested arrays and
        # hashes, shared subobjects (a subobject referenced multiple times
        # is copied once, preserving the sharing inside the copy), and
        # frozen values (the copies are not frozen).  The +copies+ hash
        # maps object ids of already copied objects to their copies.
        def _serialized_edit_deep_copy(value, copies)
          case value
          when Hash, Array
            oid = value.object_id
            if copies.has_key?(oid)
              copies[oid]
            else
              copy = value.dup
              copies[oid] = copy
              if value.is_a?(Hash)
                value.each{|k, v| copy[k] = _serialized_edit_deep_copy(v, copies)}
              else
                value.each_with_index{|v, i| copy[i] = _serialized_edit_deep_copy(v, copies)}
              end
              copy
            end
          when nil, false, true, Symbol, Numeric
            value
          else
            begin
              value.dup
            rescue TypeError
              value
            end
          end
        end
      end
    end
  end
end
