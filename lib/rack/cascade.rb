# frozen_string_literal: true

require 'stringio'

require_relative 'constants'

module Rack
  # Rack::Cascade tries a request on several apps, and returns the
  # first response that is not 404 or 405 (or in a list of configured
  # status codes).  If all applications tried return one of the configured
  # status codes, return the last response.
  #
  # Isolation (see the +isolate:+ option) only bounds what an unaccepted
  # application can do to the request environment and the request input.
  # It cannot undo external side effects an application has already
  # performed, such as writes to a database, the filesystem or a remote
  # service; those remain visible even though the application's response is
  # discarded and the request is offered to the next app.
  class Cascade
    # Raised when +replay_input:+ is enabled and the request input cannot be
    # staged within the configured buffer limit. The original request must be
    # retried with a larger (or unbounded) buffer; Cascade never silently
    # substitutes an empty body for an input that would need to be consumed
    # more than once.
    class InputReplayError < StandardError; end

    # Raised when the body of a response that is being replaced during a
    # cascade fails to close. This is reported separately from a cascade
    # (route-miss) status, because the failure happens while reclaiming the
    # previous response and the next app is never tried.
    class BodyCloseError < StandardError; end

    # Environment entries that represent shared, server-owned objects rather
    # than per-application request state. When isolation is enabled the
    # environment passed to each app is duplicated, but these objects are
    # deliberately kept identical so logging, error reporting, hijacking and
    # callbacks keep working across the whole request.
    SHARED_ENV_KEYS = [
      RACK_ERRORS,
      RACK_LOGGER,
      RACK_HIJACK,
      RACK_IS_HIJACK,
      RACK_EARLY_HINTS,
      RACK_RESPONSE_FINISHED,
      RACK_TEMPFILES,
    ].freeze

    # Default upper bound (in bytes) for the in-memory staging buffer used by
    # +replay_input:+. A larger body raises InputReplayError instead of
    # growing without limit.
    DEFAULT_INPUT_BUFFER_LIMIT = 1024 * 64

    # An array of applications to try in order.
    attr_reader :apps

    # Set the apps to send requests to, and what statuses result in
    # cascading. Arguments:
    #
    # apps: An enumerable of rack applications.
    # cascade_for: The statuses to use cascading for. If a response is
    #              received from an app with one of these statuses, the next
    #              app is tried.
    #
    # Options:
    #
    # isolate: When +true+, every application receives its own duplicate of
    #          the environment. Changes made by an application that is not
    #          accepted (including +PATH_INFO+, +SCRIPT_NAME+,
    #          +QUERY_STRING+ and any +rack.request.*+ parsing state) cannot
    #          leak into the next application; the request keys are restored
    #          before the next app is called. The entries listed in
    #          SHARED_ENV_KEYS remain the very same objects, so server-owned
    #          channels are shared by convention. Defaults to +false+, which
    #          preserves the historical single-environment behavior.
    #
    # replay_input: When +true+, the request input is made re-playable across
    #               attempts. Rewindable inputs are rewound before each app,
    #               and non-rewindable inputs are staged once into a bounded
    #               in-memory buffer (see +input_buffer_limit+). When +false+
    #               (the default), rewindable inputs are still rewound when
    #               isolation is enabled, but a consumed, non-rewindable
    #               input raises InputReplayError instead of being replaced
    #               by an empty body.
    #
    # input_buffer_limit: Maximum number of bytes buffered in memory for a
    #                     non-rewindable input when +replay_input+ is enabled.
    #                     Defaults to DEFAULT_INPUT_BUFFER_LIMIT. Use +nil+
    #                     for an unbounded buffer.
    def initialize(apps, cascade_for = [404, 405], isolate: false,
                   replay_input: false, input_buffer_limit: DEFAULT_INPUT_BUFFER_LIMIT)
      @apps = []
      apps.each { |app| add app }

      @cascade_for = {}
      [*cascade_for].each { |status| @cascade_for[status] = true }

      @isolate = isolate
      @replay_input = replay_input

      if input_buffer_limit && !(input_buffer_limit.is_a?(Integer) && input_buffer_limit > 0)
        raise ArgumentError, "input_buffer_limit must be a positive integer or nil"
      end
      @input_buffer_limit = input_buffer_limit
    end

    # Call each app in order. If the response uses a status that requires
    # cascading, close the replaced response body and try the next app with an
    # isolated environment (when configured). If all responses require
    # cascading, return the response from the last app.
    def call(env)
      return [404, { CONTENT_TYPE => "text/plain" }, []] if @apps.empty?

      result = nil
      last_body = nil
      # The input used across attempts. In replay mode this is either the
      # original input when it is already rewindable, or a bounded in-memory
      # buffer staged before the first app consumes anything.
      replay_io = nil
      manage_input = @isolate || @replay_input

      @apps.each_with_index do |app, index|
        # The SPEC says that the body must be closed after it has been iterated
        # by the server, or if it is replaced by a middleware action. Cascade
        # replaces the body each time a cascade happens. The body is closed by
        # its owner (Cascade for every replaced response, the server for the
        # final response). A failure to close is reported as such rather than
        # being mistaken for a route miss, so it is not swallowed and the next
        # app is not tried.
        close_replaced_body(last_body)
        last_body = nil

        attempt_env =
          if @isolate
            isolated_env = env.dup
            # Keep server-owned objects shared by convention.
            SHARED_ENV_KEYS.each do |key|
              isolated_env[key] = env[key] if env.key?(key)
            end
            isolated_env
          else
            env
          end

        if manage_input && (input = attempt_env[RACK_INPUT])
          if replay_io
            # Reuse the replayable input and rewind it so every app starts
            # reading from the beginning of the request body.
            attempt_env[RACK_INPUT] = replay_io
            replay_io.rewind
          elsif index.zero?
            if @replay_input
              if input.respond_to?(:rewind)
                replay_io = input
                input.rewind
              else
                # Stage before any app can consume the non-rewindable stream,
                # otherwise the full body could never be reconstructed.
                stage_input(attempt_env, input)
                replay_io = attempt_env[RACK_INPUT]
              end
            end
          else
            prepare_input_for_replay(attempt_env, input)
            replay_io = attempt_env[RACK_INPUT] if @replay_input
          end
        end

        result = app.call(attempt_env)
        return result unless @cascade_for.include?(result[0].to_i)
        last_body = result[2]
      end

      result
    end

    # Append an app to the list of apps to cascade.  This app will
    # be tried last.
    def add(app)
      @apps << app
    end

    # Whether the given app is one of the apps to cascade to.
    def include?(app)
      @apps.include?(app)
    end

    alias_method :<<, :add

    private

    # Close a body that is being replaced by another cascade attempt.
    # Bodies without a close method (including nil) need no action.
    def close_replaced_body(body)
      return unless body.respond_to?(:close)

      begin
        body.close
      rescue StandardError => e
        raise BodyCloseError, "failed to close cascaded response body: #{e.message} (#{e.class})"
      end
    end

    # Make sure the input handed to the next attempt can still be read.
    # Rewindable inputs are rewound; non-rewindable inputs are either staged
    # into the bounded replay buffer or rejected explicitly.
    def prepare_input_for_replay(env, input)
      if input.respond_to?(:rewind)
        input.rewind
      elsif @replay_input
        stage_input(env, input)
      else
        raise InputReplayError,
          "rack.input is not rewindable and has already been consumed; " \
          "enable replay_input to stage it within the configured buffer limit"
      end
    end

    # Copy a non-rewindable input into a rewindable, in-memory buffer and
    # replace rack.input with it. The buffer is bounded: exceeding the limit
    # raises InputReplayError rather than silently truncating the body or
    # growing without bound. The original input is not closed, as it remains
    # owned by the server.
    def stage_input(env, input)
      buffer = StringIO.new
      buffer.set_encoding(Encoding::BINARY)
      chunk = "".dup

      while input.read(1024 * 4, chunk)
        if @input_buffer_limit && buffer.size + chunk.bytesize > @input_buffer_limit
          raise InputReplayError,
            "rack.input exceeds the replay buffer limit of #{@input_buffer_limit} bytes"
        end
        buffer.write(chunk)
      end

      buffer.rewind
      env[RACK_INPUT] = buffer
    end
  end
end
