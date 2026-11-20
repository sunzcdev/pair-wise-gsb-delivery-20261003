# frozen_string_literal: true

module ActiveRecord
  # This is a thread locals registry for EXPLAIN. For example
  #
  # ```
  # ActiveRecord::ExplainRegistry.queries
  # ```
  #
  # returns the collected queries local to the current thread.
  #
  # Captures are scoped: every +capture+ pushes a new one on a per-thread
  # stack, and releases it once its block returns or raises. Queries triggered
  # inside a nested capture are recorded there, never in the enclosing one, so
  # an +explain+ running while another capture is in progress cannot end up
  # with the other capture's statements. Each captured query carries the
  # +owner+ it was recorded for (the main query, a preload, or the label of an
  # explicit nested capture), and nested captures are attached to their parent
  # as children.
  class ExplainRegistry # :nodoc:
    # Default labels for the phases +explain+ attributes statements to.
    MAIN_QUERY = "main query"
    PRELOAD = "preload"

    # A single SQL statement recorded while a capture was active.
    class Query
      attr_reader :sql, :binds, :owner

      def initialize(sql, binds, owner = nil)
        @sql = sql
        @binds = binds
        @owner = owner
      end

      # Keeps the historical +[sql, binds]+ shape so callers that destructure
      # or index captured queries keep working.
      def to_ary
        [@sql, @binds]
      end
      alias_method :deconstruct, :to_ary

      def [](index)
        to_ary[index]
      end

      def inspect
        "#<#{self.class.name} owner=#{owner.inspect} sql=#{sql.inspect}>"
      end
    end

    # A single capture, holding the queries recorded for it and any nested
    # captures that were started while it was active.
    class Capture
      include Enumerable

      # An owner annotation for the statements running inside a block. A
      # sticky frame keeps applying to statements triggered by nested
      # relations (a preload runs association queries through ordinary
      # relations, which would otherwise re-attribute them to a main query).
      Frame = Struct.new(:owner, :sticky)

      attr_reader :owner, :parent, :queries, :children, :frames

      def initialize(owner = nil, parent = nil)
        @owner = owner
        @parent = parent
        @queries = []
        @children = []
        @frames = []
      end

      def push_owner(owner, sticky: false)
        frames << Frame.new(owner, sticky)
      end

      def pop_owner
        frames.pop
      end

      # The owner a statement recorded right now should be attributed to. A
      # sticky phase (preloading) wins so the relation loads a preload performs
      # are still reported as preloads rather than as their own main queries.
      # Otherwise the innermost phase (the main query) applies, falling back to
      # the capture's own label for statements not driven by a relation load.
      def effective_owner
        (frames.reverse_each.find(&:sticky) || frames.last)&.owner || @owner
      end

      def record(sql, binds, owner)
        query = Query.new(sql, binds, owner)
        @queries << query
        query
      end

      # The distinct owners that recorded at least one query, in the order
      # their first query was reported.
      def owners
        @queries.map(&:owner).uniq
      end

      # The queries recorded for +owner+ (a phase label, or the label of a
      # nested capture).
      def queries_for(owner)
        @queries.select { |query| query.owner == owner }
      end

      # Positional access to the recorded queries, kept so callers can keep
      # treating a capture like the array of +[sql, binds]+ pairs it replaced.
      def [](index)
        @queries[index]
      end

      def each(&block)
        @queries.each(&block)
      end

      def size
        @queries.size
      end

      def empty?
        @queries.empty?
      end

      def inspect
        "#<#{self.class.name} owner=#{owner.inspect} queries=#{@queries.size} children=#{@children.size}>"
      end
    end

    class Subscriber
      MUTEX = Mutex.new
      @subscribed = false

      class << self
        def ensure_subscribed
          return if @subscribed
          MUTEX.synchronize do
            return if @subscribed

            ActiveSupport::Notifications.subscribe("sql.active_record", new)
            @subscribed = true
          end
        end
      end

      def start(name, id, payload)
        # unused
      end

      def finish(name, id, payload)
        if ExplainRegistry.collect? && !ignore_payload?(payload)
          ExplainRegistry.record(*payload.values_at(:sql, :binds))
        end
      end

      def silenced?(_name)
        !ExplainRegistry.collect?
      end

      # SCHEMA queries cannot be EXPLAINed, also we do not want to run EXPLAIN on
      # our own EXPLAINs no matter how loopingly beautiful that would be.
      #
      # On the other hand, we want to monitor the performance of our real database
      # queries, not the performance of the access to the query cache.
      IGNORED_PAYLOADS = %w(SCHEMA EXPLAIN).freeze
      EXPLAINED_SQLS = /\A\s*(\/\*.*\*\/)?\s*(with|select|update|delete|insert)\b/i
      def ignore_payload?(payload)
        payload[:exception] ||
          payload[:cached] ||
          IGNORED_PAYLOADS.include?(payload[:name]) ||
          !payload[:sql].match?(EXPLAINED_SQLS)
      end
    end

    class << self
      delegate :start, :reset, :collect, :collect=, :collect?, :queries,
               :capture, :record, :current_capture, :with_owner,
               to: :instance

      private
        def instance
          ActiveSupport::IsolatedExecutionState[:active_record_explain_registry] ||= new
        end
    end

    attr_accessor :collect
    attr_reader :queries

    def initialize
      reset
    end

    def start
      Subscriber.ensure_subscribed
      @collect = true
    end

    # Runs the block with a fresh capture active and returns it. The capture
    # is released once the block finishes, whether it returns or raises, so a
    # failed statement cannot leave its partial capture behind for the next
    # caller. Nested captures are linked to the capture that was active when
    # they started and are not visible through the enclosing capture's
    # +queries+.
    def capture(owner = nil, &block)
      Subscriber.ensure_subscribed

      capture = Capture.new(owner, current_capture)
      current_capture.children << capture if current_capture
      capture_stack << capture

      begin
        block.call(capture)
        capture
      ensure
        capture_stack.pop
      end
    end

    # Records a SQL statement for whichever capture is active. Statements
    # triggered while no capture is active keep landing in the flat +queries+
    # collection as plain +[sql, binds]+ pairs, preserving the previous
    # behaviour of +#start+ without a block.
    def record(sql, binds, owner = nil)
      if capture = current_capture
        capture.record(sql, binds, owner || capture.effective_owner)
      else
        @queries << [sql, binds]
      end
    end

    # Attributes the statements triggered by the block to +owner+ within the
    # active capture. Statements that run inside nested relation loads (as a
    # preload does) keep that attribution, which is how a preload's own
    # queries are told apart from the relation's main query. Outside of a
    # capture this is a plain pass-through.
    def with_owner(owner, sticky: false)
      capture = current_capture
      return yield unless capture

      capture.push_owner(owner, sticky: sticky)
      begin
        yield
      ensure
        capture.pop_owner
      end
    end

    def current_capture
      @captures&.last
    end

    def capture_stack
      @captures ||= []
    end

    def collect?
      return true if current_capture
      @collect
    end

    def reset
      @collect = false
      @captures = []
      @queries = []
    end
  end
end
