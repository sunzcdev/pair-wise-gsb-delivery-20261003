# frozen_string_literal: true
module JMESPath
  # Internal sentinel used by the diagnostic mode to represent a value
  # that is absent from the data (as opposed to an explicit `null`).
  #
  # A {Missing} value is never part of a public result. Diagnostic
  # results are sanitized through {Diagnostic.sanitize}, which replaces
  # every sentinel with a serializable Hash.
  #
  # @api private
  class Missing
    # @return [Array<String,Integer>] the path segments that were
    #   traversed, up to and including the segment that was absent.
    attr_reader :path

    def initialize(path)
      @path = path.freeze
      freeze
    end

    # Returns a new sentinel for a field access chained after this
    # missing value.
    def child(key)
      Missing.new([*@path, key])
    end

    # @return [Hash] a JSON serializable representation of this sentinel.
    def to_h
      { '$missing' => self.class.format_path(@path) }
    end

    def inspect
      "#<JMESPath::Missing #{self.class.format_path(@path)}>"
    end

    def self.format_path(path)
      path.each_with_object(+'') do |key, str|
        if key.is_a?(Integer)
          str << "[#{key}]"
        else
          str << '.' unless str.empty?
          str << key.to_s
        end
      end
    end
  end
end
