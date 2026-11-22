# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The match_hook plugin adds hooks that are called upon a successful match
    # by any of the matchers.  The hooks do not take any arguments.  If you would
    # like hooks that pass the arguments/matchers and values yielded to the route block,
    # use the match_hook_args plugin. This uses the match_hook_args plugin internally,
    # but doesn't pass the matchers and values yielded.
    #
    #   plugin :match_hook
    #
    #   match_hook do
    #     logger.debug("#{request.matched_path} matched. #{request.remaining_path} remaining.")
    #   end
    #
    # == Route Tracing
    #
    # Pass the +trace+ option to enable route tracing, which records each
    # successful match in +request.route_trace+.  This is useful for seeing
    # which part of the routing tree handled a request, and for distinguishing
    # matches that were tried but abandoned from the match that actually
    # returned the response:
    #
    #   plugin :match_hook, trace: true
    #
    #   match_hook do
    #     entry = request.route_trace.last
    #     logger.debug("#{entry.marker.inspect} matched")
    #   end
    #
    # See the match_hook_args plugin documentation for details on the trace
    # entries and their statuses.
    module MatchHook
      def self.load_dependencies(app, opts=OPTS)
        app.plugin :match_hook_args, **opts
      end

      module ClassMethods
        # Add a match hook.
        def match_hook(&block)
          meth = define_roda_method("match_hook", 0, &block)
          add_match_hook{|_,_| send(meth)}
          nil
        end
      end
    end

    register_plugin :match_hook, MatchHook
  end
end
