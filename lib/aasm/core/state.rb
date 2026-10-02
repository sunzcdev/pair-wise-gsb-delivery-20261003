# frozen_string_literal: true

module AASM::Core
  class State
    attr_reader :name, :state_machine, :options, :default_display_name

    def initialize(name, klass, state_machine, options={})
      @name = name
      @klass = klass
      @state_machine = state_machine
      @default_display_name = name.to_s.gsub(/_/, ' ').capitalize
      update(options)
    end

    # called internally by Ruby 1.9 after clone()
    def initialize_copy(orig)
      super
      @options = {}
      orig.options.each_pair do |name, setting|
        @options[name] = if setting.is_a?(Hash) || setting.is_a?(Array)
                           setting.dup
                         else
                           setting
                         end
      end
    end

    def ==(state)
      if state.is_a? Symbol
        name == state
      else
        name == state.name
      end
    end

    def <=>(state)
      if state.is_a? Symbol
        name <=> state
      else
        name <=> state.name
      end
    end

    def to_s
      name.to_s
    end

    def fire_callbacks(action, record, *args)
      action = @options[action]
      catch :halt_aasm_chain do
        action.is_a?(Array) ?
                action.each {|a| _fire_callbacks(a, record, args, keyword_arguments: keyword_arguments_flag?(args))} :
                _fire_callbacks(action, record, args, keyword_arguments: keyword_arguments_flag?(args))
      end
    end
    ruby2_keywords :fire_callbacks

    def display_name
      @display_name = begin
        if Module.const_defined?(:I18n)
          localized_name
        else
          @default_display_name
        end
      end
    end

    def localized_name
      AASM::Localizer.new.human_state_name(@klass, self)
    end
    alias human_name localized_name

    def for_select
      [display_name, name.to_s]
    end

  private

    def update(options = {})
      if options.key?(:display)
        @default_display_name = options.delete(:display)
      end
      @options = options
      self
    end

    def _fire_callbacks(action, record, args, keyword_arguments: false)
      Invoker.new(action, record, args, keyword_arguments: keyword_arguments).invoke
    end

    def keyword_arguments_flag?(args)
      args.last.is_a?(Hash) && Hash.ruby2_keywords_hash?(args.last)
    end

  end
end # AASM
