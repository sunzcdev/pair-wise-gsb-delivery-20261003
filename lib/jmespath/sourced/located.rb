 # frozen_string_literal: true
module JMESPath
  # @api private
  #
  # An internal wrapper around a value that carries provenance through the
  # evaluator. A Located is either:
  #
  # * verbatim - `pointer` is a JSON Pointer into the original document and
  #   `children` is nil (the container is traversed lazily),
  # * derived - `pointer` is nil and `inputs` lists the direct input
  #   pointers; `children` holds explicit Located entries for synthetic
  #   containers produced by the expression,
  # * missing - the expression resolves to nothing (JMESPath `null`).
  class Located
    attr_accessor :raw, :pointer, :inputs, :children, :missing

    def self.sourced(raw, pointer)
      allocate.tap do |located|
        located.raw = raw
        located.pointer = pointer
        located.inputs = nil
        located.children = nil
        located.missing = false
      end
    end

    def self.derived(raw, inputs = [], children = nil)
      allocate.tap do |located|
        located.raw = raw
        located.pointer = nil
        located.inputs = inputs
        located.children = children
        located.missing = false
      end
    end

    MISSING = allocate.tap do |located|
      located.raw = nil
      located.pointer = nil
      located.inputs = nil
      located.children = nil
      located.missing = true
      located.freeze
    end

    def missing?
      @missing
    end

    def sourced?
      !@missing && !@pointer.nil?
    end

    def derived?
      !@missing && @pointer.nil?
    end

    def synthetic?
      !@children.nil?
    end

    def root_inputs
      if sourced?
        [@pointer]
      elsif missing?
        []
      else
        @inputs || []
      end
    end
  end
end
