# frozen_string_literal: true

require 'forwardable'
require 'monitor'

module Faraday
  # Response represents an HTTP response from making an HTTP request.
  #
  # A response is *unfinished* until an adapter hands it the {Faraday::Env} it
  # received from the server. Adapters that complete requests asynchronously
  # return an unfinished response and call {#finish} once the response arrives,
  # which makes {#on_complete} run the registered callbacks in order.
  class Response
    extend Forwardable
    extend MiddlewareRegistry

    # Returned by {#next_callback} when the completion queue is exhausted.
    QUEUE_EXHAUSTED = Object.new
    private_constant :QUEUE_EXHAUSTED

    def initialize(env = nil)
      @env = Env.from(env) if env
      reset_completion_state(finished: finished?)
    end

    attr_reader :env

    def status
      finished? ? env.status : nil
    end

    def reason_phrase
      finished? ? env.reason_phrase : nil
    end

    def headers
      finished? ? env.response_headers : {}
    end

    def_delegator :headers, :[]

    # Statusless middleware may write response bodies through `Env#body`; keep
    # that representation working while preferring an explicit response body.
    def body
      return unless finished?
      return env.response_body unless env.response_body.nil?

      env.body
    end

    def url
      finished? ? env.url : nil
    end

    def finished?
      !!env
    end

    # Registers a block to run once the response is complete.
    #
    # Adapters that finish requests asynchronously return an unfinished
    # response, so middleware can register callbacks before the response
    # arrives. Callbacks run in registration order, also when a callback
    # registers another one from within {#finish}: the new callback is queued
    # behind the callbacks that were already registered, never in front of
    # them.
    #
    # On a response that is already finished the block runs immediately, as it
    # always has. A block registered while {#finish} is still draining the
    # queue also runs before {#finish} returns.
    #
    # @yieldparam env [Faraday::Env] the finished response environment
    # @return [Faraday::Response] self, so calls can be chained
    def on_complete(&block)
      immediate_env = nil
      @completion_monitor.synchronize do
        if @completion_state == :finished
          immediate_env = @env
        else
          @on_complete_callbacks << block
        end
      end
      yield(immediate_env) if immediate_env
      self
    end

    # Marks the response as complete with the given environment.
    #
    # All callbacks registered through {#on_complete} run once, in registration
    # order, with the response environment. Notifying completion more than once
    # (as asynchronous adapters may do) does not run any callback a second
    # time: the second and later calls return self. A response that was created
    # with an environment is already finished and still raises, as it never had
    # a completion to run.
    #
    # If a callback raises a `StandardError`, the remaining callbacks still run,
    # the response stays finished with its environment readable through {#env},
    # and the first error is re-raised once the queue is drained. Later errors
    # are discarded to keep the notification idempotent. Any other exception
    # (such as `Interrupt`) stops the queue immediately, but the response is
    # still marked finished before it propagates.
    #
    # @param env [Faraday::Env, Hash] the response environment
    # @return [Faraday::Response] self
    # @raise [RuntimeError] if the response was already finished when it was
    #   created rather than through this method
    def finish(env)
      return self unless begin_completion(env) == :owner

      begin
        first_error = run_complete_callbacks
      ensure
        # A non-StandardError (e.g. Interrupt) still leaves a usable response.
        settle_completion
      end
      raise first_error if first_error

      self
    end

    def success?
      finished? && env.success?
    end

    def to_hash
      {
        status: status, body: body,
        response_headers: headers,
        url: url
      }
    end

    # because @on_complete_callbacks cannot be marshalled
    def marshal_dump
      finished? ? to_hash : nil
    end

    def marshal_load(env)
      @env = Env.from(env) if env
      reset_completion_state(finished: finished?)
    end

    # Expand the env with more properties, without overriding existing ones.
    # Useful for applying request params after restoring a marshalled Response.
    def apply_request(request_env)
      raise "response didn't finish yet" unless finished?

      @env = Env.from(request_env).update(@env)
      self
    end

    private

    # Transitions from `:pending` to `:finishing`, storing the response env.
    #
    # Only the notification that wins the transition owns the callback run. A
    # repeat from the finishing thread is a no-op, which keeps re-entrant
    # notifications from deadlocking. A notification from another thread waits
    # until the callbacks are done, so every caller observes the same completed
    # response.
    #
    # @param env [Faraday::Env, Hash]
    # @return [Symbol] `:owner` when the caller has to run the callbacks,
    #   `:observed` when another notification already handled completion
    # @raise [RuntimeError] if the response came already finished from
    #   {Faraday::Response#initialize}
    def begin_completion(env)
      @completion_monitor.synchronize do
        case @completion_state
        when :pending
          @completion_state = :finishing
          @finishing_thread = Thread.current
          @finish_started = true
          @env = env.is_a?(Env) ? env : Env.from(env)
          :owner
        when :finishing
          unless @finishing_thread.equal?(Thread.current)
            @completion_cond.wait_until { @completion_state == :finished }
          end
          :observed
        else
          raise 'response already finished' unless @finish_started

          :observed
        end
      end
    end

    # Runs every queued callback once, in registration order.
    #
    # Callbacks appended while the queue is draining are picked up by the same
    # loop, so a callback registered from another callback runs after the ones
    # already queued. Errors do not stop the queue: the first one is returned
    # so {#finish} can re-raise it after every callback has run.
    #
    # @return [StandardError, nil] the first error raised by a callback
    def run_complete_callbacks
      first_error = nil
      while (callback = next_callback) != QUEUE_EXHAUSTED
        begin
          callback.call(@env)
        rescue StandardError => e
          first_error ||= e
        end
      end
      first_error
    end

    # Takes the next callback off the completion queue.
    #
    # The shift and the switch to `:finished` happen under the same lock that
    # appends callbacks, so a callback registered by another thread is never
    # lost.
    #
    # @return [Proc, Object] the next callback, or {QUEUE_EXHAUSTED} when the
    #   queue is empty (the response is then marked finished)
    def next_callback
      @completion_monitor.synchronize do
        if @on_complete_callbacks.empty?
          mark_completed
          QUEUE_EXHAUSTED
        else
          @on_complete_callbacks.shift
        end
      end
    end

    # Force-completes the state machine if draining stopped early.
    #
    # @return [void]
    def settle_completion
      @completion_monitor.synchronize { mark_completed }
    end

    # Switches to `:finished` and wakes threads waiting in
    # {#begin_completion}. Must be called while holding `@completion_monitor`.
    #
    # @return [void]
    def mark_completed
      return if @completion_state == :finished

      @completion_state = :finished
      @finishing_thread = nil
      @completion_cond.broadcast
    end

    # Resets the callback queue and the state machine guarding completion.
    #
    # @param finished [Boolean] whether the response already carries an env
    # @return [void]
    def reset_completion_state(finished:)
      @on_complete_callbacks = []
      @completion_monitor = Monitor.new
      @completion_cond = @completion_monitor.new_cond
      @completion_state = finished ? :finished : :pending
      @finish_started = false
      @finishing_thread = nil
    end

    # A duplicated response must not share the callback queue or the completion
    # monitor with the original, or completing one would consume the other's
    # callbacks. A copy never ends up mid-completion: it is either already
    # finished (like a response built with an env, which `#finish` refuses to
    # re-complete) or still pending and waiting for its own `#finish`.
    def initialize_copy(other)
      super
      @completion_monitor = Monitor.new
      @completion_cond = @completion_monitor.new_cond
      @on_complete_callbacks = other.instance_variable_get(:@on_complete_callbacks).dup
      @completion_state = finished? ? :finished : :pending if @completion_state == :finishing
      @finishing_thread = nil
    end
  end
end

require 'faraday/response/json'
require 'faraday/response/logger'
require 'faraday/response/raise_error'
