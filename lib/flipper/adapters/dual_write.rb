module Flipper
  module Adapters
    class DualWrite
      include ::Flipper::Adapter

      attr_reader :local, :remote

      # Public: Build a new sync instance.
      #
      # local - The local flipper adapter that should serve reads.
      # remote - The remote flipper adapter that writes should go to first (in
      #          addition to the local adapter).
      def initialize(local, remote, options = {})
        @local = local
        @remote = remote
      end

      def adapter_stack
        "#{name}(local: #{@local.adapter_stack}, remote: #{@remote.adapter_stack})"
      end

      def read_only?
        @remote.read_only? || @local.read_only?
      end

      def features
        @local.features
      end

      def get(feature)
        @local.get(feature)
      end

      def get_multi(features)
        @local.get_multi(features)
      end

      def get_all(**kwargs)
        @local.get_all(**kwargs)
      end

      def sync_version
        @local.sync_version
      end

      def sync_version=(version)
        @local.sync_version = version
      end

      def add(feature)
        @remote.add(feature).tap { @local.add(feature) }
      end

      def remove(feature)
        @remote.remove(feature).tap { @local.remove(feature) }
      end

      def clear(feature)
        @remote.clear(feature).tap { @local.clear(feature) }
      end

      def enable(feature, gate, thing)
        @remote.enable(feature, gate, thing).tap do
          @local.enable(feature, gate, thing)
        end
      end

      def disable(feature, gate, thing)
        @remote.disable(feature, gate, thing).tap do
          @local.disable(feature, gate, thing)
        end
      end

      # Public: Replaces one member of a set gate with another.
      #
      # The remote decides the change (including capacity/conflict errors) as
      # it does for the other writes, then the local copy is mirrored without
      # the caller's expectation: a local copy that lagged behind must not turn
      # a successful remote change into a failure.
      def replace(feature, gate, old_thing, new_thing, expected: nil)
        @remote.replace(feature, gate, old_thing, new_thing, expected: expected).tap do
          begin
            @local.replace(feature, gate, old_thing, new_thing, expected: nil)
          rescue ActorSetConflict
            # Next sync reconciles the local copy.
          end
        end
      end
    end
  end
end
