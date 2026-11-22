# frozen_string_literal: true

require 'forwardable'

module Faraday
  # Response represents an HTTP response from making an HTTP request.
  class Response
    extend Forwardable
    extend MiddlewareRegistry

    # Version of the snapshot produced by {#marshal_dump} and understood by
    # {#marshal_load}. Bump it whenever the structure of the snapshot changes.
    MARSHAL_VERSION = 1

    # Key holding the {MARSHAL_VERSION} inside a versioned snapshot.
    MARSHAL_VERSION_KEY = :marshalling_version

    # Key holding the snapshot state inside a versioned snapshot.
    MARSHAL_STATE_KEY = :state

    # State of a snapshot taken from a finished response.
    MARSHAL_STATE_FINISHED = :finished

    # State of a snapshot taken from a response that never finished.
    MARSHAL_STATE_PENDING = :pending

    # Keys persisted by a snapshot of a finished response. Only these
    # marshallable, response-facing values are saved: the registered
    # +on_complete+ callbacks, the in-flight adapter and any external IO (such
    # as streaming handles or request procs) are deliberately not part of it.
    MARSHAL_SNAPSHOT_KEYS = %i[status body response_headers url].freeze

    def initialize(env = nil)
      @env = Env.from(env) if env
      @on_complete_callbacks = []
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

    def on_complete(&block)
      if finished?
        yield(env)
      else
        @on_complete_callbacks << block
      end
      self
    end

    def finish(env)
      raise 'response already finished' if finished?

      @env = env.is_a?(Env) ? env : Env.from(env)
      callbacks = @on_complete_callbacks
      @on_complete_callbacks = []
      callbacks.each { |callback| callback.call(@env) }
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

    # Build a marshallable snapshot of the response.
    #
    # The snapshot is versioned ({MARSHAL_VERSION}) so the structure can evolve
    # without breaking older data. It only contains the response values a
    # finished response exposes through its public readers (status, body,
    # response headers and url): the registered +on_complete+ callbacks, the
    # in-flight adapter and any external IO are not part of the snapshot.
    #
    # A response that has not finished snapshots as a versioned, still-pending
    # payload rather than carrying any request or adapter state.
    #
    # @return [Hash] a versioned, marshallable snapshot
    def marshal_dump
      snapshot = {
        MARSHAL_VERSION_KEY => MARSHAL_VERSION,
        MARSHAL_STATE_KEY => finished? ? MARSHAL_STATE_FINISHED : MARSHAL_STATE_PENDING
      }
      return snapshot unless finished?

      snapshot.merge(to_hash)
    end

    # Restore a response from a snapshot produced by {#marshal_dump}.
    #
    # Snapshots written by older Faraday versions (a plain +nil+ for an
    # unfinished response or a flat +to_hash+ Hash for a finished one) are
    # still accepted. An unknown snapshot version, or a value that is not a
    # recognizable snapshot, raises {Faraday::SnapshotError} so corrupt or
    # forward-incompatible data can be told apart from a legitimate response.
    #
    # @param snapshot [nil, Hash] the marshalled snapshot
    # @return [void]
    # @raise [Faraday::SnapshotError] if the snapshot cannot be recognized
    def marshal_load(snapshot)
      @on_complete_callbacks = []

      case snapshot
      when nil
        # Legacy snapshot of a response that had not finished.
        @env = nil
      when Hash
        load_snapshot(snapshot)
      else
        raise Faraday::SnapshotError,
              "unrecognized Faraday::Response snapshot: #{snapshot.inspect}"
      end
    end

    # Expand the env with more properties, without overriding existing ones.
    # Useful for applying request params after restoring a marshalled Response.
    def apply_request(request_env)
      raise "response didn't finish yet" unless finished?

      @env = Env.from(request_env).update(@env)
      self
    end

    private

    # Populate the response from a versioned snapshot or a legacy flat hash.
    def load_snapshot(snapshot)
      version = snapshot[MARSHAL_VERSION_KEY]

      if version.nil?
        # Legacy (unversioned) snapshot: a flat #to_hash of a finished response.
        load_finished_snapshot(snapshot)
        return
      end

      unless version == MARSHAL_VERSION
        raise Faraday::SnapshotError,
              "unsupported Faraday::Response snapshot version #{version.inspect}, " \
              "expected #{MARSHAL_VERSION}"
      end

      case snapshot[MARSHAL_STATE_KEY]
      when MARSHAL_STATE_FINISHED
        load_finished_snapshot(snapshot.slice(*MARSHAL_SNAPSHOT_KEYS))
      when MARSHAL_STATE_PENDING
        @env = nil
      else
        raise Faraday::SnapshotError,
              "unrecognized Faraday::Response snapshot state: #{snapshot[MARSHAL_STATE_KEY].inspect}"
      end
    end

    # Rebuild the Env from the response-facing values of a finished snapshot.
    def load_finished_snapshot(values)
      @env = Env.from(values)
    end
  end
end

require 'faraday/response/json'
require 'faraday/response/logger'
require 'faraday/response/raise_error'
