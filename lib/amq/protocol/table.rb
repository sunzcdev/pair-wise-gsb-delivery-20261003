# encoding: binary
# frozen_string_literal: true

require "amq/protocol/type_constants"
require "amq/protocol/encoding_plan"
require "amq/protocol/table_value_encoder"
require "amq/protocol/table_value_decoder"

module AMQ
  module Protocol
    class Table

      #
      # Behaviors
      #

      include TypeConstants

      # Pack format string
      PACK_UINT32_BE = 'N'.freeze

      # AMQP 0-9-1 field names are prefixed with a single unsigned octet,
      # so a name can be at most 255 bytes long
      MAX_KEY_BYTE_SIZE = 255

      # What to do when two keys of the same hash map to the same field
      # name after Symbol/String conversion
      DUPLICATE_KEY_POLICIES = [:raise, :first, :last].freeze

      OPTION_KEYS     = [:on_duplicate_key, :fallback].freeze
      DEFAULT_OPTIONS = { on_duplicate_key: :raise, fallback: nil }.freeze

      #
      # API
      #

      class InvalidTableError < StandardError
        def initialize(key, value)
          super("Invalid table value on key #{key}: #{value.inspect} (#{value.class})")
        end
      end


      # Builds the read-only encoding plan for a table without serializing
      # it. Raises before any concatenation would happen on unsupported
      # values, overlong keys, duplicate keys or circular structures.
      def self.encoding_plan(table = nil, **options)
        table, options = resolve_table_and_options(table, options)

        EncodingPlanner.plan_table(table, on_duplicate_key: options[:on_duplicate_key], fallback: options[:fallback])
      end


      def self.encode(table = nil, **options)
        table, options = resolve_table_and_options(table, options)

        plan = EncodingPlanner.plan_table(table, on_duplicate_key: options[:on_duplicate_key], fallback: options[:fallback])

        [plan.content_size].pack(PACK_UINT32_BE) << write_entries(plan.entries)
      end


      # Entry point used by TableValueEncoder.size helpers so they share
      # the planner's type selection
      def self.plan_value_public(value)
        EncodingPlanner.plan_value(value, PlannerPath.root, :raise, nil, [])
      end


      def self.decode(data)
        table        = {}
        table_length = data.unpack1(PACK_UINT32_BE)

        return table if table_length.zero?

        offset       = 4
        while offset < (4 + table_length)
          key, offset  = decode_table_key(data, offset)
          type, offset = TableValueDecoder.decode_value_type(data, offset)

          table[key] = case type
                       when TYPE_STRING
                         v, offset = TableValueDecoder.decode_string(data, offset)
                         v
                       when TYPE_BYTE_ARRAY
                         # Ruby doesn't have a direct counterpart to
                         # ByteBuffer or byte[], so using a string feels
                         # more appropriate than an array of fixnums
                         v, offset = TableValueDecoder.decode_string(data, offset)
                         v
                       when TYPE_INTEGER
                         v, offset = TableValueDecoder.decode_integer(data, offset)
                         v
                       when TYPE_DECIMAL
                         v, offset = TableValueDecoder.decode_big_decimal(data, offset)
                         v
                       when TYPE_TIME
                         v, offset = TableValueDecoder.decode_time(data, offset)
                         v
                       when TYPE_HASH
                         v, offset = TableValueDecoder.decode_hash(data, offset)
                         v
                       when TYPE_BOOLEAN
                         v, offset = TableValueDecoder.decode_boolean(data, offset)
                         v
                       when TYPE_BYTE
                         v, offset = TableValueDecoder.decode_byte(data, offset)
                         v
                       when TYPE_SIGNED_16BIT
                         v, offset = TableValueDecoder.decode_short(data, offset)
                         v
                       when TYPE_SIGNED_64BIT
                         v, offset = TableValueDecoder.decode_long(data, offset)
                         v
                       when TYPE_32BIT_FLOAT
                         v, offset = TableValueDecoder.decode_32bit_float(data, offset)
                         v
                       when TYPE_64BIT_FLOAT
                         v, offset = TableValueDecoder.decode_64bit_float(data, offset)
                         v
                       when TYPE_VOID
                         nil
                       when TYPE_ARRAY
                         v, offset = TableValueDecoder.decode_array(data, offset)
                         v
                       else
                         raise ArgumentError, "Not a valid type: #{type.inspect}\nData: #{data.inspect}\nUnprocessed data: #{data[offset..-1].inspect}\nOffset: #{offset}\nTotal size: #{table_length}\nProcessed data: #{table.inspect}"
                       end
        end

        table
      end


      def self.length(data)
        data.unpack1(PACK_UINT32_BE)
      end


      def self.hash_size(value)
        acc = 0
        value.each do |k, v|
          acc += (1 + k.to_s.bytesize)
          acc += TableValueEncoder.field_value_size(v)
        end

        acc
      end


      def self.decode_table_key(data, offset)
        key_length = data.getbyte(offset)
        offset += 1
        key = data.byteslice(offset, key_length)
        offset += key_length

        [key, offset]
      end


      #
      # Implementation
      #

      # Ruby 3 keeps positional hashes and keyword arguments apart. The
      # historical API accepted symbol keyed tables positionally, so a bare
      # Table.encode(test: true) now arrives as keywords. When the keyword
      # hash contains a key that is not one of our options it is treated as
      # the table itself; pure option hashes configure an empty table.
      def self.resolve_table_and_options(table, options)
        if table.nil? && !(options.keys - OPTION_KEYS).empty?
          table   = options
          options = {}
        elsif !(unknown = options.keys - OPTION_KEYS).empty?
          raise ArgumentError, "unknown option(s): #{unknown.map(&:inspect).join(', ')}"
        end

        options = DEFAULT_OPTIONS.merge(options)
        unless DUPLICATE_KEY_POLICIES.include?(options[:on_duplicate_key])
          raise ArgumentError, "unknown on_duplicate_key policy #{options[:on_duplicate_key].inspect}, expected one of #{DUPLICATE_KEY_POLICIES.map(&:inspect).join(', ')}"
        end

        [table || {}, options]
      end
      private_class_method :resolve_table_and_options

      def self.write_entries(entries)
        buffer = String.new

        entries.each do |entry|
          buffer << entry.key.bytesize.chr
          buffer << entry.key
          buffer << write_value_plan(entry.value_plan)
        end

        buffer
      end
      private_class_method :write_entries

      def self.write_value_plan(plan)
        if plan.custom?
          # produced once during planning, reused as-is here
          return plan.encoded.dup
        end

        if plan.hash?
          return plan.type + [plan.table.content_size].pack(PACK_UINT32_BE) + write_entries(plan.table.entries)
        end

        if plan.array?
          payload = String.new
          plan.array.each { |element| payload << write_value_plan(element) }
          return plan.type + [payload.bytesize].pack(PACK_UINT32_BE) + payload
        end

        plan.type + TableValueEncoder.encode_payload(plan.value)
      end
      private_class_method :write_value_plan

    end # Table
  end # Protocol
end # AMQ
