# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The response_complete plugin adds hooks that run when the response
    # body has reached a terminal state, which is after the rack response
    # array has been returned.  This is useful for releasing resources
    # acquired during the request, since routing may have finished long
    # before a streaming response body has been fully sent:
    #
    #   plugin :response_complete
    #
    #   response_complete do |result|
    #     release_resources(result)
    #   end
    #
    # The hook is called with the terminal state of the response body:
    #
    # :complete :: The body was fully enumerated by the server.
    # :abort :: The body raised an exception during enumeration, or the
    #           server closed the body before it was fully enumerated.
    #
    # The hook runs in the scope of the Roda instance that handled the
    # request, so it has access to the request, response, and any instance
    # variables set during routing.
    #
    # The hook is called at most once per response, even if the server
    # calls close on the body after it was fully enumerated, or calls
    # close multiple times.
    #
    # The response body is wrapped lazily, so the hook does not cause the
    # body to be eagerly consumed.  If the original body responds to
    # close, close is still called on it, and exceptions raised by the
    # original body's each or close methods are propagated to the caller.
    module ResponseComplete
      # Rack response body wrapper that notifies the response completion
      # hooks exactly once, when the wrapped body reaches a terminal state.
      class CompletionBody
        # The body to wrap and the Roda instance handling the request.
        def initialize(body, scope)
          @body = body
          @scope = scope
          @settled = false
        end

        # Yield each element of the wrapped body.  If the body is fully
        # enumerated, notify the hooks with :complete.  If an exception
        # is raised during enumeration, notify the hooks with :abort and
        # reraise the exception.
        def each(&block)
          completed = false
          begin
            @body.each(&block)
            completed = true
          ensure
            _roda_settle(completed)
          end
        end

        # Close the wrapped body if it responds to close, and notify the
        # hooks with :abort unless the body was already fully enumerated.
        # Exceptions raised by the wrapped body's close are propagated.
        def close
          begin
            @body.close if @body.respond_to?(:close)
          ensure
            _roda_settle(false)
          end
        end

        # Delegate methods not defined on the wrapper (such as to_path)
        # to the wrapped body.
        def method_missing(meth, *args, &block)
          if @body.respond_to?(meth)
            @body.__send__(meth, *args, &block)
          else
            super
          end
        end

        def respond_to_missing?(meth, include_all=false)
          @body.respond_to?(meth, include_all) || super
        end

        private

        # Notify the hooks of the terminal state, unless they have
        # already been notified.
        def _roda_settle(completed)
          return if @settled
          @settled = true
          @scope.send(:_roda_response_complete_hook, completed ? :complete : :abort)
        end
      end

      def self.configure(app)
        app.opts[:response_complete_hooks] ||= []
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :response_complete_hooks)

        # Freeze the array of hook methods when freezing the app.
        def freeze
          response_complete_hooks.freeze

          super
        end

        # Add a response completion hook, called with the terminal state
        # of the response body (:complete or :abort).
        def response_complete(&block)
          response_complete_hooks << define_roda_method("response_complete", 1, &block)
          if response_complete_hooks.length == 1
            class_eval("alias _roda_response_complete_hook #{response_complete_hooks.first}", __FILE__, __LINE__)
          else
            class_eval("def _roda_response_complete_hook(result) #{response_complete_hooks.map{|m| "#{m}(result)"}.join(';')} end", __FILE__, __LINE__)
          end
          private :_roda_response_complete_hook
          nil
        end
      end

      module InstanceMethods
        # Wrap the response body so the completion hooks are notified
        # when the body reaches a terminal state.
        def _roda_handle_main_route
          _roda_response_complete_wrap(super)
        end

        # Wrap the response body when using the error handler or
        # middleware, so the completion hooks are notified when the body
        # reaches a terminal state.
        def _roda_handle_route(&block)
          _roda_response_complete_wrap(super)
        end

        private

        # Wrap the body of the rack response in a CompletionBody if any
        # response completion hooks are defined.
        def _roda_response_complete_wrap(res)
          if res.is_a?(Array) && !self.class.response_complete_hooks.empty?
            res[2] = CompletionBody.new(res[2], self)
          end
          res
        end

        # Default method if no response completion hooks are defined.
        def _roda_response_complete_hook(result)
        end
      end
    end

    register_plugin(:response_complete, ResponseComplete)
  end
end
