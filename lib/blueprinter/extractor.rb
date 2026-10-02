# frozen_string_literal: true

module Blueprinter
  class Extractor
    def extract(_field_name, _object, _local_options, _options = {})
      raise NotImplementedError, 'An Extractor must implement #extract'
    end

    # When true, Blueprinter forwards the nested field projection to #extract
    # as a `projection:` keyword argument. Custom extractors may opt in so they
    # can avoid work for fields that were not requested.
    def self.accepts_projection?
      false
    end

    def self.extract(field_name, object, local_options, options = {})
      new.extract(field_name, object, local_options, options)
    end
  end
end
