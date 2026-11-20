# frozen_string_literal: true

module HTTPX
  # Adds support for registering and emitting callbacks on an object (a session, a
  # request, a connection or a response).
  #
  # Registrations last for the object lifetime by default. A registration can instead be
  # scoped to the current execution, by passing <tt>local: true</tt>: it is visible to
  # every internal attempt of that execution (retries, redirects, ...), and is dropped
  # once the execution finishes, with success or error, so that a subsequent execution of
  # the same object (i.e. reusing a Request) doesn't see it anymore.
  #
  # Local registrations are kept in the same ordered list as the regular ones, so that
  # the callback order is preserved regardless of how each one was registered.
  module Callbacks
    def on(type, local: false, &action)
      callbacks(type) << action
      local_callbacks(type) << action if local
      action
    end

    def once(type, local: false, &block)
      on(type, local: local) do |*args, &callback|
        block.call(*args, &callback)
        :delete
      end
    end

    def emit(type, ...)
      log { "emit #{type.inspect} callbacks" } if respond_to?(:log)
      callbacks(type).delete_if { |pr| :delete == pr.call(...) } # rubocop:disable Style/YodaCondition
    end

    def callbacks_for?(type)
      @callbacks && @callbacks.key?(type) && @callbacks[type].any?
    end

    # returns the callbacks registered for +type+ which aren't scoped to the current
    # execution, i.e. the ones which outlive it, and can therefore be carried over to
    # objects derived from this one (i.e. a branched session), preserving their order.
    def persistent_callbacks(type)
      registered = callbacks(type)

      local = @local_callbacks && @local_callbacks[type]

      return registered if local.nil? || local.empty?

      remaining = registered.dup

      local.each do |action|
        idx = remaining.index(action)
        remaining.delete_at(idx) if idx
      end

      remaining
    end

    # whether there are callbacks registered for the current execution only (see
    # #on with <tt>local: true</tt>), for +type+ when given, or for any type otherwise.
    def local_callbacks_for?(type = nil)
      return false unless @local_callbacks

      if type
        @local_callbacks.key?(type) && @local_callbacks[type].any?
      else
        @local_callbacks.any? { |_, actions| !actions.empty? }
      end
    end

    # marks the beginning of an execution of this object, i.e. one request lifecycle,
    # which may encompass several internal attempts (retries, redirects, ...).
    #
    # executions may nest (i.e. when a plugin sends the same request again through a
    # different session internally). Registrations scoped to the execution are only
    # dropped when the outermost execution finishes.
    def start_execution
      @execution_depth = execution_depth + 1
    end

    # marks the end of an execution (see #start_execution). When the outermost execution
    # finishes, every registration scoped to it (see #on with <tt>local: true</tt>) is
    # dropped, whether it finished successfully or not.
    def finish_execution
      depth = execution_depth - 1
      depth = 0 if depth.negative?
      @execution_depth = depth

      clear_local_callbacks if depth.zero?
    end

    def in_execution?
      !execution_depth.zero?
    end

    # drops every registration scoped to the current execution (see #on with <tt>local: true</tt>).
    def clear_local_callbacks
      return unless @local_callbacks

      @local_callbacks.each do |type, actions|
        registered = @callbacks && @callbacks[type]

        next unless registered

        actions.each do |action|
          idx = registered.index(action)
          registered.delete_at(idx) if idx
        end
      end

      @local_callbacks.clear
    end

    protected

    def callbacks(type = nil)
      return @callbacks unless type

      @callbacks ||= Hash.new { |h, k| h[k] = [] }
      @callbacks[type]
    end

    def local_callbacks(type = nil)
      return @local_callbacks unless type

      @local_callbacks ||= Hash.new { |h, k| h[k] = [] }
      @local_callbacks[type]
    end

    private

    def execution_depth
      @execution_depth || 0
    end
  end
end
