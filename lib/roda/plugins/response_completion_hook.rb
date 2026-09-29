# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The response_completion_hook plugin allows you to register hooks
    # that are called when the response body has reached a terminal
    # state, either because it was fully sent to the client, or because
    # delivery was aborted.  This is useful for releasing resources
    # acquired during the request exactly once, after the response is
    # finished:
    #
    #   plugin :response_completion_hook
    #
    #   route do |r|
    #     resource = acquire_resource
    #
    #     response.on_completion do |state, error|
    #       resource.release
    #       logger.info("#{request.path}: #{state}") # :complete or :aborted
    #     end
    #
    #     # ...
    #   end
    #
    # The hook is called with the terminal state of the response, which
    # is +:complete+ if the response body was fully enumerated, and
    # +:aborted+ if it was not (either because enumerating the body
    # raised an exception, or because the server closed the response
    # body before it was fully enumerated).  If the body raised an
    # exception, the exception is passed as the second argument to the
    # hook, otherwise the second argument is nil.
    #
    # Each registered hook is called at most once per request.  The
    # hook is not called until the server has finished with the
    # response body; the plugin never eagerly consumes the body in
    # order to determine the terminal state.
    #
    # The plugin wraps the response body in a proxy when completion
    # hooks are registered.  The proxy preserves the behavior of the
    # original body: exceptions raised while enumerating the body are
    # reraised after the hooks run, and closing the proxy closes the
    # original body (if the original body responds to +close+).
    # Closing the proxy multiple times only closes the original body
    # once and does not call the hooks again.
    #
    # Note that hooks are only called for responses built through the
    # response object (i.e. via RodaResponse#finish or
    # RodaResponse#finish_with_body).  Responses returned by throwing
    # a rack response array directly with +halt+ do not trigger the
    # hooks.
    module ResponseCompletionHook
      # Proxy for the response body that calls the completion hooks
      # when the body reaches a terminal state.
      class Body
        # The wrapped response body.
        attr_reader :body

        # Wrap the given body, calling the given callbacks when the
        # body reaches a terminal state.
        def initialize(body, callbacks)
          @body = body
          @callbacks = callbacks
          @settled = false
        end

        # Yield each element of the body.  If the body is fully
        # enumerated, call the hooks with a :complete state.  If
        # enumerating the body raises an exception, call the hooks
        # with an :aborted state and reraise the exception.
        def each(&block)
          @body.each(&block)
          _settle(:complete, nil)
        rescue Exception => e
          _settle(:aborted, e)
          raise
        end

        # If the body has not reached a terminal state, call the hooks
        # with an :aborted state, since the server is closing the body
        # before it was fully enumerated.  Then close the wrapped body
        # if it responds to +close+.  Only closes the wrapped body
        # once, so that closing the proxy multiple times is safe.
        def close
          return if @closed
          @closed = true
          _settle(:aborted, nil)
          @body.close if @body.respond_to?(:close)
        end

        private

        # Call the callbacks with the terminal state, unless a
        # terminal state was already reached.
        def _settle(state, error)
          return if @settled
          @settled = true
          @callbacks.each{|callback| callback.call(state, error)}
        end
      end

      module ResponseMethods
        # Register a hook to be called when the response body reaches
        # a terminal state.  The hook is called with the terminal
        # state (:complete or :aborted) and the exception that caused
        # an abort (nil if the abort was not caused by an exception).
        def on_completion(&block)
          raise RodaError, "must pass a block to RodaResponse#on_completion" unless block
          (@completion_callbacks ||= []) << block
          nil
        end

        # Wrap the body in a ResponseCompletionHook::Body if
        # completion hooks have been registered.
        def finish
          _response_completion_hook_wrap(super)
        end

        # Wrap the given body in a ResponseCompletionHook::Body if
        # completion hooks have been registered.
        def finish_with_body(body)
          _response_completion_hook_wrap(super)
        end

        private

        # Replace the body in the rack response triple with a
        # ResponseCompletionHook::Body if completion hooks have been
        # registered.
        def _response_completion_hook_wrap(rack_response)
          if @completion_callbacks
            rack_response[2] = ResponseCompletionHook::Body.new(rack_response[2], @completion_callbacks)
          end
          rack_response
        end
      end
    end

    register_plugin(:response_completion_hook, ResponseCompletionHook)
  end
end
