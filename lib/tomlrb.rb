# frozen-string-literal: true

require 'time'
require 'stringio'
require 'tomlrb/version'
require 'tomlrb/local_date_time'
require 'tomlrb/local_date'
require 'tomlrb/local_time'
require 'tomlrb/string_utils'
require 'tomlrb/scanner'
require 'tomlrb/streaming_scanner'
require 'tomlrb/parser'
require 'tomlrb/event_parser'
require 'tomlrb/handler'

module Tomlrb
  class ParseError < StandardError; end

  # Parses a valid TOML string into its Ruby data structure
  #
  # @param string_or_io [String, StringIO] the content
  # @param options [Hash] the options hash
  # @option options [Boolean] :symbolize_keys (false) whether to return the keys as symbols or strings
  # @return [Hash] the Ruby data structure represented by the input
  def self.parse(string_or_io, **options)
    io = string_or_io.is_a?(String) ? StringIO.new(string_or_io) : string_or_io
    scanner = Scanner.new(io)
    parser = Parser.new(scanner, **options)
    begin
      handler = parser.parse
    rescue Racc::ParseError => e
      raise ParseError, e.message
    end

    handler.output
  end

  # Reads a file content and parses it into its Ruby data structure
  #
  # @param path [String] the path to the file
  # @param options [Hash] the options hash
  # @option options [Boolean] :symbolize_keys (false) whether to return the keys as symbols or strings
  # @return [Hash] the Ruby data structure represented by the input
  def self.load_file(path, **options)
    # By default Ruby sets the external encoding of an IO object to the
    # default external encoding. The default external encoding is set by
    # locale encoding or the interpreter -E option.
    tmp = File.read(path, encoding: 'utf-8')
    Tomlrb.parse(tmp, **options)
  end

  # Streams structural events for a TOML document that is read from a chunked
  # IO, without materializing the document or its output Hash.
  #
  # Events are arrays, see {Tomlrb::EventHandler} for the retained-state
  # guarantees. The event sequence is documented in README.md:
  #
  # - [:table_declare, path, kind] - a table header; kind is +:standard+ or
  #   +:array_element+, path strings with integer indices for array elements
  # - [:table_enter, path, mode] - the active table becomes +path+; mode is
  #   +:standard+, +:array_element+ or +:implicit+ (parent created by a header)
  # - [:table_leave, path] - the active table is switched away from +path+;
  #   the table is not closed forever and may be re-entered (via a parent
  #   header) or extended later (array-of-tables appends another element)
  # - [:key, path, value] - a completed root/table scalar pair
  # - [:array_start, path] / [:array_end, path] - a value array, streamed
  # - [:array_element, path, value] - an array scalar (indices in path)
  # - [:inline_table_start, path] / [:inline_table_end, path]
  # - [:inline_table_entry, path, value] - a completed inline scalar pair
  #
  # Container nesting follows paths: a nested container's path extends its
  # parent's path, and array elements use integer segments while table and
  # inline-table keys use string segments.
  #
  # Chunk boundaries may split UTF-8 sequences, escapes or (multi-line)
  # strings; errors only decidable at EOF raise {ParseError} at the end.
  #
  # @param io [#read, #getbyte] a chunked IO delivering the document bytes
  # @param options [Hash] the options hash
  # @option options [Boolean] :symbolize_keys (false) symbolize key segments
  # @option options [Integer] :chunk_size (4096) bytes requested per IO read
  # @yieldparam event [Array] each structural event
  # @return [Enumerator] when no block is given
  def self.each_event(io, **options, &block)
    return enum_for(:each_event, io, **options) unless block

    chunk_size = options.delete(:chunk_size) { 4096 }
    scanner = StreamingScanner.new(io, chunk_size: chunk_size)
    parser = EventParser.new(scanner, consumer: block, **options)
    begin
      parser.parse
    rescue Racc::ParseError => e
      raise ParseError, e.message
    end
    nil
  end
end
