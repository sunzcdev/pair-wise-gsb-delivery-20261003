require 'tsort'

module Dentaku
  class DependencyResolver
    include TSort

    def self.find_resolve_order(vars_to_dependencies_hash, case_sensitive = false)
      self.new(vars_to_dependencies_hash, case_sensitive).sort
    end

    # every independent cycle in the graph, as arrays of the original
    # variable names; groups and their members are sorted so the result is
    # stable regardless of declaration order
    def self.find_cycles(vars_to_dependencies_hash, case_sensitive = false)
      self.new(vars_to_dependencies_hash, case_sensitive).cycles
    end

    def initialize(vars_to_dependencies_hash, case_sensitive = false)
      @case_sensitive = case_sensitive
      @key_mapping = Hash[vars_to_dependencies_hash.keys.map { |k| [normalized_name(k), k] }]
      # normalize variables and their dependencies alike so child lookups match
      @vars_to_deps = Hash[vars_to_dependencies_hash.map { |k, v| [normalized_name(k), v.map { |d| normalized_name(d) }] }]
    end

    def sort
      tsort.map { |k| @key_mapping.fetch(k, k) }
    end

    def cycles
      strongly_connected_components.select { |scc|
        scc.size > 1 || @vars_to_deps.fetch(scc.first, []).include?(scc.first)
      }.map { |scc|
        scc.map { |k| @key_mapping.fetch(k, k) }.sort_by(&:to_s)
      }.sort_by { |group| group.map(&:to_s) }
    end

    def tsort_each_node(&block)
      @vars_to_deps.each_key(&block)
    end

    def tsort_each_child(node, &block)
      @vars_to_deps.fetch(node.to_s, []).each(&block)
    end

    private

    def normalized_name(variable_name)
      @case_sensitive ? variable_name.to_s : variable_name.to_s.downcase
    end
  end
end
