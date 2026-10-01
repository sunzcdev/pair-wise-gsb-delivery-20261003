module Interactor
  # Internal: Invoke one or more Interactors as an isolated, recoverable
  # subflow. The subflow shares the caller's Context, so completed steps are
  # tracked in the same rollback list, but a boundary is pushed before the
  # subflow runs.
  #
  # On success the boundary is committed: the completed interactor instances
  # remain in the Context's rollback list and take part in any later outer
  # rollback, in the normal reverse order.
  #
  # On an explicit business failure (the subflow calling "context.fail!") only
  # the steps completed after entering the subflow are rolled back and the
  # boundary is discarded, along with the subflow's top-level Context changes.
  # The failed Context is then handed to the recovery block or method and the
  # caller keeps going. Any other kind of exception is left to propagate.
  #
  # Only top-level Context key additions, removals and reassignments are
  # undone. Mutations performed inside the values themselves are not tracked.
  class Attempt
    # Internal: Initialize an Attempt.
    #
    # interactors  - The Interactor classes that make up the subflow.
    # recovery     - An optional Symbol naming a host method invoked with the
    #                failed Context when the subflow calls "fail!".
    # recovery_blk - An optional block evaluated in the host with the failed
    #                Context when the subflow calls "fail!".
    def initialize(interactors, recovery: nil, &recovery_blk)
      @interactors = interactors.flatten
      @recovery = recovery
      @recovery_blk = recovery_blk
    end

    # Internal: Run the subflow against the given Context. On success the
    # completed steps stay tracked in the Context and the Context is returned.
    # When the subflow calls "fail!", a separate Context describing the failure
    # is returned after recovery. Ordinary exceptions propagate outward.
    def run(context, host)
      context._enter_subflow
      @interactors.each { |interactor| interactor.call!(context) }
      context._commit_subflow
      context
    rescue Failure => e
      raise unless e.context.equal?(context)

      failure_context = context._abort_subflow
      recover(host, failure_context)
      failure_context
    rescue StandardError
      context._dissolve_subflow
      raise
    end

    private

    # Internal: Hand the failed Context to the configured recovery method or
    # block. The block is evaluated in the host's context so that the live
    # outer Context stays accessible while recording the reason. A failure or
    # error raised while recovering is allowed to propagate.
    def recover(host, failure_context)
      if @recovery
        host.send(@recovery, failure_context)
      elsif @recovery_blk
        host.instance_exec(failure_context, &@recovery_blk)
      end
    end
  end
end
