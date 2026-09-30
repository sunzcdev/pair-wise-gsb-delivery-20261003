# encoding: UTF-8

module TZInfo
  module DataSources
    # Represents a time zone that is defined as a link to or alias of another
    # zone.
    class LinkedTimezoneInfo < TimezoneInfo
      # @return [String] the identifier of the time zone that provides the data
      # (that this zone links to or is an alias for).
      attr_reader :link_to_identifier

      # Initializes a new {LinkedTimezoneInfo}. The passed in `identifier` and
      # `link_to_identifier` instances will be frozen.
      #
      # @param identifier [String] the identifier of the time zone.
      # @param link_to_identifier [String] the identifier of the time zone that
      #   this zone link to.
      # @raise [ArgumentError] if `identifier` or `link_to_identifier` are
      # `nil`.
      def initialize(identifier, link_to_identifier)
        super(identifier)
        raise ArgumentError, 'link_to_identifier must be specified' unless link_to_identifier
        @link_to_identifier = link_to_identifier.freeze
      end

      # @param data_source [DataSource] the {DataSource} to use when resolving
      #   the linked time zone.
      # @return [LinkedTimezone] a new {LinkedTimezone} instance for the time
      #   zone represented by this {LinkedTimezoneInfo}.
      def create_timezone(data_source = nil)
        if data_source
          LinkedTimezone.new(self, data_source)
        else
          LinkedTimezone.new(self)
        end
      end
    end
  end
end
