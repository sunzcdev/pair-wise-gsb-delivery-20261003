require "json"
module SemanticLogger
  module Formatters
    # Formatter conforming to the Elastic Common Schema (ECS).
    #
    # Emits log events using the nested field names defined by ECS so that they
    # integrate cleanly with Filebeat and the Elastic stack (Elasticsearch,
    # Kibana) without requiring an ingest pipeline to rename fields.
    #
    # Usage:
    #   SemanticLogger.add_appender(io: $stdout, formatter: :ecs)
    #
    #   # Route the payload, metric, and other SemanticLogger-specific data into
    #   # a custom top-level namespace (default "semantic_logger"):
    #   SemanticLogger.add_appender(io: $stdout, formatter: {ecs: {namespace: "my_app"}})
    #
    #   # Or merge the payload directly into ECS `labels` instead of a namespace:
    #   SemanticLogger.add_appender(io: $stdout, formatter: {ecs: {namespace: nil}})
    #
    #   # Or merge named tags and the payload into the top level of the ECS document,
    #   # declaring which source keeps a field when they collide with the ECS mapping:
    #   SemanticLogger.add_appender(io: $stdout, formatter: {ecs: {merge: :top_level, on_conflict: :ecs}})
    #
    # == Field mapping (SemanticLogger -> ECS 8.x)
    #   time                  -> @timestamp        (ISO-8601)
    #   level                 -> log.level
    #   name                  -> log.logger
    #   file_name / line      -> log.origin.file.name / log.origin.file.line
    #   method                -> log.origin.function
    #   message               -> message
    #   thread_name           -> process.thread.name
    #   pid                   -> process.pid
    #   host                  -> host.hostname
    #   application           -> service.name
    #   environment           -> service.environment
    #   exception             -> error.type / error.message / error.stack_trace
    #   duration              -> event.duration   (nanoseconds, as required by ECS)
    #   tags                  -> tags             (ECS top-level array)
    #   named_tags            -> labels.*         (scalar key/value pairs)
    #   payload               -> <namespace>.*    (or labels.* when namespace is nil)
    #   metric/metric_amount  -> <namespace>.metric / <namespace>.metric_amount
    #   dimensions            -> <namespace>.dimensions
    #
    # == Top-level merge and conflicts
    # With `merge: :top_level` the named tags and the payload merge into the top
    # level of the ECS document as custom fields, instead of living under
    # `labels` and the namespace. A key that names a field the ECS mapping
    # already wrote is a conflict, resolved as follows:
    #
    #   * Protected ECS fields (PROTECTED_FIELDS: `@timestamp` and `ecs`) always
    #     keep the value written by the ECS mapping, whichever source is
    #     preferred; the incoming value is always relocated.
    #   * Otherwise `on_conflict` declares which source keeps the field:
    #     `:ecs` (the default) keeps the ECS mapping's value and relocates the
    #     incoming one; `:log` keeps the named tag or payload value. With
    #     `:log`, two Hashes at one field are deep-merged, so a payload such as
    #     `{service: {name: "..."}}` can supply `service.name` while the ECS
    #     mapping keeps `service.environment`.
    #   * A scalar never replaces an ECS object field: when the field holds a
    #     Hash and the incoming value is a scalar, the Hash keeps the field and
    #     the scalar is relocated, whichever source is preferred.
    #
    # A relocated value is moved to the home its source has without top-level
    # merging, so the output itself explains where it went: a named tag moves to
    # `labels.<key>`, a payload value to `<namespace>.payload.<key>` (or
    # `labels.<key>` when `namespace` is nil). No value from the log is dropped;
    # only the ECS mapping's own displaced values are not relocated, since they
    # remain available from the logger/appender configuration. Between the two
    # log-data sources the payload wins over the named tags, as in the `:logfmt`
    # formatter, and the displaced named tag is relocated to `labels`.
    #
    # Merging never mutates the log: the named tags, the payload, and any
    # arrays or hashes they share with other log entries are left exactly as
    # they were, so another appender (such as the `:json` formatter) still
    # reads the original `Log` by its own rules.
    #
    # == Reference
    #   * https://www.elastic.co/docs/reference/ecs
    #   * https://www.elastic.co/docs/reference/ecs/ecs-custom-fields-in-ecs
    class Ecs < Raw
      # ECS version this formatter targets.
      ECS_VERSION = "8.11.0".freeze

      # Top-level ECS fields that always keep the value written by the ECS
      # field mapping, whichever source `on_conflict` prefers. A named tag or
      # payload key that names one of these is always relocated.
      PROTECTED_FIELDS = %i[@timestamp ecs].freeze

      # namespace: [String|Symbol|nil]
      #   Top-level field set used to hold SemanticLogger-specific data that has
      #   no native ECS home (payload, metric, metric_amount, dimensions). A proper-noun
      #   namespace is guaranteed never to collide with a current or future ECS
      #   field. Set to nil to merge the payload into ECS `labels` instead.
      #   Default: "semantic_logger"
      #
      # merge: [nil|:top_level]
      #   :top_level merges the named tags and the payload into the top level of
      #   the ECS document as custom fields, resolving collisions with the ECS
      #   field mapping as `on_conflict` declares. nil (the default) keeps them
      #   under `labels` and the namespace.
      #
      # on_conflict: [:ecs|:log]
      #   Which source keeps a contested field when `merge: :top_level` merges a
      #   named tag or payload key that the ECS field mapping already wrote:
      #   :ecs (the default) keeps the ECS mapping's value, :log keeps the log's
      #   value. Protected ECS fields (PROTECTED_FIELDS) always keep the ECS
      #   mapping's value. The losing value is relocated to its source's home
      #   (`labels` for named tags, the namespace for the payload), so where it
      #   went is visible in the output.
      attr_reader :namespace, :merge, :on_conflict

      def initialize(namespace: "semantic_logger", merge: nil, on_conflict: nil, time_format: :iso_8601, time_key: :timestamp, **args)
        @namespace   = namespace&.to_sym
        @merge       = merge&.to_sym
        @on_conflict = (on_conflict || :ecs).to_sym

        raise ArgumentError, "merge must be nil or :top_level, got: #{merge.inspect}" unless @merge.nil? || @merge == :top_level

        unless %i[ecs log].include?(@on_conflict)
          raise ArgumentError, "on_conflict must be :ecs or :log, got: #{on_conflict.inspect}"
        end

        super(time_format: time_format, time_key: time_key, **args)
      end

      # Returns the log event as a single line of ECS-formatted JSON, so it can
      # be written to stdout / a file and shipped by Filebeat or Elastic Agent.
      def call(log, logger)
        Utils.to_json(ecs_hash(super))
      end

      # Returns a batch of log events as a single JSON array.
      def batch(logs, logger)
        "[#{logs.map { |log| call(log, logger) }.join(',')}]"
      end

      private

      # Remap the flat hash built by Raw#call into the nested ECS field layout.
      def ecs_hash(hash)
        result = base(hash)
        result[:process] = process(hash)
        result[:host]    = {hostname: hash[:host]} if hash[:host]
        add_service(result, hash)
        add_origin(result, hash)
        add_event(result, hash)
        result[:tags] = hash[:tags] if hash[:tags]
        add_error(result, hash)
        add_extras(result, hash)
        result
      end

      def base(hash)
        log = {level: hash[:level].to_s}
        log[:logger] = hash[:name] if hash[:name]
        {
          "@timestamp": hash[:timestamp],
          message:      hash[:message],
          ecs:          {version: ECS_VERSION},
          log:          log
        }
      end

      def process(hash)
        result = {pid: hash[:pid]}
        result[:thread] = {name: hash[:thread].to_s} if hash[:thread]
        result
      end

      def add_service(result, hash)
        service = {}
        service[:name]        = hash[:application] if hash[:application]
        service[:environment] = hash[:environment] if hash[:environment]
        result[:service] = service unless service.empty?
      end

      def add_origin(result, hash)
        return unless hash[:file]

        origin            = {file: {name: hash[:file], line: hash[:line]}.compact}
        origin[:function] = hash[:method] if hash[:method]
        result[:log][:origin] = origin
      end

      # ECS event.duration is measured in nanoseconds.
      def add_event(result, hash)
        return unless hash[:duration_ms]

        result[:event] = {duration: (hash[:duration_ms] * 1_000_000).round}
      end

      def add_error(result, hash)
        return unless hash[:exception]

        result[:error] = {
          type:        hash[:exception][:name],
          message:     hash[:exception][:message],
          stack_trace: stack_trace(Array(hash[:exception][:stack_trace]))
        }
      end

      # Place SemanticLogger-specific data (payload, metric, dimensions) that has no native
      # ECS home into the configured namespace, plus named_tags into labels.
      def add_extras(result, hash)
        return add_merged_extras(result, hash) if merge == :top_level

        labels = hash[:named_tags].is_a?(Hash) ? hash[:named_tags].dup : {}

        extra = {}
        extra[:payload]       = hash[:payload] if hash[:payload]
        extra[:metric]        = hash[:metric] if hash[:metric]
        extra[:metric_amount] = hash[:metric_amount] if hash[:metric_amount]
        extra[:dimensions]    = hash[:dimensions] if hash[:dimensions]

        unless extra.empty?
          namespace ? result[namespace] = extra : labels.merge!(extra)
        end

        result[:labels] = labels unless labels.empty?
      end

      # Merge the named tags and the payload into the top level of the ECS
      # document, resolving collisions with the ECS field mapping as
      # `on_conflict` declares. The metric fields keep their namespaced home.
      def add_merged_extras(result, hash)
        # Keys the ECS field mapping wrote, so a conflict can tell its value
        # apart from one an earlier source merged in.
        ecs_keys = result.keys

        merge_source(result, hash[:named_tags], :named_tags, ecs_keys) if hash[:named_tags].is_a?(Hash)
        if hash[:payload].is_a?(Hash)
          merge_source(result, hash[:payload], :payload, ecs_keys)
        elsif hash[:payload]
          namespaced_extras(result)[:payload] = hash[:payload]
        end

        extra = {}
        extra[:metric]        = hash[:metric] if hash[:metric]
        extra[:metric_amount] = hash[:metric_amount] if hash[:metric_amount]
        extra[:dimensions]    = hash[:dimensions] if hash[:dimensions]
        namespaced_extras(result).merge!(extra) unless extra.empty?
      end

      # Merge one source's keys into the top level of the ECS document.
      def merge_source(result, data, source, ecs_keys)
        data.each_pair do |key, value|
          key = key.to_sym if key.respond_to?(:to_sym)
          if result[key].nil?
            result[key] = value
          else
            resolve_conflict(result, key, value, source, ecs_keys)
          end
        end
      end

      # Resolve a key that both the ECS document and a merged source claim.
      def resolve_conflict(result, key, value, source, ecs_keys)
        existing = result[key]

        # Protected ECS fields always keep the value written by the ECS mapping.
        return relocate(result, source, key, value) if PROTECTED_FIELDS.include?(key)

        if existing.is_a?(Hash) && value.is_a?(Hash)
          merge_hashes(result, key, value, source, ecs_keys)
        elsif existing.is_a?(Hash)
          # A scalar never replaces an ECS object field; it moves to its source's home.
          relocate(result, source, key, value)
        elsif value.is_a?(Hash)
          take_field(result, key, value, existing, source, ecs_keys)
        elsif ecs_keys.include?(key)
          if on_conflict == :ecs
            relocate(result, source, key, value)
          else
            result[key] = value
          end
        else
          # Between the log-data sources the later one (the payload) wins, and
          # the displaced named tag moves to its home.
          relocate(result, :named_tags, key, existing)
          result[key] = value
        end
      end

      # An incoming Hash against the ECS mapping's Hash: with :log priority the
      # two are deep-merged so the log's values win key by key; with :ecs
      # priority the ECS object keeps the field and the incoming Hash moves to
      # its source's home. Against an earlier source's Hash the two are
      # deep-merged with the later source winning.
      def merge_hashes(result, key, value, source, ecs_keys)
        if ecs_keys.include?(key)
          if on_conflict == :log
            result[key] = deep_merge(result[key], value)
          else
            relocate(result, source, key, value)
          end
        else
          result[key] = deep_merge(result[key], value, :log)
        end
      end

      # An incoming Hash against an existing scalar: the ECS mapping's scalar
      # keeps the field unless `on_conflict` is :log; an earlier source's
      # scalar always yields to the Hash and moves to its home.
      def take_field(result, key, value, existing, source, ecs_keys)
        if ecs_keys.include?(key)
          if on_conflict == :ecs
            relocate(result, source, key, value)
          else
            result[key] = value
          end
        else
          relocate(result, :named_tags, key, existing)
          result[key] = value
        end
      end

      # Deep-merge incoming into existing, returning a new Hash so that neither
      # the ECS document nor the log's own data is mutated. Nested scalar
      # conflicts follow `prefer` (:ecs keeps the existing value, :log takes
      # the incoming one); a nested Hash always wins over a scalar.
      def deep_merge(existing, incoming, prefer = on_conflict)
        merged = existing.dup
        incoming.each_pair do |key, value|
          key     = key.to_sym if key.respond_to?(:to_sym)
          current = merged[key]
          if current.is_a?(Hash) && value.is_a?(Hash)
            merged[key] = deep_merge(current, value, prefer)
          elsif current.nil?
            merged[key] = value
          elsif !current.is_a?(Hash) && (value.is_a?(Hash) || prefer == :log)
            merged[key] = value
          end
          # A current Hash against an incoming scalar keeps its field.
        end
        merged
      end

      # Move a value that lost its field to the home its source has without
      # top-level merging, so the output shows where the value went. Within a
      # home the later source (the payload) wins, as it does between sources.
      def relocate(result, source, key, value)
        home    = relocation_home(result, source)
        current = home[key]
        home[key] = current.is_a?(Hash) && value.is_a?(Hash) ? deep_merge(current, value, :log) : value
      end

      # The home a source's data has without top-level merging: `labels` for
      # named tags, the namespace for the payload (or `labels` when there is no
      # namespace).
      def relocation_home(result, source)
        return hash_at(result, :labels) unless source == :payload && namespace

        hash_at(namespaced_extras(result), :payload)
      end

      def namespaced_extras(result)
        namespace ? hash_at(result, namespace) : hash_at(result, :labels)
      end

      # The Hash at key, replacing a non-Hash so merged data always has a Hash home.
      def hash_at(hash, key)
        value = hash[key]
        value.is_a?(Hash) ? value : (hash[key] = {})
      end
    end
  end
end
