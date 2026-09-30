# frozen_string_literal: true

module Tomlrb
  class DumpError < StandardError; end

  # Serializes a Ruby data structure into a TOML 1.0 string.
  #
  # Arrays whose elements are all hashes are emitted as arrays of tables;
  # every other array (including empty arrays and mixed arrays) is emitted
  # inline. Empty hashes are always preserved.
  #
  # @api private
  class Dumper
    BARE_KEY = /\A[A-Za-z0-9_-]+\z/.freeze

    def self.dump(root)
      new.dump(root)
    end

    def initialize
      @out = []
      @active = {}
    end

    def dump(root)
      unless root.is_a?(Hash)
        raise DumpError,
              "TOML top-level value must be a Hash, got #{class_name(root)}"
      end

      visit_table_body(root, [], [])
      @out.join
    end

    private

    # +path+ locates the table for error messages (it includes array
    # indices) while +header+ is its dotted table path (indices excluded).
    #
    # Scalar key/value pairs of a table are always emitted before its
    # sub-tables. This keeps every value attached to its own table header,
    # which is in particular what isolates the successive instances of an
    # array of tables.
    def visit_table_body(table, path, header)
      enter(table, path)
      table.each do |key, value|
        next if value.is_a?(Hash) || table_array?(value)

        @out << "#{format_key(key, path)} = #{format_inline(value, path + [key])}\n"
      end
      visit_children(table, path, header)
      leave(table)
    end

    def visit_children(table, path, header)
      table.each do |key, value|
        child_path = path + [key]
        child_header = header + [key]
        if value.is_a?(Hash)
          visit_subtable(value, child_path, child_header)
        elsif table_array?(value)
          visit_table_array(value, child_path, child_header)
        end
      end
    end

    def visit_subtable(table, path, header)
      @out << "\n" if @out.any?
      @out << "[#{format_header(header)}]\n"
      visit_table_body(table, path, header)
    end

    def visit_table_array(array, path, header)
      enter(array, path)
      array.each_with_index do |table, index|
        unless table.is_a?(Hash)
          raise DumpError,
                error_message(path + [index], table,
                              'array of tables must contain only tables')
        end

        @out << "\n" if @out.any?
        @out << "[[#{format_header(header)}]]\n"
        visit_table_body(table, path + [index], header)
      end
      leave(array)
    end

    def table_array?(value)
      value.is_a?(Array) && !value.empty? && value.all? { |element| element.is_a?(Hash) }
    end

    def format_inline(value, path)
      case value
      when Hash
        enter(value, path)
        body = value.map do |key, element|
          "#{format_key(key, path)} = #{format_inline(element, path + [key])}"
        end.join(', ')
        leave(value)
        "{#{body}}"
      when Array
        enter(value, path)
        body = value.each_with_index.map do |element, index|
          format_inline(element, path + [index])
        end.join(', ')
        leave(value)
        "[#{body}]"
      else
        format_scalar(value, path)
      end
    end

    def format_scalar(value, path)
      case value
      when Integer then value.to_s
      when Float then format_float(value)
      when true, false then value.to_s
      when String then format_string(value, path)
      when LocalDate, LocalTime, LocalDateTime then value.to_s
      when DateTime then format_date_time(value)
      when Date then value.strftime('%F')
      when Time then format_time(value)
      else
        raise DumpError, error_message(path, value, 'value is not representable in TOML')
      end
    end

    def format_float(value)
      case
      when value.nan? then 'nan'
      when value.infinite? == 1 then 'inf'
      when value.infinite? == -1 then '-inf'
      else value.inspect
      end
    end

    def format_time(value)
      zone = value.utc_offset.zero? ? 'Z' : value.strftime('%:z')
      value.strftime('%FT%T') << format_fraction(value.subsec) << zone
    end

    def format_date_time(value)
      value.strftime('%FT%T') << format_fraction(value.second_fraction) << value.zone
    end

    # Emits only the digits that carry precision, with at least one digit.
    def format_fraction(fraction)
      nanos = (fraction * 1_000_000_000).round
      return '' if nanos.zero?

      '.' + format('%09d', nanos).sub(/0+\z/, '')
    end

    def format_string(value, path)
      value = encode_utf8(value, path)

      escaped = value.chars.map do |char|
        case char
        when '\\' then '\\\\'
        when '"' then '\\"'
        when "\b" then '\b'
        when "\t" then '\t'
        when "\n" then '\n'
        when "\f" then '\f'
        when "\r" then '\r'
        else
          char.ord < 0x20 || char.ord == 0x7F ? format('\\u%04X', char.ord) : char
        end
      end.join
      %("#{escaped}")
    end

    def encode_utf8(value, path)
      value = value.encode(Encoding::UTF_8) unless value.encoding == Encoding::UTF_8
      unless value.valid_encoding?
        raise DumpError, error_message(path, value, 'string must be valid UTF-8')
      end

      value
    rescue Encoding::UndefinedConversionError, Encoding::InvalidByteSequenceError,
           Encoding::ConverterNotFoundError
      raise DumpError, error_message(path, value, 'string must be valid UTF-8')
    end

    def format_key(key, path)
      unless key.is_a?(String)
        raise DumpError,
              error_message(path, key, "keys must be Strings, got #{class_name(key)}")
      end

      BARE_KEY.match?(key) ? key : format_string(key, path)
    end

    def format_header(path)
      path.map do |segment|
        segment.is_a?(Integer) ? segment.to_s : format_key(segment, path)
      end.join('.')
    end

    def enter(container, path)
      object_id = container.object_id
      if @active.key?(object_id)
        raise DumpError, error_message(path, container,
                                       'circular data structure cannot be represented in TOML')
      end

      @active[object_id] = path
    end

    def leave(container)
      @active.delete(container.object_id)
    end

    def error_message(path, value, reason)
      "cannot dump #{format_path(path)} (#{class_name(value)}): #{reason}"
    end

    def format_path(path)
      return '<root>' if path.empty?

      result = +''
      path.each do |segment|
        rendered =
          if segment.is_a?(Integer)
            "[#{segment}]"
          elsif segment.is_a?(String)
            format_key(segment, [])
          else
            segment.inspect
          end

        if rendered.start_with?('[') || result.empty?
          result << rendered
        else
          result << '.' << rendered
        end
      end
      result
    end

    def class_name(value)
      value.nil? ? 'nil' : value.class.name
    end
  end
end
