# frozen_string_literal: true
module JSONSchemer
  CATCHALL = '*'
  I18N_SEPARATOR = "\x1F" # unit separator
  I18N_SCOPE = 'json_schemer'
  I18N_ERRORS_SCOPE = "#{I18N_SCOPE}#{I18N_SEPARATOR}errors"
  X_ERROR_REGEX = /%\{(instance|instanceLocation|formattedInstanceLocation|keywordValue|keywordLocation|absoluteKeywordLocation|details|details__\w+)\}/
  CLASSIC_ERROR_TYPES = Hash.new do |hash, klass|
    hash[klass] = klass.name.rpartition('::').last.sub(/\A[[:alpha:]]/, &:downcase)
  end

  Result = Struct.new(:source, :instance, :instance_location, :keyword_location, :valid, :nested, :type, :annotation, :details, :ignore_nested, :nested_key) do
    def output(output_format)
      case output_format
      when 'classic'
        classic
      when 'flag'
        flag
      when 'basic'
        basic
      when 'detailed'
        detailed
      when 'verbose'
        verbose
      else
        raise UnknownOutputFormat, output_format
      end
    end

    def error
      return @error if defined?(@error)
      if source.x_error
        x_error_replacements = interpolation_variables.transform_keys { |key| "%{#{key}}" }
        # not using sprintf because it warns: "too many arguments for format string"
        @error = source.x_error.gsub(X_ERROR_REGEX, x_error_replacements)
        @x_error = true
      else
        @error = source.error(:formatted_instance_location => formatted_instance_location, :details => details)
        if i18n?
          begin
            @error = i18n!
            @i18n = true
          rescue I18n::MissingTranslationData
          end
        end
      end
      @error
    end

    def i18n?
      return @@i18n if defined?(@@i18n)
      @@i18n = defined?(I18n) && I18n.exists?(I18N_SCOPE)
    end

    def i18n!
      base_uri_str = source.schema.base_uri.to_s
      meta_schema_base_uri_str = source.schema.meta_schema.base_uri.to_s
      error_key = source.error_key
      I18n.translate!(
        source.absolute_keyword_location,
        :default => [
          "#{base_uri_str}#{I18N_SEPARATOR}##{resolved_keyword_location}",
          "##{resolved_keyword_location}",
          "#{base_uri_str}#{I18N_SEPARATOR}#{error_key}",
          "#{base_uri_str}#{I18N_SEPARATOR}#{CATCHALL}",
          "#{meta_schema_base_uri_str}#{I18N_SEPARATOR}#{error_key}",
          "#{meta_schema_base_uri_str}#{I18N_SEPARATOR}#{CATCHALL}",
          error_key,
          CATCHALL
        ].map!(&:to_sym),
        :separator => I18N_SEPARATOR,
        :scope => I18N_ERRORS_SCOPE,
        **interpolation_variables
      )
    end

    def to_output_unit
      out = {
        'valid' => valid,
        'keywordLocation' => resolved_keyword_location,
        'absoluteKeywordLocation' => source.absolute_keyword_location,
        'instanceLocation' => resolved_instance_location
      }
      if valid
        out['annotation'] = annotation if annotation
      else
        out['error'] = error
        out['x-error'] = true if @x_error
        out['i18n'] = true if @i18n
      end
      out
    end

    def to_classic
      schema = source.schema
      out = {
        'data' => instance,
        'data_pointer' => resolved_instance_location,
        'schema' => schema.value,
        'schema_pointer' => schema.schema_pointer,
        'root_schema' => schema.root.value,
        'type' => type || CLASSIC_ERROR_TYPES[source.class]
      }
      out['error'] = error
      out['x-error'] = true if @x_error
      out['i18n'] = true if @i18n
      out['details'] = details if details
      out
    end

    def flag
      { 'valid' => valid }
    end

    def basic
      out = to_output_unit
      if nested&.any?
        out[nested_key] = Enumerator.new do |yielder|
          results = [self]
          while result = results.pop
            if result.ignore_nested || !result.nested&.any?
              yielder << result.to_output_unit
            else
              previous_results_size = results.size
              result.nested.reverse_each do |nested_result|
                results << nested_result if nested_result.valid == valid
              end
              yielder << result.to_output_unit unless (results.size - previous_results_size) == 1
            end
          end
        end
      end
      out
    end

    def detailed
      return to_output_unit if ignore_nested || !nested&.any?
      matching_results = nested.select { |nested_result| nested_result.valid == valid }
      if matching_results.size == 1
        matching_results.first.detailed
      else
        out = to_output_unit
        if matching_results.any?
          out[nested_key] = Enumerator.new do |yielder|
            matching_results.each { |nested_result| yielder << nested_result.detailed }
          end
        end
        out
      end
    end

    def verbose
      out = to_output_unit
      if nested&.any?
        out[nested_key] = Enumerator.new do |yielder|
          nested.each { |nested_result| yielder << nested_result.verbose }
        end
      end
      out
    end

    def classic
      Enumerator.new do |yielder|
        unless valid
          results = [self]
          while result = results.pop
            if result.ignore_nested || !result.nested&.any?
              yielder << result.to_classic
            else
              previous_results_size = results.size
              result.nested.reverse_each do |nested_result|
                results << nested_result if nested_result.valid == valid
              end
              yielder << result.to_classic if (results.size - previous_results_size) == 0
            end
          end
        end
      end
    end

    def insert_property_defaults(context)
      instance_locations = {}
      instance_locations.compare_by_identity

      results = [[self, true]]
      while (result, valid = results.pop)
        next if result.source.is_a?(Schema::NOT_KEYWORD_CLASS)

        valid &&= result.valid
        result.nested&.each { |nested_result| results << [nested_result, valid] }

        if result.source.is_a?(Schema::PROPERTIES_KEYWORD_CLASS) && result.instance.is_a?(Hash)
          result.source.parsed.each do |property, schema|
            next if result.instance.key?(property)
            next unless default = default_keyword_instance(schema)
            instance_location = Location.join(result.instance_location, property)
            keyword_location = Location.join(Location.join(result.keyword_location, property), default.keyword)
            default_result = default.validate(nil, instance_location, keyword_location, nil)
            instance_locations[result.instance_location] ||= {}
            instance_locations[result.instance_location][property] ||= []
            instance_locations[result.instance_location][property] << [default_result, valid]
          end
        end
      end

      inserted = false

      instance_locations.each do |instance_location, properties|
        original_instance = context.original_instance(instance_location)
        properties.each do |property, results_with_tree_validity|
          property_inserted = yield(original_instance, property, results_with_tree_validity)
          inserted ||= (property_inserted != false)
        end
      end

      inserted
    end

    # Builds a per-object report of the properties that were "consumed" during
    # validation, following the draft 2020-12 annotation collection rules used
    # by `unevaluatedProperties`.
    #
    # The report is derived from the existing result tree only, so no keywords
    # (including custom keywords and property hooks) are evaluated again.
    def property_consumption_report
      # An "object scope" is a Schema result that validates an object instance:
      # the root and any schema applied through a declarative keyword
      # (`properties`/`patternProperties`/`additionalProperties`/items
      # variants/`contains`). `unevaluatedProperties` does not open a scope: its
      # nested results validate individual remaining property values, not the
      # object itself. Each scope consumes properties through its own adjacent
      # keywords, following the draft 2020-12 annotation collection rules used
      # by `unevaluatedProperties` (successful in-place applicator branches
      # contribute, failed ones and `not` do not).
      # A value schema reached through a different declarative path is a cousin
      # scope at the same instance location and does not share annotations; when
      # several scopes validate one location, the scope containing
      # `unevaluatedProperties` is the adjudicator the report aligns with.
      scopes = {}
      scopes.compare_by_identity
      scope_order = []

      record_scope = lambda do |schema_result|
        # A schema can validate several objects through a recursive `$ref`, so
        # scope identity includes the instance location.
        key = [schema_result.source, schema_result.instance_location]
        unless scopes.key?(key)
          scopes[key] = {
            :schema_result => schema_result,
            :location => schema_result.instance_location,
            :instance => schema_result.instance,
            :evaluated => {},
            :consumed_keys => Set[],
            :adjudicates => schema_adjudicates?(schema_result.source)
          }
          scope_order << key
        end
      end

      discover = nil
      discover = lambda do |result|
        if result.source.is_a?(Schema)
          if result.instance.is_a?(Hash)
            record_scope.call(result)
          end
          result.nested&.each { |nested| discover.call(nested) }
        else
          declarative = declarative_keyword?(result.source)
          result.nested&.each do |nested|
            if nested.source.is_a?(Schema)
              same_location = nested.instance_location == result.instance_location
              if same_location
                # In-place application (`$ref`, applicator branches): a failed
                # branch contributes nothing.
                discover.call(nested) if nested.valid
              elsif declarative
                discover.call(nested)
              end
            else
              discover.call(nested)
            end
          end
        end
      end

      discover.call(self)

      scope_order.each do |key|
        scope = scopes.fetch(key)
        evaluated = scope[:evaluated]
        consumed_keys = scope[:consumed_keys]
        scope[:schema_result].nested&.each do |keyword_result|
          # `evaluated` lists properties consumed by `properties`/
          # `patternProperties`/`additionalProperties` (with sources).
          # `consumed_keys` additionally accounts for nested in-place
          # `unevaluatedProperties`; the scope's own `unevaluatedProperties`
          # does not satisfy itself.
          collect_property_annotations(keyword_result, evaluated, consumed_keys, false)
        end
      end

      by_location = {}
      by_location.compare_by_identity
      location_order = []
      scope_order.each do |key|
        scope = scopes.fetch(key)
        location = scope[:location]
        if by_location.key?(location)
          slot = by_location.fetch(location)
          if scope[:adjudicates] && !slot[:adjudicates]
            slot[:scopes] = [scope]
            slot[:adjudicates] = true
          elsif !slot[:adjudicates]
            slot[:scopes] << scope
          end
        else
          by_location[location] = { :scopes => [scope], :adjudicates => scope[:adjudicates] }
          location_order << location
        end
      end

      location_order.map do |location|
        slot = by_location.fetch(location)
        instance = slot[:scopes].first[:instance]
        evaluated = {}
        consumed_keys = Set[]
        slot[:scopes].each do |scope|
          scope[:evaluated].each do |property, sources|
            merged = evaluated[property] ||= []
            sources.each do |source|
              already_present = merged.any? do |existing|
                existing['absoluteKeywordLocation'] == source['absoluteKeywordLocation']
              end
              merged << source unless already_present
            end
          end
          consumed_keys.merge(scope[:consumed_keys])
        end
        {
          'instanceLocation' => Location.resolve(location),
          'evaluatedProperties' => evaluated.keys.sort.map do |property|
            { 'name' => property, 'sources' => evaluated.fetch(property) }
          end,
          'remainingProperties' => instance.keys.reject { |key| consumed_keys.include?(key) }.sort
        }
      end
    end
  private

    def declarative_keyword?(source)
      declarative_scope_keyword_classes.any? { |klass| source.is_a?(klass) }
    end

    def schema_adjudicates?(schema)
      schema.parsed.is_a?(Hash) &&
        schema.parsed.keys.any? do |keyword|
          schema.parsed.fetch(keyword).is_a?(Draft202012::Vocab::Unevaluated::UnevaluatedProperties)
        end
    end

    def declarative_scope_keyword_classes
      @declarative_scope_keyword_classes ||= [
        Draft202012::Vocab::Applicator::Properties,
        Draft202012::Vocab::Applicator::PatternProperties,
        Draft202012::Vocab::Applicator::AdditionalProperties,
        Draft202012::Vocab::Applicator::Items,
        Draft202012::Vocab::Applicator::PrefixItems,
        Draft202012::Vocab::Applicator::Contains,
        Draft202012::Vocab::Unevaluated::UnevaluatedItems,
        Draft201909::Vocab::Applicator::Items,
        Draft201909::Vocab::Applicator::AdditionalItems,
        Draft201909::Vocab::Applicator::UnevaluatedItems
      ].freeze
    end

    # Collect property annotations within one object scope, mirroring
    # `UnevaluatedProperties#collect_evaluated_keys`: an adjacent property
    # keyword annotation is merged and the walk only descends into nested
    # applicator (`allOf`/`anyOf`/...) branches that successfully validated the
    # same instance location.
    #
    # `evaluated` records properties consumed by `properties`/
    # `patternProperties`/`additionalProperties` together with their constraint
    # source locations. `consumed_keys` is the full evaluated set used to derive
    # `remainingProperties`, additionally including keys consumed by a nested
    # in-place `unevaluatedProperties`. The scope's own `unevaluatedProperties`
    # is excluded (`include_unevaluated` starts false) because it cannot satisfy
    # itself.
    def collect_property_annotations(result, evaluated, consumed_keys, include_unevaluated)
      case result.source
      when Draft202012::Vocab::Applicator::Properties
        merge_properties_annotation(result, evaluated, consumed_keys)
      when Draft202012::Vocab::Applicator::PatternProperties
        merge_pattern_properties_annotation(result, evaluated, consumed_keys)
      when Draft202012::Vocab::Applicator::AdditionalProperties
        merge_additional_properties_annotation(result, evaluated, consumed_keys)
      when Draft202012::Vocab::Unevaluated::UnevaluatedProperties
        consumed_keys.merge(result.annotation) if include_unevaluated
      end
      result.nested&.each do |subresult|
        if subresult.valid && subresult.instance_location == result.instance_location
          collect_property_annotations(subresult, evaluated, consumed_keys, true)
        end
      end
    end

    def merge_properties_annotation(result, evaluated, consumed_keys)
      consumed_keys.merge(result.annotation)
      result.annotation&.each do |property|
        source_result = result.nested&.find do |nested|
          nested.source.is_a?(Schema) && nested.instance_location[:name] == property
        end
        add_evaluated_property(evaluated, property, source_result)
      end
    end

    def merge_pattern_properties_annotation(result, evaluated, consumed_keys)
      consumed_keys.merge(result.annotation)
      result.annotation&.each do |property|
        source_result = result.nested&.find do |nested|
          nested.source.is_a?(Schema) &&
            nested.instance_location[:name] == property &&
            nested.keyword_location[:name] &&
            Location.resolve(nested.keyword_location) == pattern_keyword_location(result, nested)
        end
        add_evaluated_property(evaluated, property, source_result)
      end
    end

    def pattern_keyword_location(result, nested)
      pattern = Location.escape_json_pointer_token(nested.keyword_location[:name])
      "#{Location.resolve(result.keyword_location)}/#{pattern}"
    end

    def merge_additional_properties_annotation(result, evaluated, consumed_keys)
      consumed_keys.merge(result.annotation)
      result.annotation&.each do |property|
        source_result = result.nested&.find do |nested|
          nested.source.is_a?(Schema) && nested.instance_location[:name] == property
        end
        add_evaluated_property(evaluated, property, source_result || result)
      end
    end

    def add_evaluated_property(evaluated, property, source_result)
      source_result ||= self
      sources = evaluated[property] ||= []
      source = {
        'keywordLocation' => Location.resolve(source_result.keyword_location),
        'absoluteKeywordLocation' => source_result.source.absolute_keyword_location
      }
      already_present = sources.any? do |existing|
        existing['absoluteKeywordLocation'] == source['absoluteKeywordLocation']
      end
      sources << source unless already_present
    end

    def resolved_instance_location
      @resolved_instance_location ||= Location.resolve(instance_location)
    end

    def formatted_instance_location
      @formatted_instance_location ||= resolved_instance_location.empty? ? 'root' : "`#{resolved_instance_location}`"
    end

    def resolved_keyword_location
      @resolved_keyword_location ||= Location.resolve(keyword_location)
    end

    def default_keyword_instance(schema)
      schema.parsed.fetch('default') do
        schema.parsed.find do |_keyword, keyword_instance|
          next unless keyword_instance.respond_to?(:ref_schema)
          next unless default = default_keyword_instance(keyword_instance.ref_schema)
          break default
        end
      end
    end

    def interpolation_variables
      interpolation_variables = {
        :instance => instance,
        :instanceLocation => resolved_instance_location,
        :formattedInstanceLocation => formatted_instance_location,
        :keywordValue => source.value,
        :keywordLocation => resolved_keyword_location,
        :absoluteKeywordLocation => source.absolute_keyword_location,
        :details => details,
      }
      details&.each do |key, value|
        interpolation_variables["details__#{key}".to_sym] = value
      end
      interpolation_variables
    end
  end
end
