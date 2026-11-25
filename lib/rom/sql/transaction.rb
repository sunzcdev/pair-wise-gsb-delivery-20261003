# frozen_string_literal: true

module ROM
  module SQL
    # @api private
    class Transaction < ::ROM::Transaction
      # Run a block atomically
      #
      # When a transaction is already open the block runs inside a savepoint,
      # so a failure rolls back only the block and leaves the outer
      # transaction usable. Otherwise a new transaction is opened.
      #
      # Errors are not translated here, they are raised to the caller.
      #
      # @param [Sequel::Database] connection The connection to use
      #
      # @return [Object] The value returned by the block
      #
      # @api public
      def self.run(connection, **opts, &)
        options = connection.in_transaction? ? opts.merge(savepoint: true) : opts

        connection.transaction(options) { yield }
      end

      attr_reader :connection
      private :connection

      def initialize(connection)
        super()
        @connection = connection
      end

      def run(opts = EMPTY_HASH)
        connection.transaction(opts) { yield(self) }
      rescue ::ROM::Transaction::Rollback
        # noop
      end
    end
  end
end
