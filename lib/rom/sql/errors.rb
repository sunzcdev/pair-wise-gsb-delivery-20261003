# frozen_string_literal: true

require 'sequel/deprecated'
require 'sequel/exceptions'

require 'rom/sql/error'

module ROM
  module SQL
    MissingConfigurationError = Class.new(StandardError)
    NoAssociationError        = Class.new(StandardError)
    DatabaseError             = Class.new(Error)
    ConstraintError           = Class.new(Error)
    NotNullConstraintError    = Class.new(ConstraintError)
    UniqueConstraintError     = Class.new(ConstraintError)
    ForeignKeyConstraintError = Class.new(ConstraintError)
    CheckConstraintError      = Class.new(ConstraintError)
    UnknownDBTypeError        = Class.new(StandardError)
    MissingPrimaryKeyError    = Class.new(StandardError)
    MigrationError            = Class.new(StandardError)
    UnsupportedConversion     = Class.new(MigrationError)

    # Raised when a `.sql` migration cannot be lexed/parsed. When the failure
    # maps to a source position, `file`, `line` (1-based) and `column`
    # (1-based) are provided; they are `nil` for errors without one.
    class SQLParseError < MigrationError
      attr_reader :file, :line, :column

      def initialize(message = nil, file: nil, line: nil, column: nil)
        @file = file
        @line = line
        @column = column
        super(message)
      end

      # 0-based offset matching `column`.
      def pos
        column - 1 if column
      end

      # `[file, line, column]` when all coordinates are known.
      def source_location
        [file, line, column] if file && line && column
      end
    end

    SQLEnvError               = Class.new(MigrationError)

    ERROR_MAP = {
      Sequel::DatabaseError => DatabaseError,
      Sequel::ConstraintViolation => ConstraintError,
      Sequel::NotNullConstraintViolation => NotNullConstraintError,
      Sequel::UniqueConstraintViolation => UniqueConstraintError,
      Sequel::ForeignKeyConstraintViolation => ForeignKeyConstraintError,
      Sequel::CheckConstraintViolation => CheckConstraintError
    }.freeze
  end
end
