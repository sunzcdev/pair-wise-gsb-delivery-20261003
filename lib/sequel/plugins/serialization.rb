# frozen-string-literal: true

module Sequel
  module Plugins
    # The serialization plugin allows you to keep serialized
    # ruby objects in the database, while giving you deserialized objects
    # when you call an accessor.
    #
    # This plugin works by keeping the serialized value in the values, and
    # adding a deserialized_values hash.  The reader method for serialized columns
    # will check the deserialized_values for the value, return it if present,
    # or deserialize the entry in values and return it.  The writer method will
    # set the deserialized_values entry.  This plugin adds a before_validation hook
    # that serializes all deserialized_values to values.
    #
    # You can specify the serialization format as a pair of serializer/deserializer
    # callable objects.  You can also specify the serialization format as a single
    # symbol, if such a symbol has a registered serializer/deserializer pair in the
    # plugin.  By default, the plugin registers the :marshal, :yaml, and :json
    # serialization formats.  To register your own serialization formats, use
    # Sequel::Plugins::Serialization.register_format.
    # If you use yaml or json format, you need to require the libraries, Sequel
    # does not do the requiring for you.
    #
    # You can specify the columns to serialize when loading the plugin, or later
    # using the serialize_attributes class method.
    #
    # Because of how this plugin works, it must be used inside each model class
    # that needs serialization, after any set_dataset method calls in that class.
    # Otherwise, it is possible that the default column accessors will take
    # precedence.
    #
    # Note that use of an unsafe serialization method can result in an attack vector
    # (potentially allowing remote code execution) if an attacker has the ability to
    # store data directly in the underlying column. This would affect the marshal
    # serialization format, and on older versions of Ruby, potentially the yaml and
    # json serialization formats as well. It can also affect custom formats. You
    # should ensure that attackers do not have access to store data directly in the
    # underlying column when using this plugin (especially when using an unsafe
    # serialization method).
    #
    # == Example
    #
    #   # Require json if you plan to use it, as the plugin doesn't require it for you.
    #   require 'json'
    #
    #   # Register custom serializer/deserializer pair, if desired
    #   require 'sequel/plugins/serialization'
    #   require 'base64'
    #   Sequel::Plugins::Serialization.register_format(:base64, Base64.method(:encode64), Base64.method(:decode64))
    #
    #   class User < Sequel::Model
    #     # Built-in format support when loading the plugin
    #     plugin :serialization, :json, :permissions
    #
    #     # Built-in format support after loading the plugin using serialize_attributes
    #     plugin :serialization
    #     serialize_attributes :marshal, :permissions
    #
    #     # Use custom registered serialization format just like built-in format
    #     serialize_attributes :base64, :password
    #
    #     # Use a custom serializer/deserializer pair without registering
    #     serialize_attributes [ Base64.method(:encode64), Base64.method(:decode64)], :password
    #   end
    #   user = User.create
    #   user.permissions = {global: 'read-only'}
    #   user.save
    #
# Note that if you mutate serialized column values without reassigning them,
# those changes won't be picked up by <tt>Model#save_changes</tt> or
# <tt>Model#update</tt>.  Example:
#
#   user = User[1]
#   user.permissions[:global] = 'foo'
#   user.save_changes # Will not pick up changes to permissions
#
# You can use the +serialization_modification_detection+ plugin to pick
# up such changes.
#
# As an alternative to in-place mutation, you can use +edit_serialized+
# to edit a serialized column value inside a block.  The block receives
# an isolated deep copy of the current value.  If the block exits
# normally and the edited value passes validation for the column, the
# column value is replaced.  If the block raises an exception or the
# edited value is rejected by validation, the column value is left
# unmodified:
#
#   user.edit_serialized(:permissions) do |permissions|
#     permissions[:global] = 'read-write'
#   end
module Serialization
      # The default serializers supported by the serialization module.
      # Use register_format to add serializers to this hash.
      REGISTERED_FORMATS = {}

      # Set up the column readers to do deserialization and the column writers
      # to save the value in deserialized_values.
      def self.apply(model, *args)
        model.instance_exec do
          @deserialization_map = {}
          @serialization_map = {}
        end
      end
      
      # Automatically call serialize_attributes with the format and columns unless
      # no columns were provided.
      def self.configure(model, format=nil, *columns)
        model.serialize_attributes(format, *columns) unless columns.empty?
      end

      # Register a serializer/deserializer pair with a format symbol, to allow
      # models to pick this format by name.  Both serializer and deserializer
      # should be callable objects.
      def self.register_format(format, serializer, deserializer)
        Sequel.synchronize{REGISTERED_FORMATS[format] = [serializer, deserializer].freeze}
      end
      register_format(:marshal, lambda{|v| [Marshal.dump(v)].pack('m')},
        lambda do |v|
          # Handle unpacked marshalled data for backwards compat
          v = v.unpack('m')[0] unless v.start_with?("\x04\x08")
          Marshal.load(v)
        end)
      register_format(:yaml, :to_yaml.to_proc, lambda{|s| YAML.load(s)})
      register_format(:json, Sequel.method(:object_to_json), Sequel.method(:parse_json))

      module ClassMethods
        Plugins.model_instance_variables(self, :@deserialized_values)

        # A hash with column name symbols and callable values, with the value
        # called to deserialize the column.
        attr_reader :deserialization_map

        # A hash with column name symbols and callable values, with the value
        # called to serialize the column.
        attr_reader :serialization_map

        Plugins.inherited_instance_variables(self, :@deserialization_map=>:dup, :@serialization_map=>:dup)

        # Freeze serialization metadata when freezing model class.
        def freeze
          @deserialization_map.freeze
          @serialization_map.freeze
          @serialization_module.freeze if @serialization_module

          super
        end
        
        # Create instance level reader that deserializes column values on request,
        # and instance level writer that stores new deserialized values. If +format+
        # is a symbol, it should correspond to a previously-registered format using +register_format+.
        # Otherwise, +format+ is expected to be a 2-element array of callables,
        # with the first element being the serializer, used to convert the value used by the application
        # to the value that will be stored in the database, and the second element being the deserializer,
        # used to convert the value stored the database to the value used by the application.
        def serialize_attributes(format, *columns)
          if format.is_a?(Symbol)
            unless format = Sequel.synchronize{REGISTERED_FORMATS[format]}
              raise(Error, "Unsupported serialization format: #{format} (valid formats: #{Sequel.synchronize{REGISTERED_FORMATS.keys}.inspect})")
            end
          end
          serializer, deserializer = format
          raise(Error, "No columns given.  The serialization plugin requires you specify which columns to serialize") if columns.empty?
          define_serialized_attribute_accessor(serializer, deserializer, *columns)
        end
        
        private

        # Add serializated attribute accessor methods to the serialization_module
        def define_serialized_attribute_accessor(serializer, deserializer, *columns)
          m = self
          include(@serialization_module ||= Sequel.set_temp_name(Module.new){"#{name}::@serialization_module"}) unless @serialization_module
          @serialization_module.class_eval do
            columns.each do |column|
              m.serialization_map[column] = serializer
              m.deserialization_map[column] = deserializer
              define_method(column) do 
                if deserialized_values.has_key?(column)
                  deserialized_values[column]
                elsif frozen?
                  deserialize_value(column, super())
                else
                  deserialized_values[column] = deserialize_value(column, super())
                end
              end
              alias_method(column, column)

              setter = :"#{column}="
              define_method(setter) do |v| 
                cc = changed_columns
                if !cc.include?(column) && (new? || get_column_value(column) != v)
                  cc << column

                  will_change_column(column) if respond_to?(:will_change_column)
                end

                deserialized_values[column] = v
              end
              alias_method(setter, setter)
            end
          end
        end
      end

      module InstanceMethods
        # Hash of deserialized values, used as a cache.
        def deserialized_values
          @deserialized_values ||= {}
        end

        # Edit the serialized column value in isolation, yielding a deep
        # copy of the current value to the block.  If the block raises an
        # exception, or the edited value does not pass validation for the
        # column, the column value is not modified.  Otherwise, the column
        # value is replaced with the edited value.  Returns true if the
        # column value was replaced (or the block made no changes), and
        # false if the edited value was rejected by validation.
        #
        #   user.edit_serialized(:permissions) do |permissions|
        #     permissions[:global] = 'read-write'
        #   end
        #
        # The object yielded to the block is a deep copy of the current
        # value, so modifying it inside the block does not affect the
        # model until the edit is committed, and modifying it after the
        # block returns never affects the model.  Nested hashes and arrays
        # are copied, subobjects shared inside the value remain shared
        # inside the copy, and frozen values are copied into unfrozen
        # duplicates.
        def edit_serialized(column)
          raise Error, "edit_serialized called on non-serialized column: #{column.inspect}" unless model.deserialization_map.has_key?(column)

          current = public_send(column)
          edited = isolated_serialized_value(current)
          yield edited
          return true if edited == current

          # Validate the edited value on a duplicate of the model, so a
          # validation failure leaves the receiver completely unmodified.
          check = dup
          check.send(:"#{column}=", edited)
          begin
            check.send(:_valid?, OPTS)
          rescue HookFailed
            # Validation was halted by a hook before it could run, so
            # the edited value was not verified.
            return false
          end
          column_errors = check.errors[column]
          return false unless column_errors.nil? || column_errors.empty?

          # Assign a separate copy of the edited value, so that later
          # modification of the object yielded to the block does not
          # modify the model's value.
          public_send(:"#{column}=", isolated_serialized_value(edited))
          true
        end

        # Freeze the deserialized values
        def freeze
          deserialized_values
          super
          deserialized_values.freeze
          self
        end

        # Serialize deserialized values before saving
        def before_validation
          serialize_deserialized_values
          super
        end
        
        private

        # Clear any cached deserialized values when doing a manual refresh.
        def _refresh_set_values(hash)
          @deserialized_values.clear if @deserialized_values
          super
        end

        # Deserialize the column value.  Called when the model column accessor is called to
        # return a deserialized value.
        def deserialize_value(column, v)
          unless v.nil?
            raise Sequel::Error, "no entry in deserialization_map for #{column.inspect}" unless callable = model.deserialization_map[column]
            callable.call(v)
          end
        end

        # Dup the deserialized values when duping model instance.
        def initialize_copy(other)
          super
          @deserialized_values = Hash[other.deserialized_values]
          self
        end

        # Serialize all deserialized values
        def serialize_deserialized_values
          deserialized_values.each{|k,v| @values[k] = serialize_value(k, v)}
        end

        # Serialize the column value.  Called before saving to ensure the serialized value
        # is saved in the database.
        def serialize_value(column, v)
          unless v.nil?
            raise Sequel::Error, "no entry in serialization_map for #{column.inspect}" unless callable = model.serialization_map[column]
            callable.call(v)
          end
        end

        # Return a deep copy of a serialized value, used to isolate values
        # yielded to edit_serialized blocks from the model's actual value.
        # Copies nested hashes and arrays, keeps subobjects shared inside
        # the value shared inside the copy, and duplicates frozen values
        # into unfrozen copies.
        def isolated_serialized_value(value, memo = {}.compare_by_identity)
          if Hash === value || Array === value
            return memo[value] if memo.has_key?(value)

            copy = value.dup
            memo[value] = copy

            if Hash === value
              copy.clear
              value.each do |k, v|
                copy[isolated_serialized_value(k, memo)] = isolated_serialized_value(v, memo)
              end
            else
              copy.map!{|v| isolated_serialized_value(v, memo)}
            end

            copy
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
