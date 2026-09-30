# frozen_string_literal: true

require "dry/validation/constants"

module Dry
  module Validation
    # Verifies macro calls in contract rules against declared macro signatures
    #
    # Macros are resolved through the same scope chain that is used at
    # runtime: the contract's own macro container first (which includes
    # macros inherited from its ancestors), then the global registry. Macros
    # without a declared signature are skipped, preserving their historical
    # behavior.
    #
    # @see Macro::Signature
    #
    # @api private
    class MacroValidator
      # @api private
      attr_reader :contract_class

      # @api private
      def initialize(contract_class)
        @contract_class = contract_class
        @expanded = {}
      end

      # Validate all macro calls made by the contract's rules
      #
      # @raise [InvalidMacroCallError]
      #
      # @return [void]
      #
      # @api private
      def validate!
        contract_class.rules.each do |rule|
          rule.macros.each do |spec|
            name, *args = spec.flatten(1)
            macro = resolve_macro(name, rule)

            validate_call(macro, name, args, rule)
            validate_expansion(name, rule, [name])
          end
        end
      end

      private

      # @api private
      def validate_call(macro, name, args, rule)
        return unless macro.signature

        problems = macro.signature.validate_call(args)
        return if problems.empty?

        raise InvalidMacroCallError,
              "#{contract_name}.rule(#{rule_keys(rule)}) calls macro #{name.inspect} " \
              "with invalid arguments: #{problems.join(', ')}"
      end

      # Follow declared macro-to-macro expansions, making sure every expanded
      # macro resolves in the contract's scope and no expansion is circular
      #
      # @api private
      def validate_expansion(name, rule, path)
        return if @expanded.key?(name)

        macro = resolve_macro(name, rule)
        dependencies = macro.signature ? macro.signature.macros : EMPTY_ARRAY

        dependencies.each do |dependency|
          if path.include?(dependency)
            cycle = (path[path.index(dependency)..] + [dependency])
                    .map { |step| step.inspect }
                    .join(" -> ")

            raise InvalidMacroCallError,
                  "#{contract_name}.rule(#{rule_keys(rule)}) expands macro #{path.first.inspect} " \
                  "into a circular chain: #{cycle}"
          end

          validate_expansion(dependency, rule, [*path, dependency])
        end

        @expanded[name] = true
      end

      # Resolve a macro the same way `Contract#macro` does at runtime
      #
      # @api private
      def resolve_macro(name, rule)
        container = contract_class.macros
        return container[name] if container.key?(name)
        return Macros.container[name] if Macros.container.key?(name)

        raise InvalidMacroCallError,
              "#{contract_name}.rule(#{rule_keys(rule)}) uses macro #{name.inspect} " \
              "which is not registered"
      end

      # @api private
      def contract_name
        contract_class.name || contract_class.inspect
      end

      # @api private
      def rule_keys(rule)
        rule.keys.map(&:inspect).join(", ")
      end
    end
  end
end
