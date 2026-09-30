# frozen_string_literal: true

module Grape
  class Entity
    # Consistency support for representations that must never splice a
    # half-old / half-new Hash together while a background thread mutates
    # the represented object graph.
    #
    # The application participates by giving every represented object (and
    # every versioned association collection) a *version protocol*:
    #
    #   * it responds to +representation_version+;
    #   * while the object is stable the method returns the same opaque token
    #     on every call - a +nil+ return means *a change is in progress* and
    #     is itself an unstable result;
    #   * the token MUST be advanced (and the in-progress state exposed)
    #     whenever something that affects the representation changes, namely:
    #
    #       1. an exposed attribute value changes;
    #       2. an association is replaced by another object or the contents
    #          exposed through it change;
    #       3. collection membership changes (an element is added, removed,
    #          replaced or reordered);
    #       4. the object leaves or re-enters a stable state
    #          (+begin_change!+ / +end_change!+).
    #
    # {Grape::Entity.represent_consistently} records every version before the
    # corresponding getters run and jointly verifies them once the whole Hash
    # has been assembled. Any mismatch rejects the entire response with an
    # {InconsistentVersionError}: an already built Hash is never delivered.
    # Only exposures explicitly declared +reread: true+ make a conflict
    # eligible for a bounded number of retries; ordinary exceptions are not
    # treated as version contention.
    module Consistency
      # Raised when an output cannot be attributed to a set of versions that
      # could all hold at the same time.
      class InconsistentVersionError < StandardError
        # @return [Object] the participant whose observed version changed
        attr_reader :participant

        # @return [Object, nil] version captured before the getters ran
        attr_reader :expected_version

        # @return [Object, nil] version observed when the change was detected
        attr_reader :actual_version

        def initialize(message = nil, participant: nil, expected_version: nil, actual_version: nil, retriable: false)
          @participant = participant
          @expected_version = expected_version
          @actual_version = actual_version
          @retriable = retriable
          super(message || default_message)
        end

        # @return [Boolean] whether every getter involved in the conflict was
        #   declared +reread: true+, so the entry point may try again
        def retriable?
          @retriable
        end

        private

        def default_message
          reason = actual_version.nil? ? 'a change is in progress' : 'its version changed mid-representation'
          "representation of #{participant.inspect} was rejected because #{reason}; " \
            'refusing to return a half-old half-new Hash'
        end
      end

      # Tracks the versions of every object and versioned collection touched
      # by one representation attempt and decides whether the assembled Hash
      # may be delivered.
      #
      # A session lives for the whole (possibly retried) representation; it
      # is {#reset!} before each attempt so stale participants never leak
      # between readings.
      class Session
        # Number of times the entry point re-ran the representation because a
        # declared-reread participant moved. Zero for a one-shot success.
        attr_accessor :reread_count

        def initialize
          @records = {}
          @reread_count = 0
        end

        # Forget every captured version. Called before each attempt. The
        # running reread tally (owned by the entry point) is intentionally
        # kept so the bounded-retry budget can be exhausted.
        def reset!
          @records.clear
        end

        # Capture +object+'s version before any of its getters run.
        #
        # Objects that do not implement the version protocol are opt out and
        # are simply not protected. A +nil+ version (change in progress) is a
        # conflict before the first read; it is only retriable when the
        # association exposure itself was declared +reread+.
        #
        # @param reread [Boolean, nil] +true+/+false+ when an association
        #   exposure registers the object it read; +nil+ for the entity whose
        #   exposures are being mapped.
        def register(object, reread: nil)
          return unless trackable?(object)

          if (record = @records[object])
            record[:read_flags] << reread unless reread.nil?
            return
          end

          version = object.representation_version
          record = {
            version: version,
            read_flags: reread.nil? ? [] : [reread],
            starting_nil: version.nil?
          }
          @records[object] = record

          # A root entity (no getter voted yet) defers a nil starting version
          # to its getters / the joint commit. At an association resolution
          # point the vote is already in, so reject immediately.
          return unless version.nil? && !reread.nil?

          raise_conflict(object, nil, nil, retriable: reread == true)
        end

        # Invoke one exposure while remembering whether it was declared
        # reread-safe. Nesting exposures themselves read nothing, so they do
        # not vote.
        def around_read(object, exposure)
          if (record = @records[object])
            unless exposure.nesting?
              record[:read_flags] << exposure.is_reread
              raise_starting_nil(object, record) if record[:starting_nil]
            end
          end
          yield
        end

        # Jointly re-check every captured version. The output is deliverable
        # only if all participants still stand on the versions they started
        # with. A mismatch is retriable only when every read involved was
        # declared +reread: true+.
        def commit!
          @records.each do |object, record|
            if record[:starting_nil]
              raise_starting_nil(object, record)
            end

            current = object.representation_version
            next if !current.nil? && current == record[:version]

            raise_conflict(object, record[:version], current, retriable: retriable?(record))
          end
        end

        private

        def raise_starting_nil(object, record)
          raise_conflict(object, nil, nil, retriable: retriable?(record))
        end

        # Every getter that touched the participant must be declared reread,
        # and at least one getter must have run.
        def retriable?(record)
          flags = record[:read_flags]
          flags.any? && flags.all?
        end

        def trackable?(object)
          !object.nil? && object.respond_to?(:representation_version)
        end

        def raise_conflict(object, expected, actual, retriable:)
          raise InconsistentVersionError.new(
            participant: object,
            expected_version: expected,
            actual_version: actual,
            retriable: retriable
          )
        end
      end
    end

    # A minimal version protocol the application can mix into its models and
    # collection wrappers. Applications remain responsible for bracketing
    # every relevant mutation; the mixin only maintains the token.
    #
    # @example
    #
    #   class Account
    #     include Grape::Entity::VersionProtocol
    #
    #     attr_accessor :total
    #
    #     def deposit(amount)
    #       changing { self.total += amount }
    #     end
    #   end
    #
    #   account.representation_version   #=> 0 (stable)
    #   account.deposit(10)
    #   account.representation_version   #=> 1 (stable, new token)
    module VersionProtocol
      # @return [Integer, nil] a stable, monotonically advancing token, or
      #   +nil+ while a change is in progress.
      def representation_version
        return nil if @representation_change_in_progress

        @representation_stable_version ||= 0
      end

      # Mark the object as changing: {#representation_version} returns +nil+
      # until {#end_change!} is called.
      def begin_change!
        @representation_change_in_progress = true
        self
      end

      # Finish a change and advance the stable version token.
      def end_change!
        @representation_stable_version = (@representation_stable_version || 0).next
        @representation_change_in_progress = false
        self
      end

      # Run a block between {#begin_change!} and {#end_change!}, advancing the
      # version exactly once even when the block raises.
      def changing
        begin_change!
        yield
        self
      ensure
        end_change!
      end
    end
  end
end
