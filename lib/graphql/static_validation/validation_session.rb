# frozen_string_literal: true
module GraphQL
  module StaticValidation
    # A static validation session which can be driven incrementally by an event
    # loop (a Fiber, a scheduler tick, a request-handler turn, ...) instead of
    # blocking the thread for the whole document.
    #
    # The caller hands each {#advance} call a **work budget** measured in discrete
    # units of checking work:
    #
    # - one unit for running the validation rules on one AST node,
    # - one unit per step of post-visit fragment-dependency resolution,
    # - one unit per field-merge pair comparison,
    # - and so on for other bounded, explicitly-counted rule work.
    #
    # Pausing therefore follows the amount of rule checking actually performed;
    # it never waits until the whole document is visited to look at a clock.
    #
    # When a budget is spent, the running visit suspends itself (Fiber) exactly
    # where it is -- deep inside fragment-dependency resolution or a field-merge
    # comparison if that is where the work was happening. The partial state
    # (visited prefix, accumulated fragment dependencies, merging field maps,
    # collected errors) stays alive on the session and continues on the next
    # {#advance} call.
    #
    # Each session owns its own query, {ValidationContext}, visitor and fiber,
    # so two sessions advanced in an interleaved fashion can never share or
    # clobber each other's state.
    #
    # @example Validate in turns of at most 100 work units
    #   session = schema.static_validator.validation_session(query)
    #   result = nil
    #   while (result = session.advance(work_units: 100)).unfinished?
    #     # ...give other work a turn...
    #   end
    #   if result.valid?
    #     # only a COMPLETED, error-free result means the query is valid
    #   end
    #
    class ValidationSession
      # The visit still has work left; more {#advance} calls are required.
      UNFINISHED = :unfinished
      # Every rule finished for the whole document. Check {Result#valid?}.
      COMPLETED = :completed
      # {#cancel} was called; no rule callback runs after cancellation.
      CANCELLED = :cancelled

      STATUSES = [UNFINISHED, COMPLETED, CANCELLED].freeze

      # @return [GraphQL::Query] the query being validated
      attr_reader :query
      # @return [GraphQL::StaticValidation::ValidationContext] the session-private context
      attr_reader :context
      # @return [Symbol] one of {STATUSES}
      attr_reader :status

      # @param query [GraphQL::Query]
      # @param rules [Array<#validate(context)>] rules to visit with
      # @param validate [Boolean] when `false`, no rules run and the session completes empty
      # @param max_errors [Integer, nil] abort the visit after this many errors
      def initialize(query, rules: GraphQL::StaticValidation::ALL_RULES, validate: true, max_errors: nil)
        @query = query
        @validate = validate
        rules = [] unless validate

        @status = UNFINISHED
        @remaining_work = 0
        @fiber = nil
        @execution_error = nil
        @trace_open = false

        visitor_class = PausableBaseVisitor.including_pausable_rules(rules)
        @context = GraphQL::StaticValidation::ValidationContext.new(query, visitor_class, max_errors, session: self)
      end

      # @return [Boolean] more {#advance} calls are needed
      def unfinished?
        @status == UNFINISHED
      end

      # @return [Boolean] the whole document finished validating
      def completed?
        @status == COMPLETED
      end

      # @return [Boolean] the session was cancelled
      def cancelled?
        @status == CANCELLED
      end

      # @return [Boolean] `true` once no further rule work will happen
      def finished?
        @status != UNFINISHED
      end

      # Cancel the session. After this returns:
      #
      # - {#advance} is a no-op which returns a {CANCELLED} result;
      # - if a fiber is suspended inside rule callbacks, it never resumes and no
      #   further rule callbacks (including dependency-resolve callbacks) run;
      # - errors collected before the cancellation remain available on the
      #   result, but a cancelled session is never treated as a valid query.
      #
      # @return [Result]
      def cancel
        unless finished?
          finalize(CANCELLED)
          close_trace
        end
        result
      end

      # Perform up to `work_units` units of validation work in this turn.
      #
      # @param work_units [Integer] non-negative work budget for this turn
      # @return [Result] the state after this turn
      def advance(work_units: 1)
        if work_units < 0
          raise ArgumentError, "work_units must be greater than or equal to 0, got #{work_units.inspect}"
        end

        return result if finished?

        @remaining_work = work_units
        resume_visit
        result
      end

      # Called by {ValidationContext#consume_work} from inside the driving
      # fiber. Charges `work_units` and suspends the fiber (yielding back to the
      # {#advance} caller) when the turn's budget is spent, or unwinds the visit
      # when the session was cancelled.
      # @api private
      def consume_work(work_units = 1)
        # A callback may cancel the session from the inside.
        if @status == CANCELLED
          throw(:validation_session_cancelled)
        end
        @remaining_work -= work_units
        if @remaining_work < 0
          Fiber.yield
          # Resumed for another turn; {#advance} reset the budget. Bail if the
          # session was cancelled while suspended.
          if @status == CANCELLED
            throw(:validation_session_cancelled)
          end
        end
      end

      # Errors gathered so far. A raised {GraphQL::ExecutionError} replaces
      # partial errors, matching {Validator#validate}.
      # @api private
      def errors
        if @execution_error
          [@execution_error]
        else
          @context.errors
        end
      end

      # Snapshot of the session after an {#advance} turn.
      class Result
        # @return [Symbol] one of {ValidationSession::STATUSES}
        attr_reader :status
        # @return [Array<GraphQL::StaticValidation::Error, GraphQL::ExecutionError>]
        attr_reader :errors

        def initialize(session)
          @status = session.status
          @errors = session.errors
        end

        def unfinished?
          @status == UNFINISHED
        end

        def completed?
          @status == COMPLETED
        end

        def cancelled?
          @status == CANCELLED
        end

        # @return [Boolean] `true` once no further rule work will happen
        def finished?
          @status != UNFINISHED
        end

        # A whole query is valid only when validation ran to completion with no
        # errors. An unfinished (partially passed) or cancelled session is never
        # valid, even if every fragment seen so far passed.
        def valid?
          @status == COMPLETED && @errors.empty?
        end
      end

      private

      # @return [Result]
      def result
        Result.new(self)
      end

      def finalize(status)
        @status = status
      end

      def resume_visit
        trace = @query.current_trace
        unless @trace_open
          trace.begin_validate(@query, @validate)
          @trace_open = true
        end

        # `validate: false` runs no rules at all (matches Validator#validate);
        # the session completes immediately on its first turn, budget or not.
        if !@validate
          finalize(COMPLETED)
          close_trace
          return
        end

        if @fiber.nil?
          @fiber = Fiber.new { run_visit }
        end

        begin
          @fiber.resume
        rescue GraphQL::ExecutionError => e
          @execution_error = e
          finalize(COMPLETED)
        end

        if finished?
          close_trace
        end
      end

      def close_trace
        if @trace_open
          @query.current_trace.end_validate(@query, @validate, errors)
          @trace_open = false
        end
      end

      # Runs entirely inside the driving fiber.
      def run_visit
        catch(:too_many_validation_errors) do
          catch(:validation_session_cancelled) do
            @context.visitor.visit
          end
        end
        # Normal completion and the max-errors abort both leave a deterministic
        # final set of errors and therefore count as completed. A mid-callback
        # cancellation already set the status to CANCELLED and must stay that
        # way so no further rule code is treated as having run.
        if @status == UNFINISHED
          finalize(COMPLETED)
        end
      end
    end
  end
end
