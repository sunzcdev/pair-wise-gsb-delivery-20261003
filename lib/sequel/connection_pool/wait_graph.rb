# frozen-string-literal: true

# A wait-for graph used by connection pools to detect deadlocks between
# execution units (threads/fibers, as returned by Sequel.current) that are
# waiting for connections held by each other.
#
# The graph tracks two types of edges, keyed by owner (Sequel.current):
#
# hold edges :: owner currently holds a connection for a resource
#               (a [pool, server] pair).
# wait edge  :: owner is currently blocked waiting for a resource.
#
# When adding a wait edge would close a cycle (the owners holding the
# resource are themselves waiting, directly or indirectly, on a resource
# held by the waiting owner), the cycle is returned so the pool can abort
# the acquisition that would complete the deadlock instead of waiting
# until the pool timeout.
#
# This class is not used unless the :pool_wait_graph connection pool
# option is enabled.
class Sequel::ConnectionWaitGraph
  def initialize
    @mutex = Mutex.new
    # owner => {resource => count}
    @holds = {}.compare_by_identity
    # resource => {owner => count}
    @holders = {}
    # owner => resource
    @waiting = {}.compare_by_identity
  end

  # Record that the owner now holds a connection for the resource.
  def hold(owner, resource)
    @mutex.synchronize do
      increment(@holds[owner] ||= {}, resource)
      increment(@holders[resource] ||= {}.compare_by_identity, owner)
    end
    nil
  end

  # Record that the owner no longer holds a connection for the resource.
  def release(owner, resource)
    @mutex.synchronize do
      if (resources = @holds[owner]) && decrement(resources, resource)
        @holds.delete(owner) if resources.empty?
        if owners = @holders[resource]
          decrement(owners, owner)
          @holders.delete(resource) if owners.empty?
        end
      end
    end
    nil
  end

  # Record that the owner is about to block waiting for the resource.
  # If the new wait edge completes a cycle, return the cycle as an
  # array alternating owners and resources, starting and ending with
  # the given owner, e.g. [owner, resource, other_owner, resource, owner].
  # Returns nil if no cycle is created.
  def wait(owner, resource)
    @mutex.synchronize do
      @waiting[owner] = resource
      detect_cycle(owner, resource)
    end
  end

  # Record that the owner is no longer waiting for a resource, because
  # it acquired a connection, timed out, or an exception was raised.
  def stop_waiting(owner)
    @mutex.synchronize{@waiting.delete(owner)}
    nil
  end

  # The resource the owner is currently waiting for, or nil if the owner
  # is not waiting.  Mostly useful for diagnostics and testing.
  def waiting_on(owner)
    @mutex.synchronize{@waiting[owner]}
  end

  # Whether the graph has no hold or wait edges.  Mostly useful for
  # diagnostics and testing.
  def empty?
    @mutex.synchronize{@holds.empty? && @waiting.empty?}
  end

  private

  def increment(hash, key)
    hash[key] = (hash[key] || 0) + 1
  end

  # Decrement the count for the key, removing it when it reaches zero.
  # Returns false if the key was not present.
  def decrement(hash, key)
    return false unless count = hash[key]

    if count <= 1
      hash.delete(key)
    else
      hash[key] = count - 1
    end
    true
  end

  # Look for a path from the owners holding the resource back to the
  # owner, following wait edges.  The calling code must already hold
  # the mutex.
  def detect_cycle(owner, resource)
    # Each stack entry is a [resource, chain] pair, where chain is the
    # alternating owner/resource path from owner to the resource.
    stack = [[resource, [owner, resource]]]
    visited = {}.compare_by_identity

    until stack.empty?
      res, chain = stack.pop
      next unless owners = @holders[res]

      owners.each_key do |holder|
        return chain << owner if holder == owner
        next if visited[holder]

        visited[holder] = true
        if next_res = @waiting[holder]
          stack << [next_res, chain + [holder, next_res]]
        end
      end
    end

    nil
  end
end

module Sequel
  # The global connection wait graph, shared by all connection pools
  # that enable the :pool_wait_graph option, so that wait cycles
  # spanning multiple pools can be detected.
  def self.connection_wait_graph
    @connection_wait_graph ||= ConnectionWaitGraph.new
  end
end
