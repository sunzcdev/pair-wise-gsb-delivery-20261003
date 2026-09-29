# frozen_string_literal: true

require_relative 'options_validator'

module StateMachines
  # A cost path represents the lowest-cost sequence of transitions that moves
  # an object from its current state to a target state.  The cost of a path
  # is the sum of the costs declared by each event (see
  # StateMachines::Event#cost).
  #
  # The search is purely static: no events are fired and no actions are run
  # on the object in order to explore the graph.  Guard conditions are
  # handled based on whether they can be safely evaluated without running
  # object code:
  # * State requirements, state guards (e.g. <tt>:if_state</tt>), and literal
  #   true/false conditionals are evaluated immediately and used to prune
  #   impossible branches
  # * Symbol/proc/string <tt>:if</tt> / <tt>:unless</tt> conditionals require
  #   running object code and therefore cannot be safely determined.  Paths
  #   that rely on them are still found, but the path is marked as
  #   <tt>:pending</tt> and the conditions are exposed via
  #   <tt>pending_guards</tt> so the caller can confirm them
  #
  # Since every event declares a non-negative cost, the search is guaranteed
  # to terminate even when the graph contains zero-cost cycles.  When
  # multiple paths share the same lowest cost, the winner is chosen
  # deterministically: fewest transitions first, then the lexicographically
  # smallest sequence of event names.
  #
  # == Example
  #
  #   path = machine.lowest_cost_path_for(vehicle, to: :first_gear)
  #   path.status          # => :reachable
  #   path.cost            # => 2
  #   path.events          # => [:ignite, :shift_up]
  #   path.guards          # => [#<struct StateMachines::CostPath::Guard ...>]
  #   path.pending_guards  # => []
  class CostPath < Array
    # A guard condition encountered along a cost path.  The +verdict+ is
    # <tt>:satisfied</tt> when the condition could be safely evaluated and
    # passed, or <tt>:unknown</tt> when it requires running object code and
    # still needs to be confirmed.
    Guard = Struct.new(:event, :type, :condition, :verdict) do
      # Whether this condition could not be safely evaluated and must be
      # confirmed before the path can be relied on
      def pending?
        verdict == :unknown
      end
    end

    # A single step explored by the search
    Candidate = Struct.new(:event, :to, :cost, :guards, :pending)

    # A node in the search graph
    Node = Struct.new(:state, :pending, :cost, :steps, :names, :event, :from, :guards, :parent)

    # The object whose state machine is being searched
    attr_reader :object

    # The state machine being searched
    attr_reader :machine

    # The initial state the search starts from
    attr_reader :from_name

    # The target state the search is trying to reach
    attr_reader :to_name

    # All of the guard conditions encountered along the chosen path
    attr_reader :guards

    # The outcome of the search:
    # * <tt>:reachable</tt> - a lowest-cost path was found and every guard
    #   condition along it could be safely verified
    # * <tt>:pending</tt> - a lowest-cost path was found, but it relies on
    #   guard conditions that could not be safely evaluated and must be
    #   confirmed
    # * <tt>:unreachable</tt> - the target state cannot be reached, even
    #   through transitions with unverifiable guard conditions
    attr_reader :status

    # The total cost of the chosen path.  This is nil when the target state
    # is unreachable.
    attr_reader :cost

    # Creates a new cost path search for the given object.
    #
    # Configuration options:
    # * <tt>:from</tt> - The initial state to start from.  Default is the
    #   object's current state.
    # * <tt>:to</tt> - The target state to reach.  This option is required.
    def initialize(object, machine, options = {})
      options = { from: machine.states.match!(object).name }.merge(options)
      StateMachines::OptionsValidator.assert_valid_keys!(options, :from, :to)
      raise ArgumentError, 'Must specify a :to target state' unless options[:to]

      @object = object
      @machine = machine
      @from_name = machine.states.fetch(options[:from]).name
      @to_name = machine.states.fetch(options[:to]).name
      @guards = []
      @cost = nil
      @status = :unreachable

      search
    end

    # Whether a path was found whose guard conditions are all verified
    def reachable?
      status == :reachable
    end

    # Whether a path was found that relies on unverified guard conditions
    def pending?
      status == :pending
    end

    # Whether the target state cannot be reached at all
    def unreachable?
      status == :unreachable
    end

    # Lists all of the events fired along the chosen path.
    #
    # For example,
    #
    #   path.events # => [:ignite, :shift_up]
    def events
      map(&:event)
    end

    # Lists the guard conditions along the chosen path that could not be
    # safely evaluated and must be confirmed before the path is relied on
    def pending_guards
      guards.select(&:pending?)
    end

    private

    # The sort key used to order search nodes.  Nodes are ordered by total
    # cost, then by the number of transitions, then by the sequence of event
    # names.  This makes the search deterministic and ensures that equal-cost
    # paths are always resolved the same way.
    def key_of(node)
      [node.cost, node.steps, node.names]
    end

    # Runs the lowest-cost search from the initial state to the target state
    def search
      nodes = {}
      best = {}
      root = Node.new(from_name, false, 0, 0, [], nil, nil, [], nil)
      best[[from_name, false]] = key_of(root)
      nodes[[from_name, false]] = root
      queue = [root]

      until queue.empty?
        index = queue.each_index.min_by { |i| key_of(queue[i]) }
        node = queue.delete_at(index)
        label = [node.state, node.pending]

        # Skip nodes that have been superseded by a better (or verified)
        # path to the same state
        next unless best[label] == key_of(node)
        next if node.pending && dominated_by_verified?(best, node)

        each_candidate(node.state) do |candidate|
          child = Node.new(
            candidate.to,
            node.pending || candidate.pending,
            node.cost + candidate.cost,
            node.steps + 1,
            node.names + [candidate.event],
            candidate.event,
            node.state,
            candidate.guards,
            node
          )
          child_label = [child.state, child.pending]
          child_key = key_of(child)

          next if child.pending && dominated_by_verified?(best, child)

          current = best[child_label]
          next if current && (current <=> child_key) <= 0

          best[child_label] = child_key
          nodes[child_label] = child
          queue << child
        end
      end

      finalize(nodes)
    end

    # Determines whether a verified (non-pending) path to the node's state
    # exists that is at least as good as the given pending node, making the
    # pending node irrelevant
    def dominated_by_verified?(best, node)
      (verified_key = best[[node.state, false]]) && (verified_key <=> key_of(node)) <= 0
    end

    # Selects the winning node for the target state and builds the resulting
    # transitions / guards.  A verified path is preferred when its cost is
    # tied with an unverified one.
    def finalize(nodes)
      verified = nodes[[to_name, false]]
      unverified = nodes[[to_name, true]]

      chosen =
        if verified && (!unverified || (key_of(verified) <=> key_of(unverified)) <= 0)
          @status = :reachable
          verified
        elsif unverified
          @status = :pending
          unverified
        end

      return unless chosen

      @cost = chosen.cost

      chain = []
      node = chosen
      while node
        chain.unshift(node)
        node = node.parent
      end
      chain.shift # Drop the root node, which has no transition

      chain.each do |step|
        self << Transition.new(object, machine, step.event, step.from, step.state, false)
        @guards.concat(step.guards)
      end
    end

    # Yields each transition that can statically be explored from the given
    # state.  Branches whose safely-evaluated guards fail are pruned.
    def each_candidate(state_name)
      machine.events.each do |event|
        event.branches.each do |branch|
          branch.state_requirements.each do |requirement|
            next unless requirement[:from].matches?(state_name, from: state_name)

            verdict, guards = classify_guards(branch, event.name, state_name)
            next if verdict == :violated

            candidate_to_states(requirement[:to], state_name).each do |to_state|
              yield Candidate.new(event.name, to_state, event.cost, guards, verdict == :unknown)
            end
          end
        end
      end
    end

    # Resolves the concrete states a branch can transition to from the given
    # state
    def candidate_to_states(to_matcher, state_name)
      if to_matcher.is_a?(LoopbackMatcher)
        [state_name]
      else
        to_matcher.filter(machine.states.map(&:name))
      end
    end

    # Classifies the guard conditions of a branch without running any object
    # code.  Returns the verdict (:satisfied, :unknown, or :violated) along
    # with the conditions that were encountered.
    def classify_guards(branch, event_name, state_name)
      guards = []
      pending = false

      Array(branch.if_condition).each do |condition|
        case condition
        when true
          guards << Guard.new(event_name, :if, condition, :satisfied)
        when false, nil
          guards << Guard.new(event_name, :if, condition, :violated)
          return [:violated, guards]
        else
          guards << Guard.new(event_name, :if, condition, :unknown)
          pending = true
        end
      end

      Array(branch.unless_condition).each do |condition|
        case condition
        when false, nil
          guards << Guard.new(event_name, :unless, condition, :satisfied)
        when true
          guards << Guard.new(event_name, :unless, condition, :violated)
          return [:violated, guards]
        else
          guards << Guard.new(event_name, :unless, condition, :unknown)
          pending = true
        end
      end

      branch.state_guards.each do |type, conditions|
        if state_guard_satisfied?(type, conditions, state_name)
          guards << Guard.new(event_name, type, conditions, :satisfied)
        else
          guards << Guard.new(event_name, type, conditions, :violated)
          return [:violated, guards]
        end
      end

      [pending ? :unknown : :satisfied, guards]
    end

    # Evaluates a state guard (e.g. <tt>:if_state</tt>) against the
    # hypothetical state being explored for this machine and the actual
    # states of any other machines on the object
    def state_guard_satisfied?(type, conditions, state_name)
      checks = conditions.map { |machine_name, state| state_matches?(machine_name, state, state_name) }

      case type
      when :if_state, :if_all_states
        checks.all?
      when :unless_state, :unless_any_state
        checks.none?
      when :if_any_state
        checks.any?
      when :unless_all_states
        !checks.all?
      end
    end

    # Determines whether the given machine is in the expected state.  For the
    # machine being searched, the hypothetical state being explored is used
    # instead of the object's actual state.
    def state_matches?(machine_name, expected_state, state_name)
      other = machine_name == machine.name ? machine : object.class.state_machines[machine_name]
      raise ArgumentError, "State machine '#{machine_name}' is not defined for #{object.class.name}" unless other

      state = other.states[expected_state]
      raise ArgumentError, "State '#{expected_state}' is not defined in state machine '#{machine_name}'" unless state

      current_value = other.equal?(machine) ? machine.states.fetch(state_name).value : other.read(object, :state)
      state.matches?(current_value)
    end
  end
end
