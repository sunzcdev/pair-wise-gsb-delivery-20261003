# frozen_string_literal: true

require "monitor"

module Rake

  # Raised when a task declares a resource requirement that can never be
  # satisfied: an unknown resource, a non-positive amount, or an amount
  # larger than the declared capacity of the resource.
  class InvalidResourceRequirement < ArgumentError; end

  ##
  # The ResourceManager hands out quotas for named, finite build
  # resources (such as compilers or linkers) to tasks whose
  # prerequisites are ready.
  #
  # Acquisition is atomic: a caller either holds its entire declared
  # requirement or nothing at all.  A caller never waits while holding a
  # partial allocation, so tasks cannot deadlock by holding half of
  # their resources while waiting for the rest.
  #
  # Admission is strictly first-come-first-served: a request is granted
  # only when it is the oldest pending request and every quota it needs
  # is available.  Later requests never overtake an earlier one.
  # Because every accepted request individually fits within the declared
  # capacity (see #validate!), a queued request always becomes
  # satisfiable once the requests ahead of it have been granted and
  # released, so no request can starve.
  #
  # The manager never creates threads; callers wait on their own
  # (worker) thread, so the number of worker threads stays within the
  # parallelism configured for the thread pool.
  class ResourceManager

    # Create an empty manager.  Resources are added with #declare.
    def initialize
      @limits = {}
      @available = {}
      @monitor = Monitor.new
      @changed = @monitor.new_cond
      @waiters = []
    end

    # Declare a resource +name+ with a total +capacity+ of quota.
    # The capacity must be a positive Integer.
    def declare(name, capacity)
      name = name.to_sym
      unless capacity.is_a?(Integer) && capacity > 0
        raise ArgumentError,
          "capacity for resource #{name.inspect} must be a positive Integer"
      end
      @monitor.synchronize do
        @limits[name] = capacity
        @available[name] = capacity
      end
      self
    end

    # The declared capacity of +name+, or nil if it is undeclared.
    def limit(name)
      @limits[name.to_sym]
    end

    # The quota of +name+ currently available.  Mainly for testing.
    def available(name)
      @available[name.to_sym]
    end

    # Normalize a requirements hash: nil becomes empty and resource
    # names become symbols.  Raises InvalidResourceRequirement if
    # +requirements+ is not hash-like.
    def self.normalize(requirements)
      return {} if requirements.nil?
      unless requirements.respond_to?(:to_hash)
        raise InvalidResourceRequirement,
          "resource requirements must be a Hash, got #{requirements.inspect}"
      end
      requirements.to_hash.each_with_object({}) do |(name, amount), result|
        unless name.respond_to?(:to_sym)
          raise InvalidResourceRequirement,
            "resource name must be a Symbol or String, got #{name.inspect}"
        end
        result[name.to_sym] = amount
      end
    end

    # Raise InvalidResourceRequirement if any requirement is illegal:
    # an unknown resource, a non-positive amount, or an amount exceeding
    # the declared capacity.  Returns the normalized requirements.
    def validate!(requirements)
      requirements = ResourceManager.normalize(requirements)
      requirements.each do |name, amount|
        capacity = @limits[name]
        if capacity.nil?
          raise InvalidResourceRequirement,
            "unknown resource #{name.inspect} (no capacity declared)"
        end
        unless amount.is_a?(Integer) && amount > 0
          raise InvalidResourceRequirement,
            "requirement for #{name.inspect} must be a positive Integer, " \
            "got #{amount.inspect}"
        end
        if amount > capacity
          raise InvalidResourceRequirement,
            "requirement #{name.inspect}: #{amount} exceeds declared " \
            "capacity #{capacity}"
        end
      end
      requirements
    end

    # Atomically acquire all quotas in +requirements+.  Blocks (on the
    # caller's own thread) until the request is the oldest pending
    # request and every required quota is available.  Illegal
    # requirements are rejected before any waiting happens.
    def acquire(requirements)
      requirements = validate!(requirements)
      return requirements if requirements.empty?

      @monitor.synchronize do
        request = requirements
        @waiters << request
        begin
          @changed.wait until
            @waiters.first.equal?(request) && allocatable?(request)
          request.each { |name, amount| @available[name] -= amount }
        ensure
          @waiters.delete_if { |waiter| waiter.equal?(request) }
          @changed.broadcast
        end
      end
      requirements
    end

    # Return all quotas in +requirements+.  Safe to call with any
    # quotas previously granted by #acquire.
    def release(requirements)
      requirements = ResourceManager.normalize(requirements)
      return if requirements.empty?
      @monitor.synchronize do
        requirements.each do |name, amount|
          @available[name] += amount if @limits.key?(name)
        end
        @changed.broadcast
      end
      nil
    end

    # Acquire +requirements+, yield, and return the quotas even if the
    # block raises or the calling thread is killed.
    def with_resources(requirements)
      granted = acquire(requirements)
      begin
        yield
      ensure
        release(granted)
      end
    end

    private

    def allocatable?(request) # :nodoc:
      request.all? { |name, amount| @available[name] >= amount }
    end
  end
end
