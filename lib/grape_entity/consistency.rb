# frozen_string_literal: true

module Grape
  class Entity
    # Raised when a representation cannot be proven to come from a single,
    # jointly-valid set of representation versions. The half-assembled Hash is
    # never delivered; the caller must retry at the application boundary or
    # accept that the object moved while it was being rendered.
    class UnstableVersion < StandardError
      attr_reader :object, :coordinator

      def initialize(message = nil, object: nil, coordinator: nil)
        @object = object
        @coordinator = coordinator
        super(message || default_message)
      end

      private

      def default_message
        target = begin
                   object.class.name
                 rescue StandardError
                   object.class.to_s
                 end
        "representation of #{target} mixes values from different versions"
      end
    end

    # Internal: a captured representation token (version + in-progress flag)
    # changed between capture and verification, or an object was captured
    # while a mutation was in progress. Retriable, but only while every field
    # read on the failed pass permits re-reading.
    class VersionConflict < UnstableVersion; end

    # Internal: a retry would have to invoke a getter that was not declared
    # +reread: true+. Not retriable.
    class RereadForbidden < UnstableVersion; end

    # ConsistencyCoordinator supervises one +represent_consistent+ call.
    #
    # It records, per attempt:
    # * every versioned object or collection that participates in the output,
    #   capturing its representation token the first time it is touched;
    # * every field read that produced the output, noting whether its exposure
    #   was declared +reread: true+.
    #
    # After a pass completes it verifies that every captured token is still
    # jointly valid. A failed pass is retried only if all of its reads are
    # allowed to be re-read; ordinary exceptions never reach the retry loop.
    class ConsistencyCoordinator
      attr_reader :attempts, :reads

      def initialize
        @attempts = 0
        @tokens = {}
        @reads = []
      end

      # Objects participate in the version protocol only when they answer the
      # protocol methods.
      def versioned?(object)
        object.respond_to?(:representation_version) &&
          object.respond_to?(:representation_mutating?)
      end

      # Capture an object or association collection the first time it
      # participates in the current pass.
      def register(object, path = nil)
        return unless versioned?(object)
        return if @tokens.key?(object.object_id)

        token = capture(object)
        unless stable?(object, token)
          raise VersionConflict.new(
            "captured #{label(object, path)} while a representation change was in progress",
            object: object, coordinator: self
          )
        end
        @tokens[object.object_id] = { object: object, token: token, path: path }
      end

      # Record one getter invocation that feeds the current pass.
      def read(object, field, can_reread, path = nil)
        return unless versioned?(object)

        register(object, path)
        @reads << { object_id: object.object_id, field: field, reread: can_reread, path: path }
      end

      # Start a (possibly repeated) pass. When a previous pass failed with a
      # version conflict, every getter it invoked must have been declared
      # re-readable; otherwise the spliced output cannot be rebuilt and the
      # result is rejected without touching any getter a second time.
      def begin_attempt
        if @attempts.positive?
          forbidden = @reads.find { |record| !record[:reread] }
          if forbidden
            raise RereadForbidden.new(
              "field #{field_label(forbidden)} cannot be re-read: declare reread: true " \
              'on this exposure to allow a bounded retry after a version conflict',
              object: object_for(forbidden), coordinator: self
            )
          end
        end

        @attempts += 1
        @tokens = {}
        @reads = []
      end

      # Assert that all captured tokens still describe one jointly-valid view.
      def verify!
        @tokens.each_value do |entry|
          object = entry[:object]
          current = capture(object)
          next if current == entry[:token]

          raise VersionConflict.new(
            "#{label(object, entry[:path])} changed while it was being represented",
            object: object, coordinator: self
          )
        end
        true
      end

      private

      def capture(object)
        [object.representation_version, object.representation_mutating?]
      end

      def stable?(object, token)
        token == capture(object) && token.last == false
      end

      def object_for(record)
        entry = @tokens[record[:object_id]]
        entry && entry[:object]
      end

      def field_label(record)
        (record[:path] || []).dup.push(record[:field]).map(&:to_s).join('.')
      end

      def label(object, path)
        name = object.class.name || object.class.to_s
        path_suffix = path && path.any? ? " at #{path.map(&:to_s).join('.')}" : ''
        "#{name}#{path_suffix}"
      end
    end

    # Versioned is a reference implementation of the representation version
    # protocol. Include it in the objects (and use VersionedArray for
    # association collections) that back your entities.
    #
    # The contract for the application is:
    # * every observable change to the object's represented state MUST run
    #   through +representation_updating+ (or at least call
    #   +representation_changed!+), which advances the version;
    # * a multi-step update leaves +representation_mutating?+ true for its full
    #   duration, so readers never mistake an in-progress state for stable.
    module Versioned
      # Set up the protocol state. Call from the including class's
      # +initialize+ (before or after its own state is prepared).
      def representation_initialize
        @representation_mutex = Mutex.new
        @representation_version = 0
        @representation_updating = false
      end

      # Monotonic version of the represented state. Advanced exactly once per
      # committed change performed through +representation_updating+.
      def representation_version
        @representation_mutex.synchronize { @representation_version }
      end

      # Truthy while a change that observers must never see is in progress.
      def representation_mutating?
        @representation_mutex.synchronize { @representation_updating }
      end

      def representation_token
        [representation_version, representation_mutating?]
      end

      # Advance the representation version directly. Prefer
      # +representation_updating+ for multi-step changes so the in-progress
      # window is visible to readers as well.
      def representation_changed!
        @representation_mutex.synchronize { @representation_version += 1 }
      end

      # Wrap every application update that changes represented state. The
      # version is advanced when the block finishes and +representation_mutating?
      # is true for its whole duration (even if the block raises).
      def representation_updating
        @representation_mutex.synchronize { @representation_updating = true }
        yield
      ensure
        @representation_mutex.synchronize do
          @representation_updating = false
          @representation_version += 1
        end
      end
    end

    # An Array whose own representation version advances whenever its
    # membership or order observably changes. Use it for association
    # collections that are exposed with +using:+.
    class VersionedArray < Array
      include Versioned

      def initialize(*args, &block)
        super
        representation_initialize
      end

      MUTATORS = %i[
        << append push prepend concat
        pop shift delete delete_at delete_if reject!
        select! keep_if slice! clear fill replace
        collect! map! flatten! compact! uniq! sort! sort_by!
        reverse! rotate!
      ].freeze

      MUTATORS.each do |method_name|
        define_method(method_name) do |*args, **kwargs, &block|
          result = super(*args, **kwargs, &block)
          representation_changed!
          result
        end
      end
    end
  end
end
