# frozen-string-literal: true

require 'time'
require 'stringio'
require 'tomlrb/version'
require 'tomlrb/local_date_time'
require 'tomlrb/local_date'
require 'tomlrb/local_time'
require 'tomlrb/string_utils'
require 'tomlrb/scanner'
require 'tomlrb/stream_scanner'
require 'tomlrb/parser'
require 'tomlrb/handler'
require 'tomlrb/event_handler'

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

  # Reads TOML from an IO in chunks and yields parse events instead of
  # building the whole document as a nested Hash. Useful for consumers
  # such as configuration auditors that check values one by one and do
  # not want the whole file, nor the whole output tree, in memory.
  #
  # Events are arrays of one of these shapes:
  #
  #   [:table, path]            a [table] declaration was parsed
  #   [:array_of_tables, path]  a [[table]] declaration was parsed; each
  #                             event opens a new element of the array
  #   [:value, path, value]     a key/value pair was completed; path is the
  #                             full dotted path from the document root
  #
  # Protocol notes:
  #
  # * There is intentionally no "table closed" event. A table declaration
  #   only switches the context that following :value events belong to;
  #   it does not mean the previously current table is finished forever.
  #   A normal table may be declared *after* values under its path were
  #   already emitted ([a.b] ... [a]), and an array of tables may gain a
  #   new element (and new sub-tables) at any later [[...]] header.
  # * Only explicit [...]/[[...]] headers produce declaration events.
  #   Tables created implicitly on the way to a declared one do not.
  # * Nested containers (arrays, inline tables) are delivered as a single
  #   :value event once the container is complete.
  # * A value is yielded exactly once and never retained by the parser:
  #   the only state kept is what syntax and duplicate-declaration checks
  #   require, so processed values can be garbage collected.
  #
  # Errors that can only be detected at end of input (unterminated
  # string, truncated UTF-8) are raised as Tomlrb::ParseError once the IO
  # is exhausted.
  #
  # @param string_or_io [String, #read] the content, or an IO-like object
  #   responding to +read(n)+ and returning nil at end of input
  # @param chunk_size [Integer] how many bytes to pull from the IO at once
  # @param options [Hash] the options hash
  # @option options [Boolean] :symbolize_keys (false) whether event paths
  #   are arrays of symbols or arrays of strings
  # @yield [event] each parse event, in document order
  # @return [nil]
  def self.each_event(string_or_io, chunk_size: StreamScanner::DEFAULT_CHUNK_SIZE, **options, &block)
    raise ArgumentError, 'Tomlrb.each_event requires a block' unless block

    io = string_or_io.is_a?(String) ? StringIO.new(string_or_io) : string_or_io
    scanner = StreamScanner.new(io, chunk_size: chunk_size)
    handler = EventHandler.new(block, **options)
    parser = Parser.new(scanner, handler: handler)
    begin
      parser.parse
    rescue Racc::ParseError => e
      raise ParseError, e.message
    end

    nil
  end
end
