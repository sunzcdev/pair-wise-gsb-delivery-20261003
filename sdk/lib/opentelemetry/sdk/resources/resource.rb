# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Resources
      # Resource represents a resource, which captures identifying information about the entities
      # for which telemetry (metrics or traces) is reported.
      class Resource
        # Aliases so the report types are discoverable from the class that
        # produces them; the definitions live in their own files.
        Conflict = Resources::Conflict
        SchemaConflict = Resources::SchemaConflict
        MergeReport = Resources::MergeReport

        class << self
          private :new

          # Returns a newly created Resource with the specified attributes
          #
          # @param [Hash{String => String, Numeric, Boolean}] attributes Hash of key-value pairs to be used
          #   as attributes for this resource. For backward compatibility a
          #   bare, string-keyed hash may also be passed as keywords.
          # @param [optional String] schema_url The schema URL that describes
          #   the attribute keys. An empty value means the resource has no
          #   schema URL.
          # @param [optional Object] source A label describing where the
          #   attributes came from. It is recorded per attribute in the
          #   {MergeReport}, which makes three-layer configuration diagnosable.
          # @raise [ArgumentError] If attribute keys and values are not strings
          # @return [Resource]
          #
          # @example
          #   Resource.create('service.name' => 'checkout', source: :application)
          def create(attributes = nil, schema_url: nil, source: nil, **legacy_attributes)
            attributes = legacy_attributes if attributes.nil?

            frozen_attributes = attributes.each_with_object({}) do |(k, v), memo|
              raise ArgumentError, 'attribute keys must be strings' unless k.is_a?(String)
              raise ArgumentError, 'attribute values must be (array of) strings, integers, floats, or booleans' unless Internal.valid_value?(v)

              memo[-k] = v.freeze
            end.freeze

            frozen_schema_url = normalize_schema_url(schema_url)
            sources = frozen_attributes.each_key.with_object({}) { |key, memo| memo[key] = source unless source.nil? }
            provenance = Provenance.new(sources: sources, schema_url: frozen_schema_url, schema_source: frozen_schema_url && source)

            new(frozen_attributes, provenance)
          end

          # Returns the default Resource for the current process and SDK.
          #
          # @return [Resource]
          def default
            @default ||= create({ SemanticConventions::Resource::SERVICE_NAME => 'unknown_service' }, source: :default).merge(process).merge(telemetry_sdk).merge(service_name_from_env)
          end

          # Returns a Resource describing this telemetry SDK.
          #
          # @return [Resource]
          def telemetry_sdk
            sdk_attributes = {
              SemanticConventions::Resource::TELEMETRY_SDK_NAME => 'opentelemetry',
              SemanticConventions::Resource::TELEMETRY_SDK_LANGUAGE => 'ruby',
              SemanticConventions::Resource::TELEMETRY_SDK_VERSION => OpenTelemetry::SDK::VERSION
            }

            resource_pairs = ENV.fetch('OTEL_RESOURCE_ATTRIBUTES', nil)
            return create(sdk_attributes, source: :telemetry_sdk) unless resource_pairs.is_a?(String)

            overrides = parse_resource_pairs(resource_pairs)
            surviving = overrides.reject { |_key, value| value.nil? || value.empty? }
            # Historical behaviour: an override with a missing or empty value
            # removes the key instead of setting it, even for the built-in
            # telemetry.sdk.* attributes. Non-empty overrides go through a
            # normal merge so any collision with a built-in value is reported.
            removed = overrides.keys - surviving.keys
            sdk_attributes = sdk_attributes.reject { |key, _value| removed.include?(key) }

            resource = create(sdk_attributes, source: :telemetry_sdk)
            return resource if surviving.empty?

            resource.merge(create(surviving, source: :environment), source: :environment)
          end

          # Returns a Resource describing the current process.
          #
          # @return [Resource]
          def process
            resource_attributes = {
              SemanticConventions::Resource::PROCESS_PID => Process.pid,
              SemanticConventions::Resource::PROCESS_COMMAND => $PROGRAM_NAME,
              SemanticConventions::Resource::PROCESS_RUNTIME_NAME => RUBY_ENGINE,
              SemanticConventions::Resource::PROCESS_RUNTIME_VERSION => RUBY_VERSION,
              SemanticConventions::Resource::PROCESS_RUNTIME_DESCRIPTION => RUBY_DESCRIPTION
            }

            create(resource_attributes, source: :process)
          end

          private

          def service_name_from_env
            service_name = ENV.fetch('OTEL_SERVICE_NAME', nil)
            create({ SemanticConventions::Resource::SERVICE_NAME => service_name }, source: :environment) unless service_name.nil?
          end

          # An empty schema URL is not a usable schema, so it is normalized away
          # instead of being treated as an explicit value.
          def normalize_schema_url(schema_url)
            return nil if schema_url.nil?

            value = schema_url.to_s
            value.empty? ? nil : -value
          end

          def parse_resource_pairs(resource_pairs)
            resource_pairs.split(',').each_with_object({}) do |pair, memo|
              key, value = pair.split('=')
              memo[key] = value
            end
          end
        end

        # @api private
        # The constructor is private and only for use internally by the class.
        # Users should use the {create} factory method to obtain a Resource
        # instance.
        #
        # @param [Hash<String, String>] frozen_attributes Frozen-hash of frozen-string
        #  key-value pairs to be used as attributes for this resource
        # @param [Provenance] provenance Tracks the source of each attribute and
        #   of the schema URL
        # @return [Resource]
        def initialize(frozen_attributes, provenance)
          @attributes = frozen_attributes
          @provenance = provenance
        end

        # Returns an enumerator for attributes of this Resource
        #
        # @return [Enumerator]
        def attribute_enumerator
          @attribute_enumerator ||= attributes.to_enum
        end

        # Returns a new, merged Resource by merging the current Resource with
        # the other Resource. In case of a collision, the other Resource
        # takes precedence
        #
        # Attribute collisions and schema URL conflicts are recorded in the
        # merged resource's {#merge_report} instead of raising. Explicitly
        # supplied empty strings are ordinary attribute values and take part in
        # collisions like any other value.
        #
        # @param [Resource] other The other resource to merge
        # @param [optional Object] source A label used for attributes and a
        #   schema URL in +other+ that do not already carry one. Labels already
        #   present on +other+ are preserved, which keeps three-layer
        #   configuration distinguishable in the {MergeReport}.
        # @return [Resource] A new resource formed by merging the current resource
        #   with other
        def merge(other, source: nil)
          return self unless other.is_a?(Resource)

          merged_attributes = attributes.merge(other.attributes).freeze
          merged_provenance = provenance.merge(other.provenance, own_attributes: attributes,
                                                                  incoming_attributes: other.attributes,
                                                                  source: source)

          self.class.send(:new, merged_attributes, merged_provenance)
        end

        # Returns the schema URL associated with this Resource, if any.
        #
        # @return [String, nil]
        def schema_url
          provenance.schema_url
        end

        # Returns a snapshot describing where each attribute and the schema URL
        # of this resource came from, and which collisions the merges resolved.
        #
        # The report is derived from immutable state captured at merge time, so
        # it stays valid after the merged resource is handed to providers and
        # exporters.
        #
        # @return [MergeReport]
        def merge_report
          provenance.to_report(attributes)
        end

        protected

        attr_reader :attributes, :provenance
      end
    end
  end
end
