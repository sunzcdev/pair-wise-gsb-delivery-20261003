# frozen_string_literal: true

require_relative "input_record_separator"
require_relative "row"

class CSV
  # Note: Don't use this class directly. This is an internal class.
  class Writer
    #
    # A CSV::Writer receives an output, prepares the header, format and output.
    # It allows us to write new rows in the object and rewind it.
    #
    attr_reader :lineno
    attr_reader :headers

    def initialize(output, options)
      @output = output
      @options = options
      @lineno = 0
      @start_of_row = true
      @field_index = 0
      @fields_converter = nil
      prepare
      if @options[:write_headers] and @headers
        self << @headers
      end
      @fields_converter = @options[:fields_converter]
    end

    #
    # Adds a new row
    #
    def <<(row)
      case row
      when Row
        row = row.fields
      when Hash
        row = @headers.collect {|header| row[header]}
      end

      @headers ||= row if @use_headers
      @lineno += 1

      add_fields(row)
      finish_row

      self
    end

    #
    # Writes the given +fields+ as part of the current row without
    # terminating the row. Column separators are emitted between the
    # chunks of the same row; the row separator is not. Call
    # #finish_row to terminate the row.
    #
    # This is used for streaming a row that is assembled from
    # multiple sources, so a whole row never needs to be buffered.
    #
    def add_fields(fields)
      fields = prepare_fields(fields, @field_index)
      @field_index += fields.size
      line = fields.join(@column_separator)
      unless @start_of_row
        line = @column_separator + line
      end
      emit_line(line)
      @start_of_row = false
      self
    end

    #
    # Terminates the current row by writing the row separator.
    #
    def finish_row
      emit_line(@row_separator)
      @start_of_row = true
      @field_index = 0
      self
    end

    #
    # Winds back to the beginning
    #
    def rewind
      @lineno = 0
      @headers = nil if @options[:headers].nil?
      @start_of_row = true
      @field_index = 0
    end

    private
    def prepare
      @encoding = @options[:encoding]

      prepare_header
      prepare_format
      prepare_output
    end

    def prepare_header
      headers = @options[:headers]
      case headers
      when Array
        @headers = headers
        @use_headers = true
      when String
        @headers = CSV.parse_line(headers,
                                  col_sep: @options[:column_separator],
                                  row_sep: @options[:row_separator],
                                  quote_char: @options[:quote_character])
        @use_headers = true
      when true
        @headers = nil
        @use_headers = true
      else
        @headers = nil
        @use_headers = false
      end
      return unless @headers

      converter = @options[:header_fields_converter]
      @headers = converter.convert(@headers, nil, 0, [])
      @headers.each do |header|
        header.freeze if header.is_a?(String)
      end
    end

    def prepare_force_quotes_fields(force_quotes)
      @force_quotes_fields = {}
      force_quotes.each do |name_or_index|
        case name_or_index
        when Integer
          index = name_or_index
          @force_quotes_fields[index] = true
        when String, Symbol
          name = name_or_index.to_s
          if @headers.nil?
            message = ":headers is required when you use field name " +
                      "in :force_quotes: " +
                      "#{name_or_index.inspect}: #{force_quotes.inspect}"
            raise ArgumentError, message
          end
          index = @headers.index(name)
          next if index.nil?
          @force_quotes_fields[index] = true
        else
          message = ":force_quotes element must be " +
                    "field index or field name: " +
                    "#{name_or_index.inspect}: #{force_quotes.inspect}"
          raise ArgumentError, message
        end
      end
    end

    def prepare_format
      @column_separator = @options[:column_separator].to_s.encode(@encoding)
      row_separator = @options[:row_separator]
      if row_separator == :auto
        @row_separator = InputRecordSeparator.value.encode(@encoding)
      else
        @row_separator = row_separator.to_s.encode(@encoding)
      end
      @quote_character = @options[:quote_character]
      force_quotes = @options[:force_quotes]
      if force_quotes.is_a?(Array)
        prepare_force_quotes_fields(force_quotes)
        @force_quotes = false
      elsif force_quotes
        @force_quotes_fields = nil
        @force_quotes = true
      else
        @force_quotes_fields = nil
        @force_quotes = false
      end
      unless @force_quotes
        @quotable_pattern =
          Regexp.new("[\r\n".encode(@encoding) +
                     Regexp.escape(@column_separator) +
                     Regexp.escape(@quote_character.encode(@encoding)) +
                     "]".encode(@encoding))
      end
      @quote_empty = @options.fetch(:quote_empty, true)
    end

    def prepare_output
      @output_encoding = nil
      return unless @output.is_a?(StringIO)

      output_encoding = @output.internal_encoding || @output.external_encoding
      if @encoding != output_encoding
        if @options[:force_encoding]
          @output_encoding = output_encoding
        else
          compatible_encoding = Encoding.compatible?(@encoding, output_encoding)
          if compatible_encoding
            @output.set_encoding(compatible_encoding)
            @output.seek(0, IO::SEEK_END)
          end
        end
      end
    end

    def quote_field(field)
      field = String(field)
      encoded_quote_character = @quote_character.encode(field.encoding)
      encoded_quote_character +
        field.gsub(encoded_quote_character,
                   encoded_quote_character * 2) +
        encoded_quote_character
    end

    def prepare_fields(row, offset = 0)
      if @fields_converter
        row = @fields_converter.convert(row, nil, lineno)
      end

      i = offset - 1
      row.collect do |field|
        i += 1
        quote(field, i)
      end
    end

    def emit_line(line)
      if @output_encoding
        line = line.encode(@output_encoding)
      end
      @output << line
    end

    def quote(field, i)
      if @force_quotes
        quote_field(field)
      elsif @force_quotes_fields and @force_quotes_fields[i]
        quote_field(field)
      else
        if field.nil?  # represent +nil+ fields as empty unquoted fields
          ""
        else
          field = String(field)  # Stringify fields
          # represent empty fields as empty quoted fields
          if (@quote_empty and field.empty?) or (field.valid_encoding? and @quotable_pattern.match?(field))
            quote_field(field)
          else
            field  # unquoted field
          end
        end
      end
    end
  end
end
