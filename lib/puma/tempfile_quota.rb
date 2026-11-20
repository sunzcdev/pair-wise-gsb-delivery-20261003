# frozen_string_literal: true

module Puma

  #———————————————————————— DO NOT USE — this class is for internal use only ———


  # Tracks the aggregate number of bytes held in request body tempfiles
  # across all clients of a `Puma::Server`.  A single large request may be
  # within the per-request limit, but many concurrent large requests can
  # still fill the temporary directory.  This class enforces a configurable,
  # server-wide disk quota for pending request bodies.
  #
  # A `Puma::Client` reserves bytes when its body is moved from memory to a
  # tempfile, may charge more as a chunked body grows, and releases its
  # reservation when the tempfile is closed.  Each reservation records its
  # +owner+ (the client) and its current +holder+ (the client while reading,
  # the Rack env once handed off via +rack.input+), so it is always possible
  # to verify who is holding the disk space.
  #
  # A +nil+ limit means the quota is unlimited; reservations are still
  # tracked so holders can be inspected.
  #
  class TempfileQuota # :nodoc:

    # A single client's claim on the aggregate quota.
    class Reservation
      attr_accessor :bytes, :holder
      attr_reader :owner

      def initialize(owner, bytes)
        @owner  = owner
        @holder = owner
        @bytes  = bytes
      end
    end

    attr_reader :limit, :used

    # @param limit [Integer, nil] maximum aggregate bytes held in request
    #   body tempfiles, +nil+ for unlimited
    def initialize(limit = nil)
      @limit = limit
      @used  = 0
      @reservations = {}.compare_by_identity
      @mutex = Mutex.new
    end

    # Attempt to reserve +bytes+ for +owner+.  Returns a +Reservation+
    # on success, +nil+ if the reservation would exceed the quota.
    def reserve(bytes, owner)
      @mutex.synchronize do
        return nil if over_limit?(bytes)

        reservation = Reservation.new(owner, bytes)
        @reservations[reservation] = reservation
        @used += bytes
        reservation
      end
    end

    # Grow an existing reservation by +additional+ bytes (chunked bodies
    # grow as they are read).  Returns false if the quota would be
    # exceeded, in which case nothing is charged.
    def charge(reservation, additional)
      return true if additional <= 0

      @mutex.synchronize do
        return false if over_limit?(additional)

        reservation.bytes += additional
        @used += additional
        true
      end
    end

    # Release a reservation, making its bytes available for reuse.
    def release(reservation)
      return unless reservation

      @mutex.synchronize do
        if (released = @reservations.delete(reservation))
          @used -= released.bytes
        end
      end
      nil
    end

    # Record that the tempfile backing +reservation+ has been handed off,
    # e.g. to the Rack app via +rack.input+.
    def hand_off(reservation, holder)
      @mutex.synchronize { reservation.holder = holder }
    end

    # Snapshot of current holders for verification.  Each entry is
    # +[owner, holder, bytes]+.
    def holders
      @mutex.synchronize do
        @reservations.each_value.map { |r| [r.owner, r.holder, r.bytes] }
      end
    end

    private

    def over_limit?(additional)
      @limit && @used + additional > @limit
    end
  end
end
