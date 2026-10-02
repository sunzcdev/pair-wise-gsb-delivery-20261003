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
      invoke_callbacks_compatible_with_guard(@guards, obj, args, :guard => true, :keyword_arguments => keyword_arguments_flag?(args)) &&
      invoke_callbacks_compatible_with_guard(@unless, obj, args, :unless => true, :keyword_arguments => keyword_arguments_flag?(args))
    end
    ruby2_keywords :allowed?

    def execute(obj, *args)
      invoke_callbacks_compatible_with_guard(event.state_machine.global_callbacks[:after_all_transitions], obj, args, keyword_arguments: keyword_arguments_flag?(args))
      invoke_callbacks_compatible_with_guard(@after, obj, args, keyword_arguments: keyword_arguments_flag?(args))
    end
    ruby2_keywords :execute

    def ==(obj)
      @from == obj.from && @to == obj.to
    end

    def from?(value)
      @from == value
    end

    def invoke_success_callbacks(obj, *args)
      _fire_callbacks(@success, obj, args, keyword_arguments: keyword_arguments_flag?(args))
    end
    ruby2_keywords :invoke_success_callbacks

    private

    def invoke_callbacks_compatible_with_guard(code, record, args, options={})
      if record.respond_to?(:aasm)
        record.aasm(event.state_machine.name).from_state = @from if record.aasm(event.state_machine.name).respond_to?(:from_state=)
        record.aasm(event.state_machine.name).to_state = @to if record.aasm(event.state_machine.name).respond_to?(:to_state=)
      end

      Invoker.new(code, record, args, keyword_arguments: options.fetch(:keyword_arguments, false))
             .with_options(options)
             .with_failures(failures)
             .invoke
    end
    def _fire_callbacks(code, record, args, keyword_arguments: false)
      Invoker.new(code, record, args, keyword_arguments: keyword_arguments).invoke
    end

    def keyword_arguments_flag?(args)
      args.last.is_a?(Hash) && Hash.ruby2_keywords_hash?(args.last)
    end

  end
end # AASM
