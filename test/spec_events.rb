# frozen_string_literal: true

require_relative 'helper'

separate_testing do
  require_relative '../lib/rack/events'
end

module Rack
  class TestEvents < Minitest::Test
    class EventMiddleware
      attr_reader :events

      def initialize(events)
        @events = events
      end

      def on_start(req, res)
        events << [self, __method__]
      end

      def on_commit(req, res)
        events << [self, __method__]
      end

      def on_send(req, res)
        events << [self, __method__]
      end

      def on_hijack(req, res)
        events << [self, __method__]
      end

      def on_finish(req, res)
        events << [self, __method__]
      end

      def on_error(req, res, e)
        events << [self, __method__]
      end
    end

    class FailingHandler
      def initialize(error)
        @error = error
      end

      def on_start(req, res); end
      def on_commit(req, res); end
      def on_send(req, res); end
      def on_hijack(req, res); end
      def on_finish(req, res); end

      def on_error(req, res, e)
        raise @error
      end
    end

    def test_events_fire
      events = []
      ret = [200, {}, []]
      app = lambda { |env| events << [app, :call]; ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call({})
      response_body = []
      triple[2].each { |x| response_body << x }
      triple[2].close
      triple[2] = response_body
      assert_equal ret, triple
      assert_equal [[se, :on_start],
                    [app, :call],
                    [se, :on_commit],
                    [se, :on_send],
                    [se, :on_finish],
      ], events
    end

    def test_send_and_finish_are_not_run_until_body_is_sent
      events = []
      ret = [200, {}, []]
      app = lambda { |env| events << [app, :call]; ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      e.call({})
      assert_equal [[se, :on_start],
                    [app, :call],
                    [se, :on_commit],
      ], events
    end

    def test_send_is_called_on_each
      events = []
      ret = [200, {}, []]
      app = lambda { |env| events << [app, :call]; ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call({})
      triple[2].each { |x| }
      assert_equal [[se, :on_start],
                    [app, :call],
                    [se, :on_commit],
                    [se, :on_send],
      ], events
    end

    def test_send_is_called_on_call
      events = []
      ret = [200, {}, lambda { |stream| stream.close }]
      app = lambda { |env| events << [app, :call]; ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call({})
      triple[2].call(StringIO.new)
      assert_equal [[se, :on_start],
                    [app, :call],
                    [se, :on_commit],
                    [se, :on_send],
      ], events
    end

    def test_finish_is_called_on_close
      events = []
      ret = [200, {}, []]
      app = lambda { |env| events << [app, :call]; ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call({})
      triple[2].each { |x| }
      triple[2].close
      assert_equal [[se, :on_start],
                    [app, :call],
                    [se, :on_commit],
                    [se, :on_send],
                    [se, :on_finish],
      ], events
    end

    def test_finish_is_called_in_reverse_order
      events = []
      ret = [200, {}, []]
      app = lambda { |env| events << [app, :call]; ret }
      se1 = EventMiddleware.new events
      se2 = EventMiddleware.new events
      se3 = EventMiddleware.new events

      e = Events.new app, [se1, se2, se3]
      triple = e.call({})
      triple[2].each { |x| }
      triple[2].close

      groups = events.group_by { |x| x.last }
      assert_equal groups[:on_start].map(&:first), groups[:on_finish].map(&:first).reverse
      assert_equal groups[:on_commit].map(&:first), groups[:on_finish].map(&:first)
      assert_equal groups[:on_send].map(&:first), groups[:on_finish].map(&:first)
    end

    def test_finish_is_called_if_there_is_an_exception
      events = []
      app = lambda { |env| raise }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      assert_raises(RuntimeError) do
        e.call({})
      end
      assert_equal [[se, :on_start],
                    [se, :on_error],
                    [se, :on_finish],
      ], events
    end

    def test_evented_body_proxy_respond_to_each_matches_body
      app = lambda { |env| [200, {}, lambda { |stream| stream.close }] }
      e = Events.new app, []
      triple = e.call({})
      refute triple[2].respond_to?(:each)
    end

    def test_evented_body_proxy_respond_to_other_methods
      app = lambda { |env| [200, {}, []] }
      e = Events.new app, []
      triple = e.call({})
      assert triple[2].respond_to?(:close)
      refute triple[2].respond_to?(:nonexistent_method)
    end

    def test_full_hijack_lifecycle_when_called_after_app_returns
      events = []
      io = StringIO.new
      ret = nil
      app = lambda do |env|
        ret = [200, {}, []]
        env['rack.hijack'].call
        ret
      end
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call('rack.hijack' => proc { io })

      # The webserver still receives a triple wrapping the original body, but
      # on_send/on_finish must not be driven through the body once the
      # connection was hijacked.  The takeover happens inside the app, before
      # on_commit gets to run once the response triple is built.
      assert_equal [ret[0], ret[1]], [triple[0], triple[1]]
      assert_equal [[se, :on_start],
                    [se, :on_hijack],
                    [se, :on_finish],
                    [se, :on_commit],
      ], events

      # The body must not settle the request a second time.
      triple[2].close
      assert_equal [[se, :on_start],
                    [se, :on_hijack],
                    [se, :on_finish],
                    [se, :on_commit],
      ], events
    end

    def test_full_hijack_lifecycle_when_called_during_app
      events = []
      io = StringIO.new
      ret = [200, {}, []]
      app = lambda do |env|
        env['rack.hijack'].call
        ret
      end
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call('rack.hijack' => proc { io })

      assert_equal ret[0], triple[0]
      # No response existed yet when the connection was taken over.
      assert_equal [[se, :on_start],
                    [se, :on_hijack],
                    [se, :on_finish],
                    [se, :on_commit],
      ], events
    end

    def test_full_hijack_returns_the_servers_io_untouched
      given_io = StringIO.new
      returned_io = nil
      app = lambda do |env|
        returned_io = env['rack.hijack'].call
        [200, {}, []]
      end
      e = Events.new app, []
      e.call('rack.hijack' => proc { given_io })

      assert_same given_io, returned_io
      refute given_io.closed?
    end

    def test_full_hijack_does_not_close_the_hijacked_io
      io = StringIO.new
      app = lambda do |env|
        env['rack.hijack'].call
        [200, {}, []]
      end
      e = Events.new app, []
      triple = e.call('rack.hijack' => proc { io })
      triple[2].close

      refute io.closed?
    end

    def test_full_hijack_failure_is_settled_and_re_raised
      events = []
      takeover_error = RuntimeError.new('hijack failed')
      app = lambda do |env|
        env['rack.hijack'].call
        flunk 'app should not continue after a failed hijack'
      end
      se = EventMiddleware.new events
      e = Events.new app, [se]

      error = assert_raises(RuntimeError) do
        e.call('rack.hijack' => proc { raise takeover_error })
      end
      assert_same takeover_error, error
      # The takeover itself failed before any response triple existed, so the
      # request is settled entirely by the hijack wrapper's ensure block.
      assert_equal [[se, :on_start],
                    [se, :on_hijack],
                    [se, :on_error],
                    [se, :on_finish],
      ], events
    end

    def test_full_hijack_failure_finish_runs_once
      events = []
      app = lambda do |env|
        begin
          env['rack.hijack'].call
        rescue RuntimeError
        end
        raise 'app gives up after the failed takeover'
      end
      se = EventMiddleware.new events
      e = Events.new app, [se]

      assert_raises(RuntimeError) do
        e.call('rack.hijack' => proc { raise 'hijack failed' })
      end
      # The wrapper already settled on_error and on_finish for the failed
      # takeover.  The application's own failure then propagates out, but the
      # request was already settled, so nothing is reported a second time.
      assert_equal [[se, :on_start],
                    [se, :on_hijack],
                    [se, :on_error],
                    [se, :on_finish],
      ], events
      assert_equal 1, events.count { |_, name| name == :on_finish }
    end

    def test_partial_hijack_lifecycle
      events = []
      received_stream = nil
      hijack_callback = proc { |stream| received_stream = stream }
      ret = [200, { 'rack.hijack' => hijack_callback }, []]
      app = lambda { |env| ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call('rack.hijack?' => true)

      # Not settled before the webserver invokes the callback.
      assert_equal [[se, :on_start],
                    [se, :on_commit],
      ], events

      stream = StringIO.new
      triple[1]['rack.hijack'].call(stream)

      assert_same stream, received_stream
      assert_equal [[se, :on_start],
                    [se, :on_commit],
                    [se, :on_hijack],
                    [se, :on_finish],
      ], events
      refute stream.closed?
    end

    def test_partial_hijack_on_send_is_never_called
      events = []
      ret = [200, { 'rack.hijack' => proc { |stream| } }, []]
      app = lambda { |env| ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call('rack.hijack?' => true)
      triple[1]['rack.hijack'].call(StringIO.new)
      triple[2].close

      refute events.any? { |_, name| name == :on_send }
    end

    def test_partial_hijack_callback_exception_is_settled_and_re_raised
      events = []
      callback_error = RuntimeError.new('callback blew up')
      ret = [200, { 'rack.hijack' => proc { |stream| raise callback_error } }, []]
      app = lambda { |env| ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call('rack.hijack?' => true)

      error = assert_raises(RuntimeError) do
        triple[1]['rack.hijack'].call(StringIO.new)
      end
      assert_same callback_error, error
      assert_equal [[se, :on_start],
                    [se, :on_commit],
                    [se, :on_hijack],
                    [se, :on_error],
                    [se, :on_finish],
      ], events
    end

    def test_partial_hijack_finish_runs_once
      events = []
      ret = [200, { 'rack.hijack' => proc { |stream| } }, []]
      app = lambda { |env| ret }
      se = EventMiddleware.new events
      e = Events.new app, [se]
      triple = e.call('rack.hijack?' => true)
      triple[1]['rack.hijack'].call(StringIO.new)
      triple[2].close

      finishes = events.count { |_, name| name == :on_finish }
      assert_equal 1, finishes
    end

    def test_listener_error_does_not_mask_full_hijack_failure
      listener_error = RuntimeError.new('listener broke')
      takeover_error = RuntimeError.new('takeover broke')
      app = lambda do |env|
        env['rack.hijack'].call
        [200, {}, []]
      end
      e = Events.new app, [FailingHandler.new(listener_error)]

      error = assert_raises(RuntimeError) do
        e.call('rack.hijack' => proc { raise takeover_error })
      end
      assert_same takeover_error, error
    end

    def test_listener_error_does_not_mask_partial_hijack_callback_exception
      listener_error = RuntimeError.new('listener broke')
      callback_error = RuntimeError.new('callback broke')
      ret = [200, { 'rack.hijack' => proc { |stream| raise callback_error } }, []]
      app = lambda { |env| ret }
      e = Events.new app, [FailingHandler.new(listener_error)]
      triple = e.call('rack.hijack?' => true)

      error = assert_raises(RuntimeError) do
        triple[1]['rack.hijack'].call(StringIO.new)
      end
      assert_same callback_error, error
    end

    def test_listener_error_does_not_mask_application_exception
      listener_error = RuntimeError.new('listener broke')
      app_error = RuntimeError.new('app broke')
      app = lambda { |env| raise app_error }
      e = Events.new app, [FailingHandler.new(listener_error)]

      error = assert_raises(RuntimeError) do
        e.call({})
      end
      assert_same app_error, error
    end

    def test_request_and_response_are_frozen_after_commit
      observed = nil
      handler = Object.new
      handler.define_singleton_method(:on_start) { |req, res| }
      handler.define_singleton_method(:on_commit) { |req, res| }
      handler.define_singleton_method(:on_send) do |req, res|
        observed = [req.frozen?, res.frozen?]
      end
      handler.define_singleton_method(:on_hijack) { |req, res| }
      handler.define_singleton_method(:on_finish) { |req, res| }
      handler.define_singleton_method(:on_error) { |req, res, e| }

      app = lambda { |env| [200, {}, ['hi']] }
      e = Events.new app, [handler]
      triple = e.call({})
      triple[2].each { |x| }
      triple[2].close

      assert_equal [true, true], observed
    end

    def test_event_objects_are_frozen_before_returning_the_triple
      app = lambda { |env| [200, {}, ['hi']] }
      e = Events.new app, []
      triple = e.call({})

      # The committed event objects handed to handlers must be read-only,
      # while the Rack env and headers remain mutable per the Rack spec.
      assert triple[2].request.frozen?
      assert triple[2].response.frozen?
    end

    def test_objects_are_frozen_when_full_hijack_starts
      observed = nil
      handler = Object.new
      handler.define_singleton_method(:on_start) { |req, res| }
      handler.define_singleton_method(:on_commit) { |req, res| }
      handler.define_singleton_method(:on_send) { |req, res| }
      handler.define_singleton_method(:on_hijack) do |req, res|
        observed = [req.frozen?, res&.frozen?]
      end
      handler.define_singleton_method(:on_finish) { |req, res| }
      handler.define_singleton_method(:on_error) { |req, res, e| }

      app = lambda do |env|
        env['rack.hijack'].call
        [200, {}, []]
      end
      e = Events.new app, [handler]
      triple = e.call('rack.hijack' => proc { StringIO.new })

      # No response existed when the connection was taken over...
      assert_equal [true, nil], observed
      # ...but once the app returns its triple, the response is committed too.
      assert triple[2].response.frozen?
      assert triple[2].request.frozen?
    end

    def test_objects_are_frozen_when_partial_hijack_starts
      observed = nil
      handler = Object.new
      handler.define_singleton_method(:on_start) { |req, res| }
      handler.define_singleton_method(:on_commit) { |req, res| }
      handler.define_singleton_method(:on_send) { |req, res| }
      handler.define_singleton_method(:on_hijack) do |req, res|
        observed = [req.frozen?, res.frozen?]
      end
      handler.define_singleton_method(:on_finish) { |req, res| }
      handler.define_singleton_method(:on_error) { |req, res, e| }

      app = lambda { |env| [200, { 'rack.hijack' => proc { |stream| } }, []] }
      e = Events.new app, [handler]
      triple = e.call('rack.hijack?' => true)
      triple[1]['rack.hijack'].call(StringIO.new)

      assert_equal [true, true], observed
    end

    def test_normal_body_close_still_settles_once
      events = []
      app = lambda { |env| [200, {}, ['a', 'b']] }
      se1 = EventMiddleware.new events
      se2 = EventMiddleware.new events
      e = Events.new app, [se1, se2]
      triple = e.call({})
      triple[2].each { |x| }
      triple[2].close
      triple[2].close

      finishes = events.count { |_, name| name == :on_finish }
      assert_equal 2, finishes
    end

    def test_hijack_handlers_fire_in_reverse_order
      events = []
      se1 = EventMiddleware.new events
      se2 = EventMiddleware.new events
      se3 = EventMiddleware.new events

      ret = [200, { 'rack.hijack' => proc { |stream| } }, []]
      app = lambda { |env| ret }
      e = Events.new app, [se1, se2, se3]
      triple = e.call('rack.hijack?' => true)
      triple[1]['rack.hijack'].call(StringIO.new)

      groups = events.group_by { |x| x.last }
      assert_equal [se1, se2, se3], groups[:on_start].map(&:first)
      assert_equal [se3, se2, se1], groups[:on_hijack].map(&:first)
      assert_equal [se3, se2, se1], groups[:on_finish].map(&:first)
    end

    def test_hijack_state_is_isolated_between_requests
      events = []
      app = lambda do |env|
        if env['hijack']
          env['rack.hijack'].call
        end
        [200, {}, []]
      end
      se = EventMiddleware.new events
      e = Events.new app, [se]

      e.call('hijack' => true, 'rack.hijack' => proc { StringIO.new })
      triple = e.call('hijack' => false, 'rack.hijack' => proc { StringIO.new })
      triple[2].each { |x| }
      triple[2].close

      # The second request is a normal response and must still finish on its
      # own, proving the first request's finished flag did not leak.
      names = events.map(&:last)
      assert_equal [:on_start, :on_hijack, :on_finish, :on_commit,
                    :on_start, :on_commit, :on_send, :on_finish], names
    end
  end
end
