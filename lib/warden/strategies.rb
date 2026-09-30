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

      # Provides access to strategies by label
      # :api: public
      def [](label)
        _strategies[label]
      end

      # Register an all-of strategy group. All the given member strategies
      # must succeed and authenticate the same subject for the group to
      # succeed. The given block defines how two authenticated subjects are
      # compared; it is required because two Ruby objects representing the
      # same identity must not be assumed to be `==`.
      #
      # The group is registered as a strategy under the given label, so it
      # can be passed to `authenticate`, configured through
      # `default_strategies` and mixed with ordinary strategies:
      #
      #   Warden::Strategies.all_of(:password_and_totp,
      #                             :password, :totp) do |first, other|
      #     first.id == other.id
      #   end
      #
      #   env['warden'].authenticate(:password_and_totp)
      #
      # Parameters:
      #   label - The label used to refer to the group
      #   strategies - A list of member strategy labels
      #   block - Receives two authenticated subjects and must return true
      #           only when they represent the same identity
      #
      # :api: public
      def all_of(label, *strategies, &block)
        strategies.flatten!
        if strategies.empty?
          raise ArgumentError, "all_of strategy group #{label.inspect} needs at least one strategy"
        end
        unless block_given?
          raise ArgumentError, "all_of strategy group #{label.inspect} needs a block to compare subjects"
        end

        _all_of_strategies[label] =
          Warden::Strategies::AllOf::Group.new(strategies, block)
      end

      # Provides access to a registered all-of strategy group by label.
      # :api: private
      def _all_of(label)
        _all_of_strategies[label]
      end

      # Clears all declared.
      # :api: public
      def clear!
        _strategies.clear
        _all_of_strategies.clear
      end

      # :api: private
      def _strategies
        @strategies ||= {}
      end

      # :api: private
      def _all_of_strategies
        @all_of_strategies ||= {}
      end
    end # << self
  end # Strategies
end # Warden
