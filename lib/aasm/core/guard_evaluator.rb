# frozen_string_literal: true

module AASM
  module Core
    # Evaluates the guards of candidate transitions within a single
    # availability inspection.
    #
    # One evaluator is used for exactly one inspection of one record, so that
    # every guard subject (symbol, string, proc, class or a nested array) is
    # invoked at most once, even when an event level guard is attached to
    # several candidate transitions. The memoized results never leave this
    # instance and must not be reused as authorization for a later fire.
    class GuardEvaluator
      # Branch-aware failure identifier. Invoker logs whenever a callback
      # returns a falsey value without knowing whether the subject is used as
      # a guard (falsey fails) or as an :unless callback (truthy fails), so it
      # cannot be used to derive rejection reasons.
      def self.failure_identifier(subject)
        if subject.is_a?(Symbol) || subject.is_a?(String)
          subject
        elsif subject.is_a?(Proc) && subject.respond_to?(:source_location)
          subject.source_location.join('#')
        elsif subject.is_a?(Class) && subject.instance_methods.include?(:call) &&
              subject.instance_method(:call).respond_to?(:source_location)
          subject.instance_method(:call).source_location.join('#')
        else
          subject
        end
      end

      def initialize(record, state_machine_name, args)
        @record = record
        @state_machine_name = state_machine_name
        @args = args
        @results = {}
      end

      # Returns a hash describing the outcome for one candidate transition:
      #
      #   {
      #     allowed: true/false,
      #     checks:  [ {subject:, branch:, status:, result:}, ... ]
      #   }
      #
      # Guards are evaluated in definition order: guards (with :if semantics)
      # first, then :unless guards. Each list short-circuits on its first
      # failing subject; subjects never reached this way are reported as
      # :skipped rather than being recorded as false.
      def evaluate(transition)
        set_transition_context(transition)

        guards_ok, guard_checks = evaluate_list(transition.guards, :guard)
        if guards_ok
          unless_ok, unless_checks = evaluate_list(transition.unless_guards, :unless)
        else
          # The existing short-circuit rule: failing guards skip the whole
          # :unless list (see Transition#allowed?).
          unless_checks = skip_list(transition.unless_guards, :unless)
          unless_ok = false
        end

        {
          :allowed => guards_ok && unless_ok,
          :checks => guard_checks + unless_checks
        }
      end

      private

      def evaluate_list(subjects, branch)
        checks = []
        passed = true
        Array(subjects).each do |subject|
          unless passed
            checks.concat(skip_list(Array(subject), branch))
            next
          end

          if nested_array?(subject)
            ok, nested_checks = evaluate_list(subject, branch)
            checks.concat(nested_checks)
            passed = ok
            next
          end

          status, result = invoke_with_memo(subject, branch)
          checks << build_check(subject, branch, status, result)
          passed = branch == :guard ? truthy?(result) : !truthy?(result)
        end
        [passed, checks]
      end

      def skip_list(subjects, branch)
        Array(subjects).flat_map do |subject|
          if nested_array?(subject)
            skip_list(subject, branch)
          else
            [build_check(subject, branch, :skipped, nil)]
          end
        end
      end

      def invoke_with_memo(subject, branch)
        if @results.key?(subject)
          result = @results[subject]
          status = :reused
        else
          result = invoke(subject)
          @results[subject] = result
          status = :invoked
        end

        [status, result]
      end

      def invoke(subject)
        Invoker.new(subject, @record, @args).invoke
      end

      def build_check(subject, branch, status, result)
        {
          :subject => subject,
          :branch => branch,
          :status => status,
          :result => status == :skipped ? :not_evaluated : result
        }
      end

      def set_transition_context(transition)
        return unless @record.respond_to?(:aasm)

        aasm = @record.aasm(@state_machine_name)
        aasm.from_state = transition.from if aasm.respond_to?(:from_state=)
        aasm.to_state = transition.to if aasm.respond_to?(:to_state=)
      end

      def nested_array?(subject)
        # Arrays are handled by Invoker#invoke_array internally; only plain
        # callback subjects are invoked (and memoized) directly.
        subject.is_a?(Array)
      end

      def truthy?(value)
        !!value
      end
    end
  end
end
