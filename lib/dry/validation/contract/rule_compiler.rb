# frozen_string_literal: true

require "dry/initializer"
require "dry/validation/constants"

module Dry
  module Validation
    class Contract
      # Compiles the set of rules that apply to a given combination of purposes
      #
      # Compilation happens per contract class and per purpose combination. It is
      # declarative: rule eligibility, cross-purpose dependencies and execution
      # order are all resolved here instead of relying on call order.
      #
      # @api private
      class RuleCompiler
        # @api private
        def initialize(contract_class)
          @contract_class = contract_class
        end

        # Build an immutable rule selection for the requested purposes
        #
        # @param [Array<Symbol>, Symbol, nil] requested
        #   `nil` means the universal combination (every rule is eligible).
        #
        # @return [RuleSelection]
        #
        # @api private
        def compile(requested)
          all_rules = contract_class.rules

          validate_rule_purposes!(all_rules)

          if requested.nil?
            build_universal(all_rules)
          else
            build_explicit(all_rules, normalize(requested))
          end
        end

        private

        # @api private
        attr_reader :contract_class

        # Every rule is eligible; dependencies only determine the ordering
        #
        # @api private
        def build_universal(all_rules)
          entries = topo_sort(all_rules, all_rules).map { |rule|
            RuleEntry.new(
              rule: rule,
              reason: rule.purposes.empty? ? :base : :purpose,
              purposes: rule.purposes,
              required_by: EMPTY_ARRAY
            )
          }

          RuleSelection.new(
            requested_purposes: nil,
            effective_purposes: registered_purposes,
            selected_entries: entries,
            omitted: EMPTY_ARRAY
          )
        end

        # Select rules for an explicit purpose combination
        #
        # @api private
        def build_explicit(all_rules, requested)
          validate_requested_purposes!(requested)

          effective = expand_purposes(requested)

          eligible, = all_rules.partition do |rule|
            rule.purposes.empty? || (rule.purposes & effective).any?
          end

          selected = resolve_dependencies(eligible, all_rules)
          index = index_rules(all_rules)

          entries = topo_sort(selected, all_rules).map { |rule|
            matched = rule.purposes & effective

            if rule.purposes.empty?
              RuleEntry.new(rule: rule, reason: :base, purposes: EMPTY_ARRAY,
                            required_by: EMPTY_ARRAY)
            elsif matched.any?
              RuleEntry.new(rule: rule, reason: :purpose, purposes: matched,
                            required_by: EMPTY_ARRAY)
            else
              RuleEntry.new(rule: rule, reason: :dependency, purposes: rule.purposes,
                            required_by: required_by(rule, selected, index))
            end
          }

          omitted = (all_rules - selected).map do |rule|
            RuleEntry.new(rule: rule, reason: :omitted, purposes: rule.purposes,
                          required_by: EMPTY_ARRAY)
          end

          RuleSelection.new(
            requested_purposes: requested,
            effective_purposes: effective,
            selected_entries: entries,
            omitted: omitted
          )
        end

        # Pull in every rule referenced through `depends_on`, across purposes
        #
        # @api private
        def resolve_dependencies(eligible, all_rules)
          index = index_rules(all_rules)
          selected = eligible.dup
          queue = eligible.dup

          until queue.empty?
            rule = queue.shift

            rule.depends_on.each do |dependency_name|
              dependency = resolve(dependency_name, rule, index)

              next if selected.include?(dependency)

              selected << dependency
              queue << dependency
            end
          end

          selected
        end

        # Names of the selected rules that pulled a dependency-only rule in
        #
        # @api private
        def required_by(dependency, selected, index)
          selected.filter_map { |rule|
            next if rule.equal?(dependency)

            rule.id if rule.depends_on.any? { |name|
              candidates = index.fetch(name) { [] }
              candidates.one? && candidates.first.equal?(dependency)
            }
          }.uniq
        end

        # Stable topological order that preserves declaration order for
        # independent rules
        #
        # @api private
        def topo_sort(selected, all_rules)
          index = index_rules(all_rules)
          nodes = selected
          indegree = nodes.to_h { |rule| [rule, 0] }
          dependents = ::Hash.new { |hash, key| hash[key] = [] }

          nodes.each do |rule|
            rule.depends_on.each do |name|
              dependency = resolve(name, rule, index)
              next unless indegree.key?(dependency)

              indegree[rule] += 1
              dependents[dependency] << rule
            end
          end

          ordered = []
          ready = nodes.select { |rule| indegree[rule].zero? }

          until ready.empty?
            rule = ready.shift
            ordered << rule

            dependents[rule].each do |dependent|
              indegree[dependent] -= 1
              ready << dependent if indegree[dependent].zero?
            end
          end

          if ordered.size != nodes.size
            remaining = nodes - ordered
            raise CircularRuleDependencyError,
                  "#{contract_class.name} has circular rule dependencies: " \
                  "#{remaining.map(&:id).inspect}"
          end

          ordered
        end

        # Resolve a dependency name to exactly one rule
        #
        # @api private
        def resolve(name, dependent, index)
          candidates = index.fetch(name) { [] }

          if candidates.empty?
            raise UnknownRuleDependencyError,
                  "#{contract_class.name} rule #{dependent.id.inspect} depends on " \
                  "#{name.inspect}, but no rule with that name is defined"
          elsif candidates.length > 1
            raise UnknownRuleDependencyError,
                  "#{contract_class.name} rule #{dependent.id.inspect} depends on " \
                  "#{name.inspect}, which is ambiguous (#{candidates.size} rules " \
                  "share it); give the rules an explicit `name:`"
          end

          candidates.first
        end

        # Group rules by their resolvable identifier
        #
        # @api private
        def index_rules(rules)
          rules.each_with_object({}) do |rule, index|
            (index[rule.id] ||= []) << rule
          end
        end

        # Expand requested purposes with their declared includes
        #
        # @api private
        def expand_purposes(requested)
          expanded = []
          queue = requested.dup

          until queue.empty?
            purpose = queue.shift
            next if expanded.include?(purpose)

            expanded << purpose
            queue.concat(contract_class.purpose_includes.fetch(purpose, EMPTY_ARRAY))
          end

          expanded
        end

        # @api private
        def normalize(requested)
          Array(requested).flatten.map(&:to_sym).uniq
        end

        # @api private
        def registered_purposes
          contract_class.registered_purposes
        end

        # @api private
        def validate_requested_purposes!(requested)
          unknown = requested - registered_purposes

          return if unknown.empty?

          raise UnknownPurposeError,
                "#{contract_class.name} was called with unknown purposes: " \
                "#{unknown.inspect}. Known purposes: #{registered_purposes.inspect}"
        end

        # @api private
        def validate_rule_purposes!(rules)
          rules.each do |rule|
            unknown = rule.purposes - registered_purposes

            next if unknown.empty?

            raise UnknownPurposeError,
                  "#{contract_class.name} rule #{rule.id.inspect} uses unregistered " \
                  "purposes: #{unknown.inspect}. Known purposes: " \
                  "#{registered_purposes.inspect}"
          end
        end
      end

      # A single rule together with why it was selected or omitted
      #
      # @api public
      class RuleEntry
        extend Dry::Initializer

        # @!attribute [r] rule
        #   @return [Rule]
        #   @api public
        option :rule

        # @!attribute [r] reason
        #   @return [Symbol] `:base`, `:purpose` or `:dependency`
        #   @api public
        option :reason

        # @!attribute [r] purposes
        #   @return [Array<Symbol>] Purposes that matched (or rule's own purposes)
        #   @api public
        option :purposes

        # @!attribute [r] required_by
        #   @return [Array<Symbol>] Rule ids that pulled a dependency in
        #   @api public
        option :required_by

        # @api private
        def initialize(...)
          super
          freeze
        end

        # @return [Symbol]
        #
        # @api public
        def name
          rule.id
        end
      end

      # Immutable outcome of compiling a purpose combination
      #
      # @api public
      class RuleSelection
        extend Dry::Initializer
        include ::Enumerable

        # @!attribute [r] requested_purposes
        #   @return [Array<Symbol>, nil] Purposes passed to the call
        #   @api public
        option :requested_purposes

        # @!attribute [r] effective_purposes
        #   @return [Array<Symbol>] Requested purposes expanded through includes
        #   @api public
        option :effective_purposes

        # @!attribute [r] selected_entries
        #   @return [Array<RuleEntry>] Rules that were compiled in, in run order
        #   @api public
        option :selected_entries

        # @!attribute [r] omitted
        #   @return [Array<RuleEntry>] Rules skipped because of the purposes
        #   @api public
        option :omitted

        # @api private
        def initialize(...)
          super
          freeze
        end

        # @return [Array<Rule>] Rules selected for the combination, in run order
        #
        # @api public
        def rules
          selected_entries.map(&:rule)
        end

        # @return [Array<Rule>] Rules omitted due to the selected purposes
        #
        # @api public
        def omitted_rules
          omitted.map(&:rule)
        end

        # @return [RuleEntry, nil]
        #
        # @api public
        def entry_for(rule_or_name)
          name = rule_or_name.is_a?(Rule) ? rule_or_name.id : rule_or_name.to_sym
          selected_entries.find { |entry| entry.name == name }
        end

        # @return [Boolean]
        #
        # @api public
        def selected?(rule_or_name)
          !entry_for(rule_or_name).nil?
        end

        # Enumerable support over the selected entries
        #
        # @api public
        def each(&block)
          selected_entries.each(&block)
        end
      end
    end
  end
end
