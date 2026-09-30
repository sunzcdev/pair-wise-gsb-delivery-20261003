# frozen_string_literal: true

module GraphQL
  module StaticValidation
    # A resumable, pausable run of {StaticValidation} for a single query.
    #
    # Unlike {Validator#validate}, which runs every rule to completion (or
    # timeout) before returning, a session is driven by the caller one
    # **work budget** at a time. This lets an event loop interleave static
    # validation of several queries without any one of them holding the thread
    # for the whole document.
    #
    # The budget is measured in units of rule work that are fixed by the
    # document being checked, not in wall-clock time:
    #
    # - entering the visit hook for one AST node (which runs every rule
    #   callback defined for that kind of node) costs 1 unit;
    # - resolving one fragment dependency during the post-visit dependency
    #   pass costs 1 unit;
    # - each per-definition check in the post-visit passes (fragment spread
    #   scopes, fragment usage/liveness, variable usage, duplicate names, ...)
    #   costs 1 unit.
    #
    # Because the visit suspends only between these units -- after a node was
    # fully handled and before the next node's callbacks run -- splitting a
    # document into arbitrarily many {#advance} calls produces exactly the
    # same errors, in the same order, as {Validator#validate}. The intermediate
    # state of fragment-dependency resolution, field-merge bookkeeping,
    # variable tracking and every other rule lives on the session's visitor and
    # simply waits until the session resumes.
    #
    # Sessions are independent: each one owns its {ValidationContext}, visitor
    # and rule state, so interleaving two sessions can never leak context
    # between them.
    #
    # @example Running a session a little at a time
    #   session = validator.validation_session(query)
    #   until session.status != :pending
    #     result = session.advance(100) # at most 100 units of rule work
    #     # result[:status] is :pending, :complete or :cancelled
    #   end
    #   query_ok = session.complete? && session.errors.empty?
    class ValidationSession
      # The session may still do work on a later {#advance} call.
      PENDING = :pending
      # Every rule finished. {#errors} is the final error list.
      COMPLETE = :complete
      # {#cancel} was called. No rule callback runs again and errors are frozen.
      CANCELLED = :cancelled

      # @return [GraphQL::Query] the query being validated
      attr_reader :query

      # @return [:pending, :complete, :cancelled] the current state of the run
      attr_reader :status

      # @return [Array<GraphQL::StaticValidation::Error>] errors collected so far.
      #   This is only the final error list when the session is {COMPLETE}; while
      #   {PENDING} or {CANCELLED} it is a partial list and must never be taken
      #   as proof that the whole query is valid.
      def errors
        complete? ? final_errors : @context_errors
      end

      # @api private
      def initialize(query:, rules:, validate: true, max_errors: nil)
        @query = query
        @status = PENDING
        @errors = nil
        @execution_error = nil
        @current_budget = nil

        trace = query.current_trace
        trace.begin_validate(query, validate)
        @validate_trace_open = true
        @trace_validate = validate

        rules_to_use = validate ? rules : []
        if rules_to_use.empty?
          # Nothing to check (eg `validate: false`): match Validator#validate,
          # which returns an empty error list without running the visitor.
          @context = nil
          @context_errors = []
          @fiber = nil
          finish
        else
          visitor_class = BaseVisitor.including_rules(rules_to_use)
          @context = ValidationContext.new(query, visitor_class, max_errors, work_gate: self)
          @context_errors = @context.errors
          @fiber = build_fiber
        end
      end

      # @return [Boolean] true once every rule has run and the error list is final
      def complete?
        @status == COMPLETE
      end

      # @return [Boolean] true after {#cancel}
      def cancelled?
        @status == CANCELLED
      end

      # @return [Boolean] true while more {#advance} calls may run rules
      def pending?
        @status == PENDING
      end

      # Drive validation by at most `work_budget` units of rule work.
      #
      # The budget bounds the documented checks listed on the class: AST node
      # hook entries and the post-visit per-definition checks. It is checked
      # *before* each unit, so a budget of 0 runs no checks at all and simply
      # returns {PENDING}. A unit that has started always runs to its natural
      # end -- rules are never interrupted mid-callback.
      #
      # @param work_budget [Integer] maximum number of work units to run now
      # @return [Hash] `{ status: Symbol, errors: Array }`
      def advance(work_budget = 1)
        raise ArgumentError, "work_budget must be a non-negative Integer, got #{work_budget.inspect}" unless work_budget.is_a?(Integer) && work_budget >= 0

        case @status
        when COMPLETE
          return { status: COMPLETE, errors: final_errors }
        when CANCELLED
          return { status: CANCELLED, errors: @context_errors }
        end

        @current_budget = work_budget
        begin
          @query.current_trace.validate(validate: @trace_validate, query: @query) do
            @fiber.resume
          end
        ensure
          @current_budget = nil
        end

        if @fiber.alive?
          @status = PENDING
        else
          finish
        end

        { status: @status, errors: complete? ? final_errors : @context_errors }
      end

      # Stop the session permanently.
      #
      # The suspended run is never resumed, so no rule callback runs after
      # cancellation and its partial results are discarded: {#errors} keeps
      # whatever was collected so far purely for inspection, but a cancelled
      # session is never {#complete?} and can never be treated as a valid query.
      #
      # @return [Symbol] {CANCELLED}
      def cancel
        return @status unless @status == PENDING
        @status = CANCELLED
        close_validate_trace(@context_errors)
        @status
      end

      # Called by the running visitor through {ValidationContext#charge_work}
      # before each unit of rule work. Suspends the run when the current
      # budget is spent.
      # @api private
      def charge_work(_units = 1)
        if @current_budget
          @current_budget -= 1
          if @current_budget < 0
            # Budget for this call is exhausted; the next unit waits for the
            # next {#advance}. State on the visitor/context is left intact.
            Fiber.yield
          end
        end
        nil
      end

      private

      # Run the whole visit inside a Fiber, pausing at work checkpoints.
      # The fiber starts suspended before any rule runs, so the very first
      # {#advance} accounts for the first unit of work under its own budget
      # (a budget of 0 does no work at all).
      def build_fiber
        fiber = Fiber.new do
          begin
            catch(:too_many_validation_errors) do
              Fiber.yield(:ready)
              @context.visitor.visit
            end
          rescue GraphQL::ExecutionError => e
            @execution_error = e
          end
        end
        fiber.resume # reach the initial :ready checkpoint
        fiber
      end

      def finish
        @status = COMPLETE
        close_validate_trace(final_errors)
      end

      def final_errors
        @errors ||= @execution_error ? [@execution_error] : @context_errors
      end

      def close_validate_trace(errs)
        return unless @validate_trace_open
        @validate_trace_open = false
        @query.current_trace.end_validate(@query, @trace_validate, errs)
      end
    end
  end
end
