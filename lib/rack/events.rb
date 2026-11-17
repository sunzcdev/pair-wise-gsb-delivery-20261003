# frozen_string_literal: true

require_relative 'body_proxy'
require_relative 'request'
require_relative 'response'

module Rack
  ### This middleware provides hooks to certain places in the request /
  # response lifecycle.  This is so that middleware that don't need to filter
  # the response data can safely leave it alone and not have to send messages
  # down the traditional "rack stack".
  #
  # The events are:
  #
  # * on_start(request, response)
  #
  #   This event is sent at the start of the request, before the next
  #   middleware in the chain is called.  This method is called with a request
  #   object, and a response object.  Right now, the response object is always
  #   nil, but in the future it may actually be a real response object.
  #
  # * on_commit(request, response)
  #
  #   The response has been committed.  The application has returned, but the
  #   response has not been sent to the webserver yet.  This method is always
  #   called with a request object and the response object.  The response
  #   object is constructed from the rack triple that the application returned.
  #   Changes may still be made to the response object at this point.
  #
  # * on_send(request, response)
  #
  #   The webserver has started iterating over the response body, or has called
  #   the streaming body, and presumably has started sending data over the
  #   wire. This method is always called with a request object and the response
  #   object. The response object is constructed from the rack triple that the
  #   application returned.  Changes SHOULD NOT be made to the response object
  #   as the webserver has already started sending data.  Any mutations will
  #   likely result in an exception.
  #
  # * on_hijack(request, response)
  #
  #   The application has hijacked the connection.  This happens either when
  #   the application calls the +rack.hijack+ env callable (full hijack, which
  #   hands over the raw connection), or when the webserver invokes the
  #   +rack.hijack+ response header callback (partial hijack, which hands over
  #   only the response body stream).  It is sent after +on_commit+ (or before
  #   +on_commit+ if the application performs a full hijack before returning
  #   its response triple, in which case the response argument is +nil+) and in
  #   place of +on_send+: once a hijack has taken place, the response is no
  #   longer driven through the normal response body, so +on_send+ is not
  #   called.  The request and response objects are read-only at this point,
  #   and the hijacked stream is owned by the application and the webserver;
  #   the event middleware must not close it.
  #
  # * on_finish(request, response)
  #
  #   The webserver has closed the response, and all data has been written to
  #   the response socket.  The request and response object should both be
  #   read-only at this point.  The body MAY NOT be available on the response
  #   object as it may have been flushed to the socket.
  #
  #   For a hijacked response, this is sent once the hijack has been fully
  #   settled: after the +rack.hijack+ env callable returns (or fails) for a
  #   full hijack, or after the partial hijack callback returns (or raises) and
  #   the webserver has closed the hijacked stream.  It is guaranteed to run at
  #   most once per request, no matter how the response is settled.
  #
  # * on_error(request, response, error)
  #
  #   An exception has occurred in the application, in an event handler, while
  #   taking over a hijacked connection, or inside a hijack callback.
  #   This method will get the request, the response (if available) and the
  #   exception that was raised.
  #
  # ## Order
  #
  # `on_start` is called on the handlers in the order that they were passed to
  # the constructor.  `on_commit`, `on_send`, `on_hijack`, `on_finish`, and
  # `on_error` are called in the reverse order.  `on_finish` handlers are
  # called inside an `ensure` block, so they are guaranteed to be called even if
  # something raises an exception.  If something raises an exception in a
  # `on_finish` method, then nothing is guaranteed.
  #
  # Exceptions raised by event handlers never replace the exception that was
  # being handled: if the application, a hijack callable, or a hijack callback
  # raises, that original exception is always re-raised after the handlers have
  # been notified.

  class Events
    module Abstract
      def on_start(req, res)
      end

      def on_commit(req, res)
      end

      def on_send(req, res)
      end

      def on_hijack(req, res)
      end

      def on_finish(req, res)
      end

      def on_error(req, res, e)
      end
    end

    # Per-request bookkeeping.  A single Rack::Events middleware instance
    # serves many requests, so all request-scoped state lives here and is
    # captured by the wrapped hijack callables and the body proxy.
    class Invocation # :nodoc:
      attr_reader :request, :response, :handlers

      def initialize(handlers, request)
        @handlers = handlers
        @request  = request
        @response = nil
        @finished = false
      end

      def response=(response)
        @response = response
      end

      # Freeze the event objects once the response triple has been committed
      # to the webserver, so handlers can no longer reassign their state.
      # The underlying env and header hashes themselves stay unfrozen: the
      # Rack specification requires them to remain mutable for later
      # middleware and the webserver.  A full hijack can take place before the
      # application returns a response, in which case the response is frozen
      # later when it is built.
      def commit
        request.freeze
        response.freeze if response && !response.frozen?
      end

      def on_commit
        handlers.reverse_each { |handler| handler.on_commit request, response }
      end

      def on_send
        handlers.reverse_each { |handler| handler.on_send request, response }
      end

      # The application has taken over the connection (full or partial hijack).
      # on_send never fires for a hijacked response, which is driven outside
      # the normal response body.
      def on_hijack
        commit
        handlers.reverse_each { |handler| handler.on_hijack request, response }
      end

      def notify_error(error)
        handlers.reverse_each do |handler|
          begin
            handler.on_error request, response, error
          rescue StandardError
            # A failing listener must not mask the error being reported.
          end
        end
      end

      # Run the on_finish handlers at most once per request, regardless of how
      # the response is settled (hijack takeover, hijack failure, hijacked
      # stream close, or body.close).
      def finish
        return if @finished
        @finished = true
        handlers.reverse_each { |handler| handler.on_finish request, response }
      end

      def finished?
        @finished
      end
    end

    class EventedBodyProxy < Rack::BodyProxy # :nodoc:
      attr_reader :request, :response

      def initialize(body, invocation, &block)
        super(body, &block)
        @invocation = invocation
        @request    = invocation.request
        @response   = invocation.response
      end

      def each
        @invocation.on_send
        super
      end

      def call(stream)
        @invocation.on_send
        super
      end

      def respond_to?(method_name, include_all = false)
        case method_name
        when :each, :call
          @body.respond_to?(method_name, include_all)
        else
          super
        end
      end
    end

    class BufferedResponse < Rack::Response::Raw # :nodoc:
      attr_reader :body

      def initialize(status, headers, body)
        super(status, headers)
        @body = body
      end

      def to_a; [status, headers, body]; end
    end

    def initialize(app, handlers)
      @app      = app
      @handlers = handlers
    end

    def call(env)
      request    = make_request env
      invocation = Invocation.new(@handlers, request)
      on_start request, nil

      # A full hijack gives the application the raw connection as soon as it
      # calls env['rack.hijack'], and the application may do so at any point,
      # including while it is still building the response.  Install the
      # wrapper before calling the application so the takeover is observed
      # whenever it happens.
      if request.env['rack.hijack']
        wrap_full_hijack(invocation)
      end

      begin
        status, headers, body = @app.call request.env
        response = make_response status, headers, body
        invocation.response = response
        # If a full hijack already committed the request while the app was
        # running, this only freezes the freshly built response.
        invocation.commit
        invocation.on_commit
      rescue StandardError => e
        # The application (or an on_commit handler) failed before a response
        # triple could be handed to the webserver, so settle the request here.
        # If a full hijack already took over the connection and the request
        # was settled when its callable returned, the handlers have already
        # been notified and finished: do not report or finish a second time.
        unless invocation.finished?
          invocation.notify_error e
          invocation.finish
        end
        raise
      end

      # A partial hijack is driven by the webserver through the
      # 'rack.hijack' response header callback, after the status and headers
      # have been sent.  The webserver ignores the body in that case.
      if request.env['rack.hijack?'] && response.headers.key?('rack.hijack')
        wrap_partial_hijack(invocation)
      end

      # The response triple is now handed over to the webserver.  For a
      # hijacked response, commit happens when the takeover actually starts
      # (see Invocation#on_hijack).
      invocation.commit unless request.frozen?

      body = EventedBodyProxy.new(body, invocation) do
        invocation.finish
      end
      [response.status, response.headers, body]
    end

    private

    # Wrap env['rack.hijack'] so that the takeover can be observed and the
    # request is settled (exactly once) when the callable returns or fails.
    def wrap_full_hijack(invocation)
      request         = invocation.request
      original_hijack = request.env['rack.hijack']

      request.env['rack.hijack'] = proc do
        # The application is taking over the raw connection.  From here on
        # the connection belongs to the application and the webserver, so the
        # returned IO must be handed back untouched and never closed here.
        invocation.on_hijack

        begin
          io = original_hijack.call
        rescue StandardError => e
          # The webserver could not hand over the connection.
          invocation.notify_error e
          raise
        ensure
          invocation.finish
        end

        io
      end
    end

    # Wrap the 'rack.hijack' response header callback so that the takeover can
    # be observed and the request is settled when the callback returns, raises,
    # or the webserver closes the hijacked stream.
    def wrap_partial_hijack(invocation)
      response        = invocation.response
      original_hijack = response.headers['rack.hijack']

      response.headers['rack.hijack'] = proc do |stream|
        # The webserver has sent the status and headers and is handing over
        # the response body stream.  The stream remains owned by the
        # application and the webserver, so it must not be closed here.
        invocation.on_hijack

        begin
          original_hijack.call(stream)
        rescue StandardError => e
          # The hijack callback failed while handling the stream.
          invocation.notify_error e
          raise
        ensure
          # Settle when the callback returns or raises.  If the webserver
          # closes the stream while the callback is still running, that is the
          # application's stream to manage; it is not closed again here.
          invocation.finish
        end
      end
    end

    def on_start(request, response)
      @handlers.each { |handler| handler.on_start request, nil }
    end

    def make_request(env)
      Rack::Request.new env
    end

    def make_response(status, headers, body)
      BufferedResponse.new status, headers, body
    end
  end
end
