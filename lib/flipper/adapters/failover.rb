require 'flipper/adapters/read_receipt'

module Flipper
  module Adapters
    class Failover
      include ::Flipper::Adapter

      # Public: Build a new failover instance.
      #
      # primary   - The primary flipper adapter.
      # secondary - The secondary flipper adapter which services reads when
      #             the primary adapter is unavailable.
      # options   - Hash of options:
      #             :dual_write - Boolean, whether to update secondary when
      #                           primary is updated
      #             :errors - Array of exception types for which to failover
      #             :receipt - Boolean, whether to collect a ReadReceipt of
      #                        where every readable value came from. Defaults
      #                        to false so existing behavior is unchanged.

      attr_reader :primary, :secondary

      # Public: The Flipper::Adapters::ReadReceipt collected when the adapter
      # is built with `receipt: true`, nil otherwise.
      attr_reader :read_receipt

      def initialize(primary, secondary, options = {})
        @primary = primary
        @secondary = secondary

        @dual_write = options.fetch(:dual_write, false)
        @errors = options.fetch(:errors, [ StandardError ])
        @read_receipt = ReadReceipt.new if options.fetch(:receipt, false)
      end

      def adapter_stack
        "#{name}(primary: #{@primary.adapter_stack}, secondary: #{@secondary.adapter_stack})"
      end

      def read_only?
        @primary.read_only? || (@dual_write && @secondary.read_only?)
      end

      def features
        result = @primary.features
        record_features(result, source: :primary, status: :ok)
        result
      rescue *@errors => error
        result = @secondary.features
        record_features(result, source: :secondary, status: :degraded, error: error)
        result
      end

      def get(feature)
        result = @primary.get(feature)
        record_get(feature, result, source: :primary, status: status_for(result))
        result
      rescue *@errors => error
        result = @secondary.get(feature)
        record_get(feature, result, source: :secondary, status: :degraded, error: error)
        result
      end

      def get_multi(features)
        result = @primary.get_multi(features)
        record_multi(features, result, source: :primary)
        result
      rescue *@errors => error
        result = @secondary.get_multi(features)
        record_multi(features, result, source: :secondary, error: error)
        result
      end

      def get_all(**kwargs)
        result = @primary.get_all(**kwargs)
        record_all(result, source: :primary)
        result
      rescue *@errors => error
        result = @secondary.get_all(**kwargs)
        record_all(result, source: :secondary, error: error)
        result
      end

      def sync_version
        result = @primary.sync_version
        record_sync_version(result, source: :primary, status: :ok)
        result
      rescue *@errors => error
        result = @secondary.sync_version
        record_sync_version(result, source: :secondary, status: :degraded, error: error)
        result
      end

      def sync_version=(version)
        @primary.sync_version = version
        @secondary.sync_version = version if @dual_write
      end

      def add(feature)
        @primary.add(feature).tap do
          @secondary.add(feature) if @dual_write
        end
      end

      def remove(feature)
        @primary.remove(feature).tap do
          @secondary.remove(feature) if @dual_write
        end
      end

      def clear(feature)
        @primary.clear(feature).tap do
          @secondary.clear(feature) if @dual_write
        end
      end

      def enable(feature, gate, thing)
        @primary.enable(feature, gate, thing).tap do
          @secondary.enable(feature, gate, thing) if @dual_write
        end
      end

      def disable(feature, gate, thing)
        @primary.disable(feature, gate, thing).tap do
          @secondary.disable(feature, gate, thing) if @dual_write
        end
      end

      private

      # Internal: A healthy primary that returns the default config is an
      # explicit "feature has no gate values" answer, not a hole that a stale
      # secondary value is allowed to fill. Mark it :missing so the receipt
      # can tell that apart from a stored value.
      def status_for(result)
        result == default_config ? :missing : :ok
      end

      def record_get(feature, result, source:, status:, error: nil)
        return unless @read_receipt
        @read_receipt.record(
          feature.key,
          source: source,
          status: status,
          operation: :get,
          values: { feature.key => result },
          error: error,
        )
        @read_receipt.record_failure(:get, [feature.key], error) if error
      end

      def record_multi(features, result, source:, error: nil)
        return unless @read_receipt
        keys = features.map(&:key)
        if error
          @read_receipt.record(
            keys,
            source: source,
            status: :degraded,
            operation: :get_multi,
            values: result,
            error: error,
          )
          @read_receipt.record_failure(:get_multi, keys, error)
        else
          keys.each do |key|
            value = result[key]
            @read_receipt.record(
              key,
              source: source,
              status: status_for(value),
              operation: :get_multi,
              values: { key => value },
            )
          end
        end
      end

      def record_all(result, source:, error: nil)
        return unless @read_receipt
        keys = result.keys
        if error
          @read_receipt.record(
            keys,
            source: source,
            status: :degraded,
            operation: :get_all,
            values: result,
            error: error,
          )
          @read_receipt.record_failure(:get_all, keys, error)
        else
          keys.each do |key|
            value = result[key]
            @read_receipt.record(
              key,
              source: source,
              status: status_for(value),
              operation: :get_all,
              values: { key => value },
            )
          end
        end
      end

      def record_features(result, source:, status:, error: nil)
        return unless @read_receipt
        key = ReadReceipt.features_key
        @read_receipt.record(
          key,
          source: source,
          status: status,
          operation: :features,
          values: result,
          error: error,
        )
        @read_receipt.record_failure(:features, [key], error) if error
      end

      def record_sync_version(result, source:, status:, error: nil)
        return unless @read_receipt
        key = ReadReceipt.sync_version_key
        @read_receipt.record(
          key,
          source: source,
          status: status,
          operation: :sync_version,
          values: result,
          error: error,
        )
        @read_receipt.record_failure(:sync_version, [key], error) if error
      end
    end
  end
end
