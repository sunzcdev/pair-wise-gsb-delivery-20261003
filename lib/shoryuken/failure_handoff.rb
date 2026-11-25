# frozen_string_literal: true

module Shoryuken
  # Optional, acknowledgement-safe handoff for permanently failing messages.
  #
  # A worker that classifies an exception as non-retryable can hand the failed
  # message over to a destination adapter before deleting the source receipt.
  # The destination is addressed by the stable pair of +source_queue+ and
  # +source_message_id+ - never by the SQS receipt handle, which changes on every
  # redelivery. This makes admission idempotent across "response lost" retries.
  #
  # The only rule that protects the source queue is: delete the current receipt
  # only when admission can be confirmed. An explicit rejection or an
  # indeterminate result leaves the source message untouched so SQS redelivers it.
  #
  # Adapters are duck typed. The recommended interface is:
  #
  #   class MyDestination
  #     # Idempotent admission keyed by identity. Returns a symbol or a
  #     # Shoryuken::FailureHandoff::Result.
  #     def accept_failure(identity, record)
  #       :accepted
  #     end
  #
  #     # Looks up a previously admitted result. Returns :accepted, :rejected,
  #     # :not_found/nil (or a Result). Raising means "currently unavailable".
  #     def failure_result(identity)
  #       :accepted
  #     end
  #   end
  #
  # The Dispatcher also understands many common method and keyword/positional
  # spellings (accept/admit/result/find_result, source_queue:/message_id:, ...),
  # so a local double written against slightly different naming still drives the
  # same reconciliation.
  module FailureHandoff
    ACCEPTED  = :accepted
    REJECTED  = :rejected
    UNKNOWN   = :unknown
    NOT_FOUND = :not_found

    # Stable idempotency identity of a source message. The receipt handle is
    # deliberately absent: a redelivery gets a new receipt for the same message.
    Identity = Struct.new(:source_queue, :source_message_id, keyword_init: true) do
      # @return [String] the stable identity key shared by every redelivery
      def idempotency_key
        "#{source_queue}/#{source_message_id}"
      end

      alias key idempotency_key
      alias to_s idempotency_key
      alias queue source_queue
      alias queue_name source_queue
      alias source source_queue
      alias message_id source_message_id
      alias id source_message_id
      alias source_id source_message_id
    end

    # Traceable failure record sent to the destination. It always carries the
    # original source queue and message id; the receipt handle recorded here is
    # informational and must never be used to derive the identity.
    Record = Struct.new(
      :identity,
      :source_queue,
      :source_message_id,
      :receipt_handle,
      :exception_class,
      :exception_message,
      :backtrace,
      :body,
      :worker,
      :failed_at,
      keyword_init: true
    ) do
      alias queue source_queue
      alias message_id source_message_id
      alias receipt receipt_handle

      def idempotency_key
        identity&.idempotency_key || "#{source_queue}/#{source_message_id}"
      end

      alias key idempotency_key

      # @return [Hash] traceable original source coordinates
      def source
        {
          source_queue: source_queue,
          queue: source_queue,
          source_message_id: source_message_id,
          message_id: source_message_id,
          receipt_handle: receipt_handle,
          receipt: receipt_handle
        }
      end

      # Lets simple doubles treat the record as either an object or hash.
      #
      # @param key [Symbol, String]
      # @return [Object]
      def [](key)
        member = key.to_sym
        return super(member) if members.include?(member)

        case member
        when :source then source
        when :queue, :queue_name then source_queue
        when :message_id, :id then source_message_id
        when :receipt then receipt_handle
        when :key, :idempotency_key then idempotency_key
        end
      end

      def fetch(key, *default, &block)
        member = key.to_sym
        if members.include?(member)
          value = public_send(member)
          return value unless value.nil?
        else
          value = self[key]
          return value unless value.nil?
        end

        return default.first unless default.empty?
        return block.call(key) if block

        raise KeyError, "key not found: #{key.inspect}"
      end

      attr_accessor :exception_object

      # @return [Exception, nil] original exception, when available in-process
      def exception
        exception_object
      end
    end

    # Normalized outcome of an admission attempt or result lookup.
    class Result
      STATUSES = [ACCEPTED, REJECTED, UNKNOWN, NOT_FOUND].freeze

      # @return [Symbol] one of ACCEPTED, REJECTED, UNKNOWN, NOT_FOUND
      attr_reader :status

      # @return [Object, nil] the raw adapter value
      attr_reader :value

      # @return [Exception, nil] the error that made the outcome indeterminate
      attr_reader :error

      def initialize(status, value = nil, error: nil)
        @status = normalize_status(status)
        @value  = value
        @error  = error
      end

      def accepted?
        status == ACCEPTED
      end

      def rejected?
        status == REJECTED
      end

      def unknown?
        status == UNKNOWN
      end

      def not_found?
        status == NOT_FOUND
      end

      # A terminal outcome is safe to act on without another reconciliation.
      def terminal?
        accepted? || rejected?
      end

      def self.accepted(value = nil)
        new(ACCEPTED, value)
      end

      def self.rejected(value = nil)
        new(REJECTED, value)
      end

      def self.unknown(value = nil, error: nil)
        new(UNKNOWN, value, error: error)
      end

      def self.not_found(value = nil)
        new(NOT_FOUND, value)
      end

      # Coerces the wide range of values a duck-typed adapter may return.
      #
      # A keyword-style hash (e.g. `wrap(status: :accepted)`) is accepted as the
      # value just like an explicit positional hash.
      #
      # @param value [Object] raw adapter return value
      # @param error [Exception, nil] error raised by the adapter call
      # @return [Result]
      def self.wrap(value = nil, error: nil, **keywords)
        value = keywords if value.nil? && !keywords.empty?

        return unknown(error: error) if error
        return value if value.is_a?(Result)

        case value
        when true then accepted(value)
        when false then rejected(value)
        when nil then not_found
        when Symbol, String then from_token(value.to_s, value)
        when Hash then from_hash(value)
        else from_duck(value)
        end
      end

      class << self
        private

        def from_token(token, raw)
          normalized = token.strip.downcase.tr(' -', '__')
          case normalized
          when 'accepted', 'accept', 'admitted', 'admission_accepted', 'ok', 'success', 'succeeded', 'done'
            accepted(raw)
          when 'rejected', 'reject', 'denied', 'refused', 'nack', 'nacked', 'not_accepted'
            rejected(raw)
          when 'not_found', 'missing', 'absent'
            not_found(raw)
          else
            # pending/unavailable/unknown and anything unrecognized are indeterminate
            unknown(raw)
          end
        end

        def from_hash(hash)
          if hash.key?(:accepted) || hash.key?('accepted')
            flag = hash[:accepted] || hash['accepted']
            return flag ? accepted(hash) : rejected(hash)
          end

          state = hash[:status] || hash['status'] || hash[:state] || hash['state'] ||
                  hash[:result] || hash['result'] || hash[:outcome] || hash['outcome']
          return self.new(wrap(state).status, hash) if state

          unknown(hash)
        end

        def from_duck(value)
          if value.respond_to?(:accepted?) && value.respond_to?(:rejected?)
            status = if value.accepted?
                       ACCEPTED
                     elsif value.rejected?
                       REJECTED
                     elsif value.respond_to?(:not_found?) && value.not_found?
                       NOT_FOUND
                     else
                       UNKNOWN
                     end
            new(status, value)
          elsif value.respond_to?(:status)
            new(wrap(value.status).status, value)
          else
            unknown(value)
          end
        end
      end

      private

      def normalize_status(status)
        STATUSES.include?(status) ? status : UNKNOWN
      end
    end

    # Invokes methods on an otherwise unconstrained adapter using many common
    # spellings and call shapes. Exactly one selected method is invoked per
    # {#call}; argument-shape mismatches are resolved through parameter
    # introspection rather than by retrying calls.
    class Dispatcher
      ACCEPT_METHODS = %i[
        accept_failure admit_failure accept_admission acknowledge_failure
        failure_accept create_failure record_failure store_failure
        create_admission register_failure
        accept! admit!
        accept admit admission create_admission_record
        receive_failure receive handoff_failure handover_failure
        handoff handover relay_failure relay
        deliver_failure deliver transfer_failure transfer
        publish_failure publish dead_letter archive_failure archive
        create record store call
      ].freeze

      QUERY_METHODS = %i[
        failure_result admission_result acceptance_result handoff_result handover_result
        result_for_failure result_for source_result
        find_failure find_result fetch_failure fetch_result get_failure get_result
        query_failure query_result lookup_failure lookup_result read_failure
        failure_query admission_query handoff_query
        query failure_status admission_status acceptance admission
        result status
        find_by_source find_by_identity admission_for
        find fetch lookup read get
        accepted? was_accepted? admission_accepted? admitted?
      ].freeze

      # @return [Object] the destination adapter
      attr_reader :adapter

      def initialize(adapter)
        @adapter = adapter
      end

      # Invokes the first matching candidate method.
      #
      # @param candidates [Array<Symbol>] method names in priority order
      # @param context [Hash] named values available to satisfy parameters
      # @return [Array] one of:
      #   `[:ok, method_name, value]`, `[:missing, nil, nil]`,
      #   `[:error, method_name, exception]`
      def call(candidates, context)
        resolved = resolve(candidates, context)
        return [:missing, nil, nil] unless resolved

        name, method_obj, args, kwargs = resolved
        value = method_obj.call(*args, **kwargs)
        [:ok, name, value]
      rescue => e
        [:error, name, e]
      end

      private

      # Finds the first candidate whose parameters can be satisfied.
      #
      # @return [(Symbol, #call, Array, Hash), nil]
      def resolve(candidates, context)
        candidates.each do |candidate|
          next unless adapter.respond_to?(candidate)

          begin
            # Proc/Method expose their real signature through #parameters;
            # #method(:call) would only show the generic #call signature.
            if candidate == :call && callable_signature?(adapter)
              callable = adapter
              parameters = adapter.parameters
            else
              callable = adapter.method(candidate)
              parameters = callable.parameters
            end

            args, kwargs = build_arguments(parameters, context)
          rescue StandardError
            # A reflection failure on one candidate must not abort the search.
            next
          end

          next if args.nil?

          return [candidate, callable, args, kwargs]
        end

        nil
      end

      # @return [Boolean] true when the object carries its own call signature
      def callable_signature?(object)
        object.is_a?(Proc) || object.is_a?(Method)
      end

      # Builds positional and keyword arguments from Method#parameters.
      #
      # @return [(Array, Hash), nil] nil when a required parameter cannot be met
      def build_arguments(parameters, context)
        values = parameter_values(context)
        args = []

        required_positional = parameters.select { |type, _| type == :req }
        required_positional.each_with_index do |(_, name), index|
          if name && values.key?(name)
            args << values[name]
          else
            args << positional_fallback(index, required_positional.size, context)
          end
        end

        kwargs = {}
        parameters.each do |type, name|
          next unless name

          case type
          when :keyreq
            return nil unless values.key?(name)

            kwargs[name] = values[name]
          when :key
            kwargs[name] = values[name] if values.key?(name)
          when :keyrest
            keyword_spread(context).each { |key, value| kwargs[key] = value unless kwargs.key?(key) }
          end
        end

        [args, kwargs]
      end

      # Values addressable by the many parameter names adapters/doubles use.
      def parameter_values(ctx)
        identity = ctx[:identity]
        record   = ctx[:record]
        queue    = ctx[:source_queue]
        mid      = ctx[:source_message_id]
        receipt  = ctx[:receipt_handle]
        error    = ctx[:exception]
        payload  = ctx[:payload]
        key      = ctx[:key]

        {
          identity: identity,
          handoff_identity: identity,
          idempotent_identity: identity,
          key: key,
          idempotency_key: key,
          identity_key: key,
          handoff_id: key,
          dedup_key: key,
          stable_key: key,
          source_key: key,

          source_queue: queue,
          source: queue,
          queue: queue,
          queue_name: queue,
          from_queue: queue,
          origin_queue: queue,
          q: queue,

          source_message_id: mid,
          message_id: mid,
          source_id: mid,
          origin_message_id: mid,
          msg_id: mid,
          mid: mid,
          id: mid,

          receipt_handle: receipt,
          receipt: receipt,
          receipt_id: receipt,

          record: record,
          failure_record: record,
          failure: record,
          event: record,
          handoff_record: record,

          payload: payload,
          attributes: payload,
          data: payload,
          params: payload,
          failure_payload: payload,

          exception: error,
          error: error,
          err: error,

          exception_class: ctx[:exception_class],
          exception_message: ctx[:exception_message],
          backtrace: ctx[:backtrace],
          body: ctx[:body],
          worker: ctx[:worker]
        }
      end

      # Canonical keyword hash splatted into a `**kwargs` parameter.
      def keyword_spread(ctx)
        {
          identity: ctx[:identity],
          key: ctx[:key],
          source_queue: ctx[:source_queue],
          source_message_id: ctx[:source_message_id],
          receipt_handle: ctx[:receipt_handle],
          record: ctx[:record],
          payload: ctx[:payload],
          exception: ctx[:exception],
          body: ctx[:body],
          worker: ctx[:worker]
        }
      end

      # Best-effort values for unnamed/generic positional parameters.
      def positional_fallback(index, count, ctx)
        case count
        when 1
          ctx[:identity]
        when 2
          index.zero? ? ctx[:source_queue] : ctx[:source_message_id]
        when 3
          [ctx[:source_queue], ctx[:source_message_id], ctx[:record]][index]
        else
          sequence = [
            ctx[:source_queue],
            ctx[:source_message_id],
            ctx[:record],
            ctx[:exception],
            ctx[:receipt_handle],
            ctx[:body]
          ]
          sequence[index]
        end
      end
    end

    # Process-local memory of identities whose admission response was ambiguous.
    # It is an optimization that makes a redelivery reconcile by query before
    # calling admit again. Correctness does not depend on it: the destination's
    # stable identity and idempotent admission remain the source of truth.
    class RecoveryMarks
      DEFAULT_TTL = 3_600
      MAX_ENTRIES = 10_000

      def initialize(ttl: DEFAULT_TTL, max_entries: MAX_ENTRIES)
        @ttl         = ttl
        @max_entries = max_entries
        @keys        = {}
        @mutex       = Mutex.new
      end

      def add(key)
        @mutex.synchronize do
          now = Time.now
          @keys[key] = now
          prune_expired_locked(now)
          evict_oldest_locked if @keys.size > @max_entries
        end
      end

      def delete(key)
        @mutex.synchronize { @keys.delete(key) }
      end

      def pending?(key)
        @mutex.synchronize do
          marked_at = @keys[key]
          next false unless marked_at
          next true if Time.now - marked_at < @ttl

          @keys.delete(key)
          false
        end
      end

      def clear
        @mutex.synchronize { @keys.clear }
      end

      private

      def prune_expired_locked(now)
        @keys.delete_if { |_key, marked_at| now - marked_at >= @ttl }
      end

      def evict_oldest_locked
        oldest_key, = @keys.min_by { |_key, marked_at| marked_at }
        @keys.delete(oldest_key) if oldest_key
      end
    end

    # Coordinates idempotent admission with query-based reconciliation.
    #
    # Sequence for the first observed delivery:
    #   1. Admit under the stable source identity.
    #   2. A definitive accepted/rejected response decides immediately.
    #   3. An ambiguous admission (lost response, timeout, unavailable) is
    #      reconciled through one result query. Only confirmed acceptance yields
    #      +accepted?+; otherwise the identity is marked for query-first retry.
    #
    # On redelivery after an ambiguous attempt, query first. This avoids another
    # admission call when the original accepted result is now queryable. A
    # destination-owned idempotency key still protects new processes or cleared
    # marks.
    class Gateway
      include Shoryuken::Util

      # @return [Dispatcher]
      attr_reader :dispatcher

      def initialize(adapter, lookup_first: false, recovery_marks: FailureHandoff.recovery_marks)
        @dispatcher     = adapter.is_a?(Dispatcher) ? adapter : Dispatcher.new(adapter)
        @lookup_first   = lookup_first
        @recovery_marks = recovery_marks
      end

      # Runs the full confirmation protocol for one source message.
      #
      # @param source_queue [String]
      # @param source_message_id [String]
      # @param receipt_handle [String, nil] current receipt (traceability only)
      # @param exception [Exception, nil]
      # @param body [Object]
      # @param worker [Object, nil]
      # @return [Result]
      def confirm(source_queue:, source_message_id:, receipt_handle: nil, exception: nil, body: nil, worker: nil)
        identity = Identity.new(source_queue: source_queue.to_s, source_message_id: source_message_id.to_s)
        record   = build_record(identity, receipt_handle, exception, body, worker)
        context  = build_context(identity, record, receipt_handle, exception, body, worker)
        key      = identity.idempotency_key

        if lookup_first?(key)
          existing = query(context)
          if existing.terminal?
            @recovery_marks.delete(key)
            return existing
          end
        end

        admission = admit(context)
        if admission.terminal?
          @recovery_marks.delete(key)
          return admission
        end

        reconciled = query(context)
        if reconciled.terminal?
          @recovery_marks.delete(key)
          return reconciled
        end

        @recovery_marks.add(key)
        Result.unknown(error: admission.error || reconciled.error)
      end

      # @return [Boolean] whether to query before invoking admission
      def lookup_first?(key)
        @lookup_first || @recovery_marks.pending?(key)
      end

      # Queries the destination, normalizing missing/error outcomes.
      #
      # A lookup that returns a present but unlabelled record is interpreted as
      # the stored admission result (accepted); explicit statuses always win.
      #
      # @return [Result] never raises
      def query(context)
        status, name, value_or_error = dispatcher.call(Dispatcher::QUERY_METHODS, context)
        case status
        when :ok then interpret_result(value_or_error, name)
        when :error then Result.unknown(error: value_or_error)
        else Result.unknown
        end
      end

      # Interprets an admission return value or a result lookup.
      #
      # A boolean acceptance predicate answers "accepted yet?": false means
      # not found/pending rather than an explicit rejection. A present but
      # unlabelled record/hash is the stored admission and counts as accepted;
      # explicit statuses always win.
      #
      # @return [Result]
      def interpret_result(value, method_name = nil)
        # A boolean acceptance predicate answers "accepted yet?": false means
        # not found/pending rather than an explicit rejection.
        if method_name.to_s.end_with?('?')
          return value ? Result.accepted(value) : Result.not_found(value)
        end

        result = Result.wrap(value)
        return result if result.terminal? || result.not_found?

        case value
        when Hash
          return Result.not_found(value) if value.empty?
          return result if state_key?(value)

          Result.accepted(value)
        when Symbol, String
          result
        else
          # A present record/object without its own status is the found admission.
          value.respond_to?(:status) ? result : Result.accepted(value)
        end
      end

      # Kept as a descriptive alias for lookup callers.
      alias interpret_lookup interpret_result

      STATE_KEYS = %i[status state result outcome accepted].freeze

      def state_key?(hash)
        STATE_KEYS.any? { |key| hash.key?(key) || hash.key?(key.to_s) }
      end

      # Admits one failure, normalizing the outcome.
      #
      # A normal return carrying a present but unlabelled record also counts as
      # confirmation; a raised call is the "lost response" case and becomes
      # unknown so the caller reconciles via lookup.
      #
      # @return [Result] never raises
      def admit(context)
        status, name, value_or_error = dispatcher.call(Dispatcher::ACCEPT_METHODS, context)
        case status
        when :ok
          interpret_result(value_or_error, name).tap do |result|
            logger.debug { "Failure handoff via #{name} for #{context[:key]} -> #{result.status}" }
          end
        when :error then Result.unknown(error: value_or_error)
        else Result.unknown
        end
      end

      private

      def build_record(identity, receipt_handle, exception, body, worker)
        Record.new(
          identity: identity,
          source_queue: identity.source_queue,
          source_message_id: identity.source_message_id,
          receipt_handle: receipt_handle,
          exception_class: exception&.class&.name,
          exception_message: exception&.message,
          backtrace: exception&.backtrace,
          body: body,
          worker: worker,
          failed_at: Time.now
        ).tap { |record| record.exception_object = exception }
      end

      def build_context(identity, record, receipt_handle, exception, body, worker)
        payload = {
          source_queue: identity.source_queue,
          source_message_id: identity.source_message_id,
          receipt_handle: receipt_handle,
          exception_class: record.exception_class,
          exception_message: record.exception_message,
          backtrace: record.backtrace,
          body: body,
          failed_at: record.failed_at
        }

        {
          identity: identity,
          key: identity.idempotency_key,
          source_queue: identity.source_queue,
          source_message_id: identity.source_message_id,
          receipt_handle: receipt_handle,
          record: record,
          payload: payload,
          exception: exception,
          exception_class: record.exception_class,
          exception_message: record.exception_message,
          backtrace: record.backtrace,
          body: body,
          worker: worker
        }
      end
    end

    # Optional base class for destination adapters. Shoryuken only requires the
    # duck type (see {Dispatcher}); subclassing this is purely documentation.
    #
    # @example
    #   class MyAdapter < Shoryuken::FailureHandoff::Adapter
    #     def accept_failure(identity, record)
    #       store.put(identity.idempotency_key, record.to_h)
    #       :accepted
    #     end
    #
    #     def failure_result(identity)
    #       store.status(identity.idempotency_key) # :accepted / :rejected / nil
    #     end
    #   end
    class Adapter
      # Idempotently admits one failure record.
      #
      # @param identity [Identity] stable source queue + message id
      # @param record [Record] traceable failure details
      # @return [Symbol, Result, Boolean, Hash] an acceptance outcome
      def accept_failure(identity, record)
        raise NotImplementedError, "#{self.class} must implement #accept_failure(identity, record)"
      end

      # Looks up a previously admitted result.
      #
      # @param identity [Identity] stable source queue + message id
      # @return [Symbol, Result, Boolean, Hash, nil] the stored result; nil when unknown
      def failure_result(identity)
        raise NotImplementedError, "#{self.class} must implement #failure_result(identity)"
      end
    end

    # A simple thread-safe, idempotent in-memory destination. It doubles as the
    # reference adapter and as a convenient local double in tests.
    class MemoryDestination < Adapter
      def initialize
        @records = {}
        @mutex   = Mutex.new
      end

      # Idempotently accepts a failure for the identity.
      #
      # @return [Symbol] :accepted
      def accept_failure(identity, record = nil)
        key = key_of(identity)
        @mutex.synchronize do
          @records[key] ||= {
            status: ACCEPTED,
            identity: identity,
            record: record,
            accepted_at: Time.now
          }
        end
        :accepted
      end

      # @return [Symbol, nil] :accepted when known, nil otherwise
      def failure_result(identity)
        @mutex.synchronize { @records[key_of(identity)] }&.fetch(:status)
      end

      # @return [Array] the stored records (one per stable identity)
      def records
        @mutex.synchronize { @records.values.dup }
      end

      def size
        @mutex.synchronize { @records.size }
      end

      private

      def key_of(identity)
        identity.respond_to?(:idempotency_key) ? identity.idempotency_key : identity.to_s
      end
    end

    class << self
      # Process-local ambiguous-identity memory shared by middleware calls.
      #
      # @return [RecoveryMarks]
      def recovery_marks
        @recovery_marks ||= RecoveryMarks.new
      end

      # Clears process-local recovery memory. Primarily useful between tests.
      #
      # @return [void]
      def reset_recovery_marks!
        @recovery_marks = RecoveryMarks.new
      end

      # Module-level convenience entry point.
      #
      # @param adapter [Object, nil] destination adapter (defaults to global config)
      # @return [Result]
      def confirm(adapter = nil, **kwargs)
        adapter ||= Shoryuken.failure_handoff
        raise ArgumentError, 'a failure handoff adapter is required' if adapter.nil?

        Gateway.new(adapter).confirm(**kwargs)
      end

      # @return [Gateway] for the given adapter
      def gateway(adapter = nil)
        adapter ||= Shoryuken.failure_handoff
        Gateway.new(adapter)
      end
    end

    # Naming aliases for the coordinator entry point.
    Coordinator = Gateway unless const_defined?(:Coordinator)
    Handler = Gateway unless const_defined?(:Handler)
  end
end
