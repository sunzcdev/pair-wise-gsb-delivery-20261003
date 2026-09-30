# encoding: utf-8
# frozen_string_literal: true
module Warden
  module Strategies
    class << self
      # Add a strategy and store it in a hash.
      def add(label, strategy = nil, &block)
        strategy ||= Class.new(Warden::Strategies::Base)
        strategy.class_eval(&block) if block_given?

        unless strategy.method_defined?(:authenticate!)
          raise NoMethodError, "authenticate! is not declared in the #{label.inspect} strategy"
        end

        base = Warden::Strategies::Base
        unless strategy.ancestors.include?(base)
          raise "#{label.inspect} is not a #{base}"
        end

        _strategies[label] = strategy
      end

      # Update a previously given strategy.
      def update(label, &block)
        strategy = _strategies[label]
        raise "Unknown strategy #{label.inspect}" unless strategy
        add(label, strategy, &block)
      end

      # Add an "all of" strategy group and store it in a hash.
      #
      # Every strategy in +strategy_names+ is run and must succeed, and all
      # of the resulting users must identify the same subject, before the
      # group logs anyone in.  The block is the application-provided subject
      # matcher: it receives two authenticated user objects and must return
      # true when they represent the same identity.
      #
      # Example:
      #
      #   Warden::Strategies.add_all_of(:two_factor, [:password, :otp]) do |user_a, user_b|
      #     user_a.id == user_b.id
      #   end
      #
      # :api: public
      def add_all_of(label, strategy_names, &subject_matcher)
        strategy_names = Array(strategy_names)

        if strategy_names.empty?
          raise ArgumentError, "all_of strategy #{label.inspect} requires at least one member strategy"
        end

        unless subject_matcher
          raise ArgumentError, "all_of strategy #{label.inspect} requires a subject matcher block"
        end

        strategy = Class.new(Warden::Strategies::AllOf)
        strategy.strategy_names = strategy_names
        strategy.subject_matcher = subject_matcher
        add(label, strategy)
      end

      # Provides access to strategies by label
      # :api: public
      def [](label)
        _strategies[label]
      end

      # Clears all declared.
      # :api: public
      def clear!
        _strategies.clear
      end

      # :api: private
      def _strategies
        @strategies ||= {}
      end
    end # << self
  end # Strategies
end # Warden
