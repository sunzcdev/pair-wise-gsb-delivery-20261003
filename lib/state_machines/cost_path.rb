# frozen_string_literal: true

require_relative 'options_validator'

module StateMachines
  # A cost path represents the lowest-cost sequence of transitions that moves
  # an object from one state to a target state, based on the non-negative
  # costs declared for each event.
  #
  # The search is purely static: it walks the graph of states and branches
  # without firing events, running actions, or evaluating application-defined
  # guard code.  Guards are handled as follows:
  #
  # * State guards (+if_state+ / +unless_state+ and their all/any variants)
  #   only read the current state of other machines.  They are evaluated
  #   safely and used to prune impossible transitions.
  # * Application-defined conditions (+if+ / +unless+) cannot be verified
  #   without running arbitrary code, so transitions using them are kept as
  #   candidates and their guards are flagged as +:pending+ ("to be
  #   confirmed") in the result.
  #
  # Since event costs are non-negative, the search (a Dijkstra-style
  # relaxation over states) is guaranteed to terminate, even in the presence
  # of zero-cost cycles.  When multiple paths share the lowest cost, the one
  # with fewer transitions is chosen; remaining ties are broken by the
  # lexicographic order of the event name sequence, making the selection
  # stable for a given machine definition.
  #
  # == Examples
  #
  #   path = StateMachines::CostPath.new(vehicle, machine, to: :first_gear)
  #   path.status          # => :reachable (or :pending / :unreachable)
  #   path.cost            # => 2
  #   path.events          # => [:ignite, :shift_up]
  #   path.guards          # => [{event: :ignite, guard: :if, condition: :seatbelt_on?, status: :pending}]
  #   path.pending_guards  # => [{event: :ignite, guard: :if, condition: :seatbelt_on?, status: :pending}]
  class CostPath
    include Enumerable

    # The object whose state machine is being planned for
    attr_reader :object

    # The state machine being planned on
    attr_reader :machine

    # The initial state the search starts from
    attr_reader :from_name

    # The target state the search plans towards
    attr_reader :to_name

    # The total cost of the cheapest path found.  This is nil when the
    # target state is unreachable.
    attr_reader :cost

    # The guard conditions encountered along the cheapest path.  Each entry
    # is a hash of:
    # * <tt>:event</tt> - The event whose transition is guarded
    # * <tt>:guard</tt> - The guard type (:if, :unless, :if_state, ...)
    # * <tt>:condition</tt> - The configured condition
    # * <tt>:status</tt> - :verified for safely evaluated state guards,
    #   :pending for conditions that could not be evaluated
    attr_reader :guards

    # Creates a new lowest-cost path query for the given object.
    #
    # Configuration options:
    # * <tt>:from</tt> - The initial state to start from.  Default is the
    #   object's current state.
    # * <tt>:to</tt> - The target end state.  Required.
    def initialize(object, machine, options = {})
      options = { from: machine.states.match!(object).name }.merge(options)
      StateMachines::OptionsValidator.assert_valid_keys!(options, :from, :to)
      raise ArgumentError, 'Must specify :to state' unless options[:to]

      @object = object
      @machine = machine
      @from_name = machine.states.fetch(options[:from]).name
      @to_name = machine.states.fetch(options[:to]).name

      search
    end

    # Lists the transitions that make up the cheapest path.  Empty when the
    # target state is unreachable.
    def transitions
      @transitions ||= []
    end

    # Iterates over each transition in the cheapest path
    def each(&)
      transitions.each(&)
    end

    # Lists the events that would be fired along the cheapest path.
    #
    # For example,
    #
    #   path.events # => [:ignite, :shift_up]
    def events
      transitions.map(&:event)
    end

    # Lists the guard conditions along the cheapest path that could not be
    # evaluated safely and still need to be confirmed before the path can be
    # considered guaranteed.
    def pending_guards
      guards.select { |guard| guard[:status] == :pending }
    end

    # The outcome of the query:
    # * <tt>:reachable</tt> - A fully verified cheapest path was found
    # * <tt>:pending</tt> - The cheapest path depends on guard conditions
    #   that could not be evaluated and must be confirmed
    # * <tt>:unreachable</tt> - No sequence of transitions leads to the
    #   target state
    def status
      if unreachable?
        :unreachable
      elsif pending?
        :pending
      else
        :reachable
      end
    end

    # Whether a cheapest path was found and all of its guard conditions were
    # verified safely
    def reachable?
      !unreachable? && pending_guards.empty?
    end

    # Whether a cheapest path was found but it depends on guard conditions
    # that could not be evaluated
    def pending?
      !unreachable? && pending_guards.any?
    end

    # Whether no sequence of transitions leads to the target state
    def unreachable?
      @cost.nil?
    end

    private

    # Runs a Dijkstra-style search over the state graph.  Each state keeps
    # the best entry found so far; entries are only replaced by strictly
    # better ones, so zero-cost cycles cannot cause infinite searches.
    def search
      start = build_entry(from_name, 0, [], [])
      best = { from_name => start }
      queue = [start]

      until queue.empty?
        current = queue.min_by { |candidate| sort_key(candidate) }
        queue.delete(current)
        next unless best[current[:state]].equal?(current)

        edges_from(current[:state]).each do |edge|
          candidate = build_entry(
            edge[:to],
            current[:cost] + edge[:cost],
            current[:transitions] + [edge[:transition]],
            current[:guards] + edge[:guards]
          )
          known = best[edge[:to]]
          next if known && (sort_key(candidate) <=> sort_key(known)) >= 0

          best[edge[:to]] = candidate
          queue << candidate
        end
      end

      if (target = best[to_name])
        @cost = target[:cost]
        @transitions = target[:transitions]
        @guards = target[:guards]
      else
        @cost = nil
        @transitions = []
        @guards = []
      end
    end

    # Builds a search entry for a state reached with the given cost,
    # transitions, and guards
    def build_entry(state, cost, transitions, guards)
      { state: state, cost: cost, transitions: transitions, guards: guards }
    end

    # The stable ordering used to compare paths: lowest total cost first,
    # then fewest transitions, then lexicographic event name sequence
    def sort_key(entry)
      [entry[:cost], entry[:transitions].length, entry[:transitions].map { |transition| transition.event.to_s }]
    end

    # Expands the static edges (event branches) leading out of the given
    # state.  Branches whose state guards fail are pruned; branches guarded
    # by application code are kept with their guards flagged as pending.
    def edges_from(state)
      machine.events.flat_map do |event|
        event.branches.filter_map do |branch|
          match = branch.match(object, { from: state, guard: false })
          next unless match
          next unless branch.state_guards_match?(object)

          to = if match[:to].is_a?(LoopbackMatcher)
                 state
               else
                 match[:to].filter([state] | machine.states.map(&:name)).first
               end

          {
            to: to,
            cost: event.cost,
            transition: Transition.new(object, machine, event.name, state, to, false),
            guards: guard_entries(event, branch)
          }
        end
      end
    end

    # Describes the guard conditions configured for the given branch.  State
    # guards have already been verified during pruning; :if / :unless
    # conditions are application code and therefore pending confirmation.
    def guard_entries(event, branch)
      entries = branch.state_guards.map do |type, conditions|
        { event: event.name, guard: type, condition: conditions, status: :verified }
      end
      Array(branch.if_condition).each do |condition|
        entries << { event: event.name, guard: :if, condition: condition, status: :pending }
      end
      Array(branch.unless_condition).each do |condition|
        entries << { event: event.name, guard: :unless, condition: condition, status: :pending }
      end
      entries
    end
  end
end
