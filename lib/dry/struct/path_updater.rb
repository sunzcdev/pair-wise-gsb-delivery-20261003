# frozen_string_literal: true

module Dry
  class Struct
    # Immutable, type-checked updates of values located by struct attributes
    # and array indices.
    #
    # @api private
    class PathUpdater
      # Sentinel value used to remove an optional attribute.
      Delete = ::Object.new.freeze

      ANY_TYPE = Types::Any

      Change = ::Struct.new(:path, :delete, :validated, keyword_init: true)

      # Convert public input into a common change representation.
      def self.change(path, value)
        Change.new(
          path: normalize_path(path),
          delete: value.equal?(Delete),
          validated: value.equal?(Delete) ? nil : value
        )
      end

      def self.normalize_path(path)
        unless path.is_a?(::Array) && !path.empty? &&
               path.all? { |segment| segment.is_a?(::Symbol) || segment.is_a?(::Integer) }
          raise ::ArgumentError,
                "path must be a non-empty array of symbols and integers, #{path.inspect} given"
        end

        path.dup
      end

      # @param [Struct] struct
      # @param [Array<Change>] changes
      def initialize(struct, changes)
        @struct = struct
        @changes = changes
      end

      # Validate all changes before rebuilding any node.
      #
      # @return [Struct]
      #
      # @api private
      def call
        return @struct if @changes.empty?

        validate_no_prefixes
        validate_changes

        struct = rebuild_struct(@struct, [])

        if defined?(::IceNine) && value_class?(@struct.class)
          ::IceNine.deep_freeze(struct)
        else
          struct
        end
      end

      private

      def validate_no_prefixes
        @changes.map(&:path).combination(2).each do |left, right|
          next unless left == right || prefix?(left, right) || prefix?(right, left)

          raise ::ArgumentError, "conflicting paths #{left.inspect} and #{right.inspect}"
        end
      end

      def validate_changes
        @changes.each do |change|
          terminal = locate(change.path)

          if change.delete
            unless change.path.last.is_a?(::Symbol)
              raise ::ArgumentError,
                    "can't delete an array member at #{change.path.inspect}"
            end
            unless terminal[:optional]
              raise ::ArgumentError,
                    "can't delete required attribute at #{change.path.inspect}"
            end
          else
            result = validate_change(change, terminal[:type])

            unless result.success?
              message = result.error.respond_to?(:message) ? result.error.message : result.error
              raise Error,
                    "[#{@struct.class}.new] #{message} at #{change.path.inspect}"
            end

            change.validated = result.input
          end
        end
      end

      def validate_change(change, type)
        # Struct classes and their constructor wrappers pass an instance of the
        # same struct class through without applying the constructor again.
        struct_class = self.class.struct_class(type)
        if struct_class && change.validated.instance_of?(struct_class)
          Dry::Types::Result::Success.new(change.validated)
        else
          type.try(change.validated)
        end
      end

      # Walk the schema and current value while retaining the terminal type and
      # whether the terminal is an optional struct attribute.
      def locate(path)
        node = @struct
        type = @struct.class
        terminal_type = type
        optional = false

        path.each_with_index do |segment, depth|
          if segment.is_a?(::Symbol)
            unless node.is_a?(::Dry::Struct)
              raise ::TypeError, "can't access attribute #{segment.inspect} on #{node.inspect}"
            end

            key = node.class.schema.key(segment) {
              raise MissingAttributeError.new(attribute: segment, klass: node.class)
            }

            terminal_type = key.type
            type = terminal_type
            optional = !key.required?
            node = node[segment]
          else
            unless node.is_a?(::Array)
              raise ::TypeError, "can't access index #{segment.inspect} on #{node.inspect}"
            end

            array_type = self.class.array_type(type)
            check_array_node(node, segment, array_type)

            member_type =
              array_type.respond_to?(:member) ? array_type.member : ANY_TYPE
            terminal_type = member_type
            type = member_type
            optional = false
            node = node.fetch(segment)
          end

          # Keep traversing only when the current value itself has another
          # struct/array level. Terminal validation still uses the declared type.
          if depth < path.size - 1
            if path[depth + 1].is_a?(::Symbol)
              runtime_struct_type!(node, segment)
              type = node.class
            else
              type = runtime_array_type!(type, node, segment)
            end
          end
        end

        {type: terminal_type, node: node, optional: optional}
      rescue Error, MissingAttributeError, ::ArgumentError
        raise
      rescue ::KeyError, ::IndexError, ::TypeError => exception
        raise Error, "invalid path #{path.inspect}: #{exception.message}"
      end

      def check_array_node(node, index, array_type)
        unless array_type
          raise ::TypeError, "value is not an array"
        end
        unless node.is_a?(::Array)
          raise ::TypeError, "can't access index #{index.inspect} on #{node.inspect}"
        end
        if index.negative?
          raise ::IndexError, "array index #{index} cannot be negative"
        end
        if index >= node.size
          raise ::IndexError, "array index #{index} is outside of array bounds"
        end
      end

      def runtime_struct_type!(node, segment)
        unless node.is_a?(::Dry::Struct)
          raise ::TypeError,
                "value after #{segment.inspect} is not a traversable struct"
        end

        node.class
      end

      def runtime_array_type!(type, node, segment)
        unless node.is_a?(::Array)
          raise ::TypeError,
                "value after #{segment.inspect} is not a traversable array"
        end

        self.class.array_type(type) || Types::Array.new(::Array)
      end

      def rebuild_struct(struct, prefix)
        attributes = struct.__attributes__.dup

        struct.class.schema.each do |key|
          child_path = prefix + [key.name]
          change = change_at(child_path)
          descendants = descendants?(child_path)
          next unless change || descendants

          if change
            if change.delete
              attributes.delete(key.name)
            else
              attributes[key.name] = change.validated
            end
          else
            current = struct[key.name]

            if current.is_a?(::Dry::Struct)
              attributes[key.name] = rebuild_struct(current, child_path)
            elsif current.is_a?(::Array)
              attributes[key.name] = rebuild_array(current, child_path)
            end
          end
        end

        struct.class.load(attributes)
      end

      def value_class?(klass)
        klass.respond_to?(:value_class?) && klass.value_class?
      end

      def rebuild_array(array, prefix)
        array.each_with_index.map do |value, index|
          child_path = prefix + [index]
          change = change_at(child_path)

          if change
            change.validated
          elsif descendants?(child_path) && value.is_a?(::Dry::Struct)
            rebuild_struct(value, child_path)
          elsif descendants?(child_path) && value.is_a?(::Array)
            rebuild_array(value, child_path)
          else
            value
          end
        end
      end

      def change_at(path)
        @changes.find { |change| change.path == path }
      end

      def descendants?(prefix)
        @changes.any? { |change|
          change.path.size > prefix.size && change.path[0, prefix.size] == prefix
        }
      end

      def prefix?(left, right)
        left.size < right.size && right[0, left.size] == left
      end

      # Return an array type when a type represents an array.
      def self.array_type(type)
        unwrapped = unwrap(type)
        unwrapped if unwrapped.is_a?(Types::Array)
      end

      # Return the exact struct class represented by a type.
      def self.struct_class(type)
        unwrapped = unwrap(type)
        unwrapped if unwrapped.is_a?(::Class) && unwrapped <= ::Dry::Struct
      end

      def self.unwrap(type)
        seen = {}

        while type && !seen.key?(type.object_id)
          seen[type.object_id] = true

          if type.is_a?(Types::Sum) && type.optional?
            type = type.right
          elsif wrapped_type?(type)
            type = type.type
          else
            break
          end
        end

        type
      end
      private_class_method :unwrap

      def self.wrapped_type?(type)
        type.is_a?(Types::Decorator) ||
          type.is_a?(Types::Constrained) ||
          type.is_a?(Types::Constructor) ||
          type.is_a?(Types::Lax)
      end
      private_class_method :wrapped_type?
    end
  end
end
