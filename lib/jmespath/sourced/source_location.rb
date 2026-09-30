 # frozen_string_literal: true
module JMESPath
  # Describes where a single value in a {SourcedResult} comes from in the
  # original document.
  #
  # A value copied verbatim from the document has a {#pointer} (a
  # {https://www.rfc-editor.org/rfc/rfc6901 JSON Pointer}) and an empty
  # {#inputs} list. A value computed by the expression (results of
  # functions, comparisons, literals, multi-selects, projections, ...) is
  # flagged with {#derived?} and {#inputs} lists the JSON Pointers of its
  # direct inputs. A projection position that does not exist in the result
  # is represented by {SourceLocation.missing}.
  class SourceLocation
    # @return [String, nil] JSON Pointer into the original document, or
    #   `nil` when the value is derived.
    attr_reader :pointer

    # @return [Array<String>] JSON Pointers of the direct inputs that were
    #   used to compute a derived value. Empty for verbatim values.
    attr_reader :inputs

    def initialize(pointer, derived, inputs)
      @pointer = pointer
      @derived = derived
      @inputs = inputs
    end

    # @api private
    def self.sourced(pointer)
      new(pointer, false, EMPTY_INPUTS)
    end

    # @api private
    def self.derived(inputs = EMPTY_INPUTS)
      new(nil, true, inputs.dup)
    end

    # Shared entry for values the expression could not resolve.
    # @return [SourceLocation]
    def self.missing
      MISSING
    end

    # @return [Boolean] `true` when the value is copied verbatim from the
    #   original document.
    def sourced?
      !@derived && !missing?
    end

    # @return [Boolean] `true` when the value was computed by the
    #   expression rather than copied from the document.
    def derived?
      @derived
    end

    # @return [Boolean] `true` when the expression does not produce a
    #   value at this position.
    def missing?
      equal?(MISSING)
    end

    # @return [Boolean]
    def ==(other)
      other.is_a?(SourceLocation) &&
        pointer == other.pointer &&
        derived? == other.derived? &&
        inputs == other.inputs
    end
    alias eql? ==

    def hash
      [pointer, @derived, inputs].hash
    end

    def inspect
      if missing?
        '#<JMESPath::SourceLocation missing>'
      elsif derived?
        "#<JMESPath::SourceLocation derived inputs=#{inputs.inspect}>"
      else
        "#<JMESPath::SourceLocation #{pointer.inspect}>"
      end
    end

    EMPTY_INPUTS = [].freeze
    private_constant :EMPTY_INPUTS

    MISSING = new(nil, false, EMPTY_INPUTS)
    MISSING.freeze
    private_constant :MISSING
  end
end
