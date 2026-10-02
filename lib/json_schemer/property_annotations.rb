# frozen_string_literal: true
module JSONSchemer
  # Traverses a validation result tree and yields property annotation results
  # (`properties`, `patternProperties`, `additionalProperties` or
  # `unevaluatedProperties`) that are visible at the traversal entry point,
  # following the draft 2020-12 evaluation annotation rules:
  #
  # * in-place applicators (`$ref`, `allOf`, `anyOf`, `oneOf`, `if`/`then`/
  #   `else`, `dependentSchemas`) only expose annotations from successful
  #   subschemas
  # * `not` never contributes annotations
  #
  # The same traversal is used by the `unevaluatedProperties` keyword and the
  # optional property consumption report so they can never disagree.
  module PropertyAnnotations
    PROPERTY_KEYWORD_CLASSES = [
      Draft202012::Vocab::Applicator::Properties,
      Draft202012::Vocab::Applicator::PatternProperties,
      Draft202012::Vocab::Applicator::AdditionalProperties,
      Draft202012::Vocab::Unevaluated::UnevaluatedProperties
    ].freeze

    NON_CONTRIBUTING_APPLICATORS = [
      Draft202012::Vocab::Applicator::Not
    ].freeze

    class << self
      def each(result, &block)
        case result.source
        when *PROPERTY_KEYWORD_CLASSES
          yield(result)
        when *NON_CONTRIBUTING_APPLICATORS
          # `not` never produces annotations
        else
          result.nested&.each do |nested_result|
            if nested_result.valid && nested_result.instance_location == result.instance_location
              each(nested_result, &block)
            end
          end
        end
      end
    end
  end

  # Builds an optional per-object report of the property constraints evaluated
  # during validation and the schema locations that supplied them. It walks the
  # already-produced validation result tree once, so no keywords or property
  # hooks are executed a second time.
  module PropertyConsumptionReport
    PROPERTIES = Draft202012::Vocab::Applicator::Properties
    PATTERN_PROPERTIES = Draft202012::Vocab::Applicator::PatternProperties
    ADDITIONAL_PROPERTIES = Draft202012::Vocab::Applicator::AdditionalProperties
    UNEVALUATED_PROPERTIES = Draft202012::Vocab::Unevaluated::UnevaluatedProperties

    class << self
      def build(root_result)
        object_locations = {}

        walk(root_result) do |schema_result|
          next unless schema_result.instance.is_a?(Hash)

          location = Location.resolve(schema_result.instance_location)
          entry = object_locations[location] ||= {
            :instance => schema_result.instance,
            :sources => Hash.new { |hash, key| hash[key] = {} },
            :consumed => Set[]
          }

          schema_result.nested&.each do |keyword_result|
            PropertyAnnotations.each(keyword_result) do |annotation_result|
              collect_sources(annotation_result, entry[:sources])
              collect_consumed(annotation_result, entry[:consumed])
            end
          end
        end

        object_locations.keys.sort.map do |location|
          entry = object_locations.fetch(location)
          evaluated_properties = entry[:sources].keys.sort.each_with_object({}) do |property, out|
            out[property] = entry[:sources].fetch(property).values
          end
          remaining_properties = entry[:instance].keys.reject { |key| entry[:consumed].include?(key) }.sort
          {
            'instanceLocation' => location,
            'evaluatedProperties' => evaluated_properties,
            'remainingProperties' => remaining_properties
          }
        end
      end

    private

      def walk(result, &block)
        yield(result) if result.source.is_a?(Schema)
        result.nested&.each { |nested_result| walk(nested_result, &block) }
      end

      def collect_consumed(result, consumed)
        return if result.source.is_a?(UNEVALUATED_PROPERTIES)
        return unless result.valid

        consumed.merge(result.annotation)
      end

      def collect_sources(result, sources)
        case result.source
        when PROPERTIES
          result.source.parsed.each_key do |property|
            add_source(result, sources, property, Location.join(result.keyword_location, property))
          end
        when PATTERN_PROPERTIES
          result.source.parsed.each_key do |pattern|
            pattern_keyword_location = Location.join(result.keyword_location, pattern)
            result.annotation.each { |property| add_source(result, sources, property, pattern_keyword_location) }
          end
        when ADDITIONAL_PROPERTIES, UNEVALUATED_PROPERTIES
          result.annotation.each { |property| add_source(result, sources, property, result.keyword_location) }
        end
      end

      def add_source(annotation_result, sources, property, keyword_location)
        return unless annotation_result.instance.key?(property)

        nested_result = find_property_result(annotation_result, property)
        resolved_keyword_location = Location.resolve(keyword_location)
        source = nested_result ? source_from_nested(annotation_result, nested_result, resolved_keyword_location)
                               : source_from_annotation(annotation_result, resolved_keyword_location)
        sources[property][source.fetch('absoluteKeywordLocation') + resolved_keyword_location] = source
      end

      def find_property_result(annotation_result, property)
        property_location =
          "#{Location.resolve(annotation_result.instance_location)}/#{Location.escape_json_pointer_token(property)}"
        annotation_result.nested&.find { |result| Location.resolve(result.instance_location) == property_location }
      end

      def source_from_nested(annotation_result, nested_result, resolved_keyword_location)
        {
          'keyword' => annotation_result.source.error_key,
          'keywordLocation' => resolved_keyword_location,
          'absoluteKeywordLocation' => nested_result.source.absolute_keyword_location,
          'valid' => nested_result.valid
        }
      end

      def source_from_annotation(annotation_result, resolved_keyword_location)
        {
          'keyword' => annotation_result.source.error_key,
          'keywordLocation' => resolved_keyword_location,
          'absoluteKeywordLocation' => annotation_result.source.absolute_keyword_location,
          'valid' => annotation_result.valid
        }
      end
    end
  end
end
