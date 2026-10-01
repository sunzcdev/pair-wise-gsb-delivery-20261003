module MultiJson
  # Raised when an option cannot be honoured by the selected adapter without
  # breaking its promise. For example, a backend that only exposes Float numbers
  # must not silently round away precision while pretending to preserve the
  # original JSON number.
  class UnsupportedOptionError < ArgumentError
    def self.build(adapter, option, message = nil)
      name = adapter.respond_to?(:name) ? adapter.name : adapter.to_s
      default = "#{name} cannot honour :#{option} without losing information"
      new("#{default}. #{message}".rstrip)
    end
  end
end
