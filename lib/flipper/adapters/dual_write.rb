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

      # Public: Flip a group of flags on both backends together. The two
      # backends are built from the same committed state and receive the exact
      # same buffered writes, so a release never leaves them pointing at
      # different backends. The validator runs against the combined final state
      # before either backend is touched; an illegal combination raises and
      # leaves both backends on their previous gating. Reads through this
      # adapter keep serving the committed local state until the batch commits
      # (the read barrier).
      def transaction(validate: nil)
        local_batch = Adapters::Batch.from(@local)
        remote_batch = Adapters::Batch.from(@remote)
        batch = CompositeBatch.new(local_batch, remote_batch)

        result = yield batch
        validate&.call(batch)

        @remote.import(remote_batch)
        @local.import(local_batch)
        result
      end

      # Internal: Routes every buffered write to an identical batch on each
      # backend while presenting the local backend's read view, matching how
      # DualWrite serves reads from local outside of a transaction.
      class CompositeBatch
        include ::Flipper::Adapter

        attr_reader :local, :remote

        def initialize(local, remote)
          @local = local
          @remote = remote
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

        def read_only?
          false
        end

        def add(feature)
          @remote.add(feature)
          @local.add(feature)
        end

        def remove(feature)
          @remote.remove(feature)
          @local.remove(feature)
        end

        def clear(feature)
          @remote.clear(feature)
          @local.clear(feature)
        end

        def enable(feature, gate, thing)
          @remote.enable(feature, gate, thing)
          @local.enable(feature, gate, thing)
        end

        def disable(feature, gate, thing)
          @remote.disable(feature, gate, thing)
          @local.disable(feature, gate, thing)
        end
      end
    end
  end
end
