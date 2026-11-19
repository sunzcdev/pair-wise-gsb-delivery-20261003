# encoding: binary
# frozen_string_literal: true

require "date"
require "amq/protocol/type_constants"
require "amq/protocol/float_32bit"
require "amq/protocol/table_value_encoder"

module AMQ
  module Protocol
    class Table
      # Read-only description of how a single table value (including the
      # leading type tag) will be serialized.
      class ValuePlan
        attr_reader :type, :size, :value, :table, :array, :encoded

        def initialize(type:, size:, value: nil, table: nil, array: nil, encoded: nil)
          @type    = type
          @size    = size
          @value   = value
          @table   = table
          @array   = array&.freeze
          @encoded = encoded
          freeze
        end

        def hash?
          !@table.nil?
        end

        def array?
          !@array.nil?
        end

        def custom?
          !@encoded.nil?
        end
      end

      # One key/value pair of an EncodingPlan.
      class Entry
        attr_reader :key, :original_key, :value_plan

        def initialize(key:, original_key:, value_plan:)
          @key          = key
          @original_key = original_key
          @value_plan   = value_plan
          freeze
        end

        def type
          @value_plan.type
        end

        def size
          # 1 byte for the key length octet, the key itself, then the value
          1 + @key.bytesize + @value_plan.size
        end
      end

      # Read-only encoding plan for a whole table: every key, the selected
      # AMQP type tag and the exact encoded size, computed before any byte
      # of the real payload is concatenated.
      class EncodingPlan
        attr_reader :entries

        def initialize(entries)
          @entries = entries.dup.freeze
          freeze
        end

        def content_size
          @entries.sum(&:size)
        end

        def total_size
          # the table itself is prefixed with a 4 byte length
          4 + content_size
        end

        # Flat, inspection friendly view of the plan: one row per key,
        # recursively descending into nested tables
        def rows
          @entries.flat_map do |entry|
            row = { key: entry.key, type: entry.type, size: entry.size }
            if entry.value_plan.hash?
              [row] + entry.value_plan.table.rows.map do |nested|
                nested.merge(key: "#{entry.key}.#{nested[:key]}")
              end
            else
              [row]
            end
          end
        end
      end

      class TableError < StandardError
      end

      class KeyTooLongError < TableError
        def initialize(path, byte_size)
          super("field name at #{path} is #{byte_size} bytes long, the AMQP 0-9-1 limit is #{Table::MAX_KEY_BYTE_SIZE} bytes")
        end
      end

      class DuplicateKeyError < TableError
        def initialize(path, key)
          super("duplicate field name #{key.inspect} at #{path} after Symbol/String conversion; use the on_duplicate_key option to pick an explicit policy")
        end
      end

      class CircularStructureError < TableError
        def initialize(path)
          super("circular structure detected while planning table encoding at #{path}")
        end
      end

      class UnsupportedValueError < TableError
        def initialize(path, value)
          super("unsupported table value at #{path}: #{value.inspect} (#{value.class})")
        end
      end

      # Builds an EncodingPlan without touching the input data. The same
      # type selection rules as TableValueEncoder.encode are used, so the
      # plan always matches what actually ends up on the wire.
      module EncodingPlanner
        module_function

        def plan_table(hash, on_duplicate_key:, fallback:)
          EncodingPlan.new(plan_hash_entries(hash, PlannerPath.root, on_duplicate_key, fallback, []))
        end

        def plan_value(value, path, on_duplicate_key, fallback, container_stack)
          case value
          when Hash
            guard_cycle(value, path, container_stack)
            nested_stack = container_stack + [value.object_id]
            nested  = EncodingPlan.new(plan_hash_entries(value, path, on_duplicate_key, fallback, nested_stack))
            ValuePlan.new(type: TypeConstants::TYPE_HASH, size: 1 + 4 + nested.content_size, value: value, table: nested)
          when Array
            guard_cycle(value, path, container_stack)
            nested_stack = container_stack + [value.object_id]
            elements = value.each_with_index.map do |element, index|
              plan_value(element, path.child_index(index), on_duplicate_key, fallback, nested_stack)
            end
            ValuePlan.new(
              type: TypeConstants::TYPE_ARRAY,
              size: 1 + 4 + elements.sum(&:size),
              value: value,
              array: elements
            )
          else
            plan_scalar(value, path, fallback)
          end
        end

        def plan_hash_entries(hash, path, on_duplicate_key, fallback, container_stack)
          entries = []
          seen    = {}

          hash.each do |raw_key, value|
            key = raw_key.to_s
            child_path = path.child_key(raw_key)

            if key.bytesize > Table::MAX_KEY_BYTE_SIZE
              raise KeyTooLongError.new(child_path.to_s, key.bytesize)
            end

            if seen.key?(key)
              case on_duplicate_key
              when :raise
                raise DuplicateKeyError.new(path.to_s, key)
              when :first
                next
              when :last
                entries.delete(seen[key])
              else
                raise ArgumentError, "unknown on_duplicate_key policy #{on_duplicate_key.inspect}, expected one of :raise, :first, :last"
              end
            end

            entry = Entry.new(
              key: key.b,
              original_key: raw_key,
              value_plan: plan_value(value, child_path, on_duplicate_key, fallback, container_stack)
            )
            seen[key] = entry
            entries << entry
          end

          entries
        end

        def plan_scalar(value, path, fallback)
          type = TableValueEncoder.type_tag(value)
          return plan_custom(value, path, fallback) if type.nil?

          size =
            case value
            when String, Symbol then 1 + 4 + value.to_s.bytesize
            when Integer        then 1 + 8
            when Float32Bit     then 1 + 4
            when Float          then 1 + 8
            when true, false    then 1 + 1
            when Time, DateTime then 1 + 8
            when nil            then 1
            else
              # BigDecimal: type_tag already validated the match, the wire
              # form is a tag plus a decimals octet and a uint32
              1 + 5
            end

          ValuePlan.new(type: type, size: size, value: value)
        end

        # The custom encoder is invoked exactly once here, during planning.
        # Its output is cached on the plan and reused while writing, so it
        # never runs a second time just to measure the result.
        def plan_custom(value, path, fallback)
          raise UnsupportedValueError.new(path.to_s, value) if fallback.nil?

          encoded = fallback.call(value, path.to_s)
          unless encoded.is_a?(String) && !encoded.empty?
            raise UnsupportedValueError.new(path.to_s, value)
          end

          ValuePlan.new(type: encoded.byteslice(0, 1), size: encoded.bytesize, value: value, encoded: encoded.b.freeze)
        end

        def guard_cycle(container, path, stack)
          raise CircularStructureError.new(path.to_s) if stack.include?(container.object_id)
        end
      end

      # Append only path used while walking the input. Never escapes the
      # planner: error messages receive a plain frozen string.
      class PlannerPath
        def self.root
          new([])
        end

        def initialize(segments)
          @segments = segments
        end

        def child_key(key)
          self.class.new(@segments + [key.to_s])
        end

        def child_index(index)
          self.class.new(@segments + ["[#{index}]"])
        end

        def to_s
          return "<root>" if @segments.empty?

          @segments.each_with_index.map do |segment, index|
            segment.start_with?("[") ? segment : (index.zero? ? segment : ".#{segment}")
          end.join
        end
      end
      private_constant :PlannerPath
    end
  end
end
