# frozen_string_literal: true

module SolidCache
  class Store
    module Failsafe
      TRANSIENT_ACTIVE_RECORD_ERRORS = [
        ActiveRecord::AdapterTimeout,
        ActiveRecord::ConnectionNotEstablished,
        ActiveRecord::Deadlocked,
        ActiveRecord::LockWaitTimeout,
        ActiveRecord::QueryCanceled,
        ActiveRecord::StatementTimeout
      ]

      DEFAULT_ERROR_HANDLER = ->(method:, returning:, exception:) do
        if Store.logger
          Store.logger.error { "SolidCacheStore: #{method} failed, returned #{returning.inspect}: #{exception.class}: #{exception.message}" }
        end
      end

      def initialize(options = {})
        super(options)

        @error_handler = options.fetch(:error_handler, DEFAULT_ERROR_HANDLER)
      end

      private
        attr_reader :error_handler

        def failsafe(method, returning: nil)
          yield
        rescue *TRANSIENT_ACTIVE_RECORD_ERRORS => error
          handle_error(method: method, returning: returning, exception: error)
          returning
        end

        # Like #failsafe, but returns the error that was handled alongside the
        # fallback value, so callers can report on a failure instead of
        # silently treating it as an empty result.
        def failsafe_with_error(method, returning: nil)
          [ yield, nil ]
        rescue *TRANSIENT_ACTIVE_RECORD_ERRORS => error
          handle_error(method: method, returning: returning, exception: error)
          [ returning, error ]
        end

        def handle_error(method:, returning:, exception:)
          ActiveSupport.error_reporter&.report(exception, handled: true, severity: :warning)
          error_handler&.call(method: method, exception: exception, returning: returning)
        end
    end
  end
end
