# frozen-string-literal: true

module Tomlrb
  class ParseError < StandardError; end

  Location = Struct.new(:line, :column) do
    def to_s
      return 'unknown location' if line.nil?

      "line #{line}, column #{column}"
    end
  end

  KeyPart = Struct.new(:name, :location) do
    def to_s
      name
    end
  end

  # Structured origin of a single key declaration.
  #
  # +kind+ is one of:
  # - +:value+            a key/value pair (the terminal key of an assignment)
  # - +:explicit_table+   a table header ([a.b] / [[a.b]])
  # - +:implicit_table+   an intermediate component of a dotted path
  # - +:inline_table+     a key whose value is an inline table ({ ... })
  Source = Struct.new(:kind, :location) do
    KIND_DESCRIPTIONS = {
      value: 'a value',
      explicit_table: 'an explicit table',
      implicit_table: 'an implicit parent table',
      inline_table: 'a closed inline table'
    }.freeze

    def description
      KIND_DESCRIPTIONS.fetch(kind)
    end

    def to_s
      "#{description} at #{location}"
    end
  end
end
