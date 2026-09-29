# frozen_string_literal: true

require "dry/validation/constants"
require "dry/validation/rule_selection"

module Dry
  module Validation
    # Compiles a purpose-specific, ordered selection of rules
    #
    # The compiler turns the full set of rules defined by a contract into the
    # subset that applies to a given combination of purposes. It:
    #
    # * keeps base rules (rules without a purpose tag),
    # * selects rules tagged with one of the requested purposes,
    # * forces rules declared mandatory for a requested purpose,
    # * pulls in explicit dependencies transitively,
    # * orders the result so that dependencies always run first.
    #
    # Selection and ordering are derived from the contract definition rather
    # than from call order.
    #
    # @api private
    class RuleCompiler
      # @!attribute [r] rules
      #   @return [Array<Rule>]
      #   @api private
      attr_reader :rules

      # @api private
      def initialize(rules)
        @rules = rules
      end

      # Build a {RuleSelection} for the given purposes
      #
      # @param [Array<Symbol>] purposes
      #
      # @return [RuleSelection]
      #
      # @api private
      def compile(*purposes)
        active = purposes.flatten.map(&:to_sym).uniq

        validate_dependencies!

        included = expand_dependencies(seed(active))
        ordered = topo_sort(included)

        chosen = ordered.each_with_object({}) { |rule, map| map[rule.object_id] = true }

        omitted = rules.reject { |rule| chosen.key?(rule.object_id) }.map { |rule|
          RuleSelection::Entry.new(rule: rule, reason: :purpose)
        }

        RuleSelection.new(
          purposes: active,
          applied: ordered.map { |rule| RuleSelection::Entry.new(**included[rule]) },
          omitted: omitted
        )
      end

      private

      # Rules directly selected by base membership, purpose tag or mandatory flag
      #
      # @return [Hash{Rule => Hash}]
      #
      # @api private
      def seed(active)
        rules.each_with_object({}) do |rule, selected|
          if rule.base?
            add_meta(selected, rule, reason: :base)
          elsif (rule.purposes.map(&:to_sym) & active).any?
            add_meta(selected, rule, reason: :purpose)
          elsif (rule.mandatory.map(&:to_sym) & active).any?
            add_meta(selected, rule, reason: :mandatory)
          end
        end
      end

      # Transitively pull in every dependency of the selected rules
      #
      # A dependency on a key pulls in every rule sharing that root key.
      #
      # @return [Hash{Rule => Hash}]
      #
      # @api private
      def expand_dependencies(seed_selection)
        selected = seed_selection
        by_id = group_by_id(rules)
        queue = selected.keys.dup

        until queue.empty?
          rule = queue.shift

          rule.depends_on.each do |dep_id|
            by_id.fetch(dep_id).each do |dep_rule|
              next if selected.key?(dep_rule)

              selected[dep_rule] = {
                rule: dep_rule,
                reason: :dependency,
                source: rule.id
              }
              queue << dep_rule
            end
          end
        end

        selected
      end

      # Order rules preserving definition order while putting dependencies first
      #
      # @return [Array<Rule>]
      #
      # @api private
      def topo_sort(included)
        order = rules.each_with_index.to_h { |rule, index| [rule.object_id, index] }
        remaining = rules.select { |rule| included.key?(rule) }

        resolved = []
        resolved_ids = []

        until remaining.empty?
          ready = remaining.select { |rule| dependencies_satisfied?(rule, included, resolved_ids) }

          if ready.empty?
            ids = remaining.map(&:inspect).inspect
            raise CircularDependencyError, "circular rule dependency among #{ids}"
          end

          nxt = ready.min_by { |rule| order[rule.object_id] }
          resolved << nxt
          resolved_ids << nxt.object_id
          remaining.delete(nxt)
        end

        resolved
      end

      # Check whether every included dependency of a rule has already run
      #
      # @return [Boolean]
      #
      # @api private
      def dependencies_satisfied?(rule, included, resolved_ids)
        rule.depends_on.all? { |dep_id|
          included.keys
                  .select { |candidate| candidate.id == dep_id }
                  .all? { |dep| resolved_ids.include?(dep.object_id) }
        }
      end

      # Ensure every declared dependency points at a rule that exists
      #
      # @api private
      def validate_dependencies!
        by_id = group_by_id(rules)

        rules.each do |rule|
          unknown = rule.depends_on.reject { |dep_id| by_id.key?(dep_id) }

          next if unknown.empty?

          raise InvalidDependencyError,
                "#{rule.inspect} depends on unknown rule(s): #{unknown.inspect}"
        end
      end

      # Record a rule keeping the strongest (lowest priority number) reason
      #
      # @api private
      def add_meta(selected, rule, reason:, source: nil)
        priority = {base: 0, purpose: 1, mandatory: 2, dependency: 3}

        if selected.key?(rule)
          return if priority.fetch(reason) >= priority.fetch(selected[rule][:reason])
        end

        selected[rule] = {rule: rule, reason: reason, source: source}
      end

      # Group rules by their stable root-key identifier
      #
      # @return [Hash{Symbol => Array<Rule>}]
      #
      # @api private
      def group_by_id(list)
        list.each_with_object(Hash.new { |hash, key| hash[key] = [] }) do |rule, map|
          map[rule.id] << rule
        end
      end
    end
  end
end
