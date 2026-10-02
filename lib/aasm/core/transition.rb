# frozen_string_literal: true

module AASM::Core
  class Transition
    include AASM::DslHelper

    attr_reader :from, :to, :event, :opts, :failures
    alias_method :options, :opts

    def initialize(event, opts, &block)
      add_options_from_dsl(opts, [:on_transition, :guard, :after, :success], &block) if block

      @event = event
      @from = opts[:from]
      @to = opts[:to]
      @guards = Array(opts[:guards]) + Array(opts[:guard]) + Array(opts[:if])
      @unless = Array(opts[:unless]) #TODO: This could use a better name
      @failures = []

      if opts[:on_transition]
        warn '[DEPRECATION] :on_transition is deprecated, use :after instead'
        opts[:after] = Array(opts[:after]) + Array(opts[:on_transition])
      end
      @after = Array(opts[:after])
      @after = @after[0] if @after.size == 1

      @success = Array(opts[:success])
      @success = @success[0] if @success.size == 1

      @opts = opts
    end

    # called internally by Ruby 1.9 after clone()
    def initialize_copy(orig)
      super
      @guards = @guards.dup
      @unless = @unless.dup
      @opts   = {}
      orig.opts.each_pair { |name, setting| @opts[name] = setting.is_a?(Hash) || setting.is_a?(Array) ? setting.dup : setting }
    end

    def allowed?(obj, *args)
      target = reset_context_target(obj)
      result = invoke_callbacks_compatible_with_guard(@guards, obj, args, :guard => true) &&
               invoke_callbacks_compatible_with_guard(@unless, obj, args, :unless => true)
      target&.reset_event_transition_context unless result
      result
    rescue StandardError
      target&.reset_event_transition_context
      raise
    end

    def execute(obj, *args, selected_to: nil)
      set_transition_context(obj, selected_to)
      invoke_callbacks_compatible_with_guard(event.state_machine.global_callbacks[:after_all_transitions], obj, args)
      invoke_callbacks_compatible_with_guard(@after, obj, args)
    end

    def ==(obj)
      @from == obj.from && @to == obj.to
    end

    def from?(value)
      @from == value
    end

    def invoke_success_callbacks(obj, *args)
      _fire_callbacks(@success, obj, args)
    end

    private

    def invoke_callbacks_compatible_with_guard(code, record, args, options={}, selected_to=nil)
      if record.respond_to?(:aasm)
        set_transition_context(record, selected_to || @to)
      end

      Invoker.new(code, record, args)
             .with_options(options)
             .with_failures(failures)
             .invoke
    end

    def set_transition_context(record, selected_to=nil)
      return unless record.respond_to?(:aasm)

      target = transition_context_target(record)
      target.from_state = @from if target.respond_to?(:from_state=)
      target.to_state = selected_to || @to if target.respond_to?(:to_state=)
    end

    def transition_context_target(record)
      record.aasm(event.state_machine.name)
    end

    def reset_context_target(record)
      return unless record.respond_to?(:aasm)

      target = transition_context_target(record)
      target.respond_to?(:reset_event_transition_context) ? target : nil
    end

    def _fire_callbacks(code, record, args)
      Invoker.new(code, record, args).invoke
    end

  end
end # AASM
