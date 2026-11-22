# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The match_hook_args plugin adds hooks that are called upon a successful match
    # by any of the matchers. It is similar to the match_hook plugin, but it allows
    # for passing the matchers and block arguments for each match method.
    #
    #   plugin :match_hook_args
    #
    #   add_match_hook do |matchers, block_args|
    #     logger.debug("matchers: #{matchers.inspect}. #{block_args.inspect} yielded.")
    #   end
    #
    #   # Term is an implicit matcher used for terminating matches, and
    #   # will be included in the array of matchers yielded to the match hook 
    #   # if a terminating match is used.
    #   term = self.class::RodaRequest::TERM
    #
    #   route do |r|
    #     r.root do
    #       # for a request for /
    #       # matchers: nil, block_args: nil
    #     end
    #
    #     r.on 'a', ['b', 'c'], Integer do |segment, id|
    #       # for a request for /a/b/1
    #       # matchers: ["a", ["b", "c"], Integer], block_args: ["b", 1]
    #     end
    #
    #     r.get 'd' do
    #       # for a request for /d
    #       # matchers: ["d", term], block_args: []
    #     end
    #   end
    #
    # == Route Tracing
    #
    # The match_hook_args plugin can optionally record a route trace, using
    # the +trace+ option:
    #
    #   plugin :match_hook_args, trace: true
    #
    #   add_match_hook do |matchers, block_args|
    #     entry = request.route_trace.last
    #     logger.debug("#{entry.marker.inspect} matched (tentative)")
    #   end
    #
    # A traced route records every successful match as a TraceEntry, using
    # the declared matchers as the stable marker, and never records the
    # captured values yielded to the match block.  Each entry starts in the
    # +:tried+ state.  When the match is part of the route that ultimately
    # returns the response, it is changed to +:committed+ when the response
    # is halted.  If the match block is exited without handling the request
    # (for example, via the pass or break plugins), the entry is changed to
    # +:abandoned+.
    #
    #   plugin :match_hook_args, trace: true
    #   plugin :pass
    #
    #   route do |r|
    #     r.on "a" do
    #       r.pass
    #     end
    #
    #     r.on "a" do
    #       "handled"
    #     end
    #   end
    #
    #   # for a request for /a, request.route_trace has two entries:
    #   #   marker: ["a"], status: :abandoned
    #   #   marker: ["a"], status: :committed, final
    #
    # A match that is entered but not committed to by the time the request
    # finishes (for example, because the match block raised an exception) is
    # reported as abandoned as well.  Committed entries that are not final are
    # outer matches that were exited because the final match returned the
    # response.  Exactly one entry in a completed trace is final: the innermost
    # match that returned the response.
    #
    # If an intermediate response is discarded and routing starts again (for
    # example, with the class_level_routing or status_handler plugins), the
    # entries from the discarded dispatch are changed to +:abandoned+ once the
    # replacement dispatch starts.  An interim result that has already been
    # written to the response is not discarded, so its match stays committed.
    #
    # The marker omits the capture values, so it can be logged without leaking
    # sensitive path segments:
    #
    #   route do |r|
    #     r.on "users", Integer do |id|
    #       # marker is ["users", Integer], not ["users", 123]
    #     end
    #   end
    #
    # Tracing never changes how often a matcher is executed.  Each matcher is
    # called exactly as often as it would be without tracing.
    #
    # +request.route_trace+ returns the array of TraceEntry instances for the
    # current request.  This is the live array, so entries recorded earlier
    # will have their status updated as routing continues.  If tracing is not
    # enabled, it returns a frozen empty array.  +request.route_trace_enabled?+
    # can be used to check whether tracing is enabled.
    module MatchHookArgs
      # A single successful match in the route trace.  +marker+ contains the
      # declared matchers (with any capture values omitted), and +status+ is
      # +:tried+, +:committed+, or +:abandoned+.
      class TraceEntry
        # The stable marker for the match.  This is an array of the matchers
        # used in the match, or nil for a match without matchers.
        attr_reader :marker

        # The current status of the entry, one of +:tried+, +:committed+,
        # or +:abandoned+.
        attr_reader :status

        # The position of the entry in the route trace.
        attr_reader :index

        alias matchers marker

        def initialize(marker, index)
          @marker = marker
          @index = index
          @status = :tried
          @final = false
        end

        # Whether the match has not yet been committed or abandoned.
        def tried?
          @status == :tried
        end

        # Whether the match is part of the route that returned the response.
        def committed?
          @status == :committed
        end

        # Whether the match was tried but did not handle the request.
        def abandoned?
          @status == :abandoned
        end

        # Whether this is the innermost match that returned the response.
        # Only one entry in a trace can be final.
        def final?
          @final
        end

        def commit! # :nodoc:
          @status = :committed
          self
        end

        def abandon! # :nodoc:
          @status = :abandoned if @status == :tried
          self
        end

        def final! # :nodoc:
          @status = :committed
          @final = true
          self
        end

        # Mark the entry as abandoned even though it had been committed.
        # Used when a later dispatch supersedes the response this entry
        # contributed to.
        def supersede! # :nodoc:
          @status = :abandoned
          @final = false
          self
        end

        def inspect
          "#<#{self.class.name} marker=#{@marker.inspect} status=#{@status}#{' final' if @final}>"
        end
      end

      EMPTY_TRACE = [].freeze

      def self.configure(app, opts=(no_positional_opts = true; OPTS), trace: opts[:trace], **kw)
        RodaPlugins.check_keywords("the match_hook_args plugin", no_positional_opts, opts, kw)
        app.opts[:match_hook_args] ||= []
        app.opts[:match_hook_args_trace] = trace unless trace.nil?
      end

      # Return a stable declaration marker for the given matchers.  The marker
      # only contains the declared matchers, not any values captured while
      # matching.  Mutable declaration objects are copied and frozen so later
      # mutation cannot change a marker already recorded in the trace.
      def self.trace_marker(matchers)
        return if matchers.nil?
        matchers.map{|matcher| trace_marker_value(matcher)}.freeze
      end

      def self.trace_marker_value(matcher)
        case matcher
        when String
          matcher.frozen? ? matcher : matcher.dup.freeze
        when Array
          matcher.map{|m| trace_marker_value(m)}.freeze
        when Hash
          h = {}
          matcher.each{|k, v| h[trace_marker_value(k)] = trace_marker_value(v)}
          h.freeze
        when Set
          Set.new(matcher.map{|m| trace_marker_value(m)}).freeze
        else
          matcher
        end
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :match_hook_args)
        RodaPlugins.opt_attr_reader(self, :match_hook_args_trace)

        # Freeze the array of hook methods when freezing the app
        def freeze
          match_hook_args.freeze
          super
        end

        # Add a match hook that will be called with matchers and block args.
        def add_match_hook(&block)
          match_hook_args << define_roda_method("match_hook_args", :any, &block)

          if match_hook_args.length == 1
            class_eval("alias _match_hook_args #{match_hook_args.first}", __FILE__, __LINE__)
          else
            class_eval("def _match_hook_args(v, a); #{match_hook_args.map{|m| "#{m}(v, a)"}.join(';')} end", __FILE__, __LINE__)
          end

          public :_match_hook_args

          nil
        end
      end

      module InstanceMethods
        # Default empty method if no match hooks are defined.
        def _match_hook_args(matchers, block_args)
        end
      end

      module RequestMethods
        # The route trace for the current request, an array of TraceEntry
        # instances.  This is the live array, so entries recorded earlier have
        # their status updated as routing continues.  If route tracing is not
        # enabled, returns a frozen empty array.
        def route_trace
          if route_trace_enabled?
            @route_trace ||= []
          else
            EMPTY_TRACE
          end
        end

        # Whether route tracing is enabled for the current request.
        def route_trace_enabled?
          !!roda_class.match_hook_args_trace
        end

        private

        # Add a trace entry for the match with the given matchers, returning
        # the entry.  The marker only contains the declared matchers.
        #
        # If an earlier dispatch attempt already committed a final entry, and
        # the response is still empty, then the response from that attempt was
        # discarded and routing has started again (for example, with the
        # class_level_routing or status_handler plugins).  In that case, mark
        # the entries from the discarded attempt abandoned and start a fresh
        # attempt.  If the response has already been written to, the earlier
        # match did return a result, so its entry remains committed and final.
        def _trace_match(matchers)
          trace = (@route_trace ||= [])

          if @route_trace_final && response.empty?
            trace.each{|e| e.supersede! if e.committed?}
            @route_trace_final = nil
          end

          trace << entry = TraceEntry.new(MatchHookArgs.trace_marker(matchers), trace.length)
          entry
        end

        # Mark the trace entry as committed.  The first entry committed for
        # the request is the innermost match that committed the response, and
        # is the only entry marked final.  Outer matches committed after it
        # (which exited because of that response) are committed but not final.
        # A match with a higher index than the final match is a separate
        # dispatch whose result did not become the response, so it is marked
        # abandoned.
        def _trace_commit(entry)
          return if entry.committed?

          if final = @route_trace_final
            if entry.index > final.index
              entry.abandon!
            else
              entry.commit!
            end
          else
            entry.final!
            @route_trace_final = entry
          end
        end

        # Call the match hook with matchers and block args if yielding to the block before yielding to the block.
        def if_match(v)
          unless route_trace_enabled?
            return super do |*a|
              scope._match_hook_args(v, a)
              yield(*a)
            end
          end

          entry = nil
          completed = false

          res = catch(:halt) do
            r = super do |*a|
              entry = _trace_match(v)
              scope._match_hook_args(v, a)
              yield(*a)
            end
            completed = true
            r
          end

          if completed
            # The match block returned without halting the request (such as
            # via the break plugin), so the match was not used for the response.
            entry.abandon! if entry
            res
          else
            _trace_commit(entry) if entry
            throw :halt, res
          end
        ensure
          entry.abandon! if entry && !entry.committed?
        end

        # Call the match hook with nil matchers and blocks before yielding to the block
        def always
          unless route_trace_enabled?
            scope._match_hook_args(nil, nil)
            return super
          end

          entry = _trace_match(nil)
          completed = false

          res = catch(:halt) do
            scope._match_hook_args(nil, nil)
            r = super
            completed = true
            r
          end

          if completed
            # The match block returned without halting the request (such as
            # via the break plugin), so the match was not used for the response.
            entry.abandon!
            res
          else
            _trace_commit(entry)
            throw :halt, res
          end
        ensure
          entry.abandon! if entry && !entry.committed?
        end
      end
    end

    register_plugin :match_hook_args, MatchHookArgs
  end
end

