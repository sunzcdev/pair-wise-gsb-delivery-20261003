# frozen_string_literal: true

require "active_record/connection_adapters/query_intent"

module ActiveRecord
  class FutureResult # :nodoc:
    class Complete
      attr_reader :result
      delegate :empty?, :to_a, to: :result

      def initialize(result)
        @result = result
      end

      def pending?
        false
      end

      def canceled?
        false
      end

      # An already-materialized result cannot be withdrawn: there is no
      # background work to reclaim.
      def cancel
        false
      end

      def then(&block)
        Promise::Complete.new(@result.then(&block))
      end
    end

    Canceled = Class.new(ActiveRecordError)

    def self.wrap(result)
      case result
      when self, Complete
        result
      else
        Complete.new(result)
      end
    end

    delegate :empty?, :to_a, to: :result
    delegate :lock_wait, to: :@intent

    def initialize(intent)
      @intent = intent
    end

    def then(&block)
      Promise.new(self, block)
    end

    # Explicitly withdraws this future result before it is consumed.
    #
    # See QueryIntent#cancel for the full semantics: a query that has not
    # started never borrows a connection, while a running one is interrupted
    # through the adapter's native mechanism when available. Accessing #result
    # afterwards raises FutureResult::Canceled, keeping explicit withdrawals
    # distinct from genuine database errors.
    #
    # Returns +true+ when the intent was canceled by this call, +false+ when
    # it had already finished.
    def cancel
      @intent.cancel
    end

    def result
      raise Canceled if canceled?

      @intent.cast_result
    end

    def pending?
      @intent.pending?
    end

    def canceled?
      @intent.canceled? || @intent.session_finalized?
    end
  end
end
