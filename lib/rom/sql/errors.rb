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
    SQLParseError             = Class.new(MigrationError)
    SQLEnvError               = Class.new(MigrationError)

    # Raised when a relation expression cannot be pushed down to the
    # gateway's connection and no local fallback was declared
    UnsupportedSQLError       = Class.new(StandardError)

    # Raised when a declared local fallback would have to pull more
    # tuples than the relation is allowed to buffer
    FallbackExceededError     = Class.new(StandardError)

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
