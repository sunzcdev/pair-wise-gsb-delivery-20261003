# frozen_string_literal: true
module Rake

  ##
  # InvocationIdentity is the canonical, structural identity of a single
  # task invocation: the fully scoped task name plus the argument values
  # bound to the task's declared formal parameters (defaults already
  # applied) plus any extra positional arguments.
  #
  # Two invocations refer to the same build "instance" of a task if and
  # only if their identities are equal.  Equality and hashing are
  # structural -- they are computed from the task name, the ordered
  # name/value bindings and the extras themselves, never from a formatted
  # string such as Array#to_s -- so two different argument lists whose
  # stringification happens to collide still produce distinct identities.
  #
  class InvocationIdentity
    # Fully qualified (namespace scoped) name of the invoked task.
    attr_reader :task_name

    # Ordered hash of formal parameter name (Symbol) => bound value.
    # Every declared formal parameter appears, whether or not a value was
    # supplied for it (unsupplied parameters bind to nil).
    attr_reader :bindings

    # List of extra positional arguments that are not bound to any
    # declared formal parameter.
    attr_reader :extras

    def initialize(task_name, bindings, extras)
      @task_name = task_name.to_s
      @bindings  = bindings.freeze
      @extras    = extras.freeze
    end

    # Build the identity of +task+ invoked with +task_args+ (a
    # Rake::TaskArguments).  Values are bound to the task's real formal
    # parameters; declared defaults should already have been applied to
    # +task_args+ so that an explicit value and a defaulted value produce
    # the same identity.
    def self.for_task(task, task_args)
      bindings = {}
      task.arg_names.each do |name|
        bindings[name.to_sym] = task_args[name]
      end
      new(task.name, bindings, task_args.extras)
    end

    # Structural equality: same task, same formal parameter bindings and
    # same extras.
    def ==(other)
      other.is_a?(InvocationIdentity) &&
        task_name == other.task_name &&
        bindings  == other.bindings &&
        extras    == other.extras
    end
    alias eql? ==

    def hash
      [self.class, task_name, bindings, extras].hash
    end

    # Human readable rendering used by --trace and investigation output.
    # This is only a display form; identity is never derived from it.
    def to_s
      return task_name if bindings.empty? && extras.empty?
      parts = bindings.map { |name, value| "#{name}: #{value.inspect}" }
      parts.concat(extras.map { |value| "*#{value.inspect}" })
      "#{task_name}[#{parts.join(', ')}]"
    end
    alias inspect to_s
  end
end
