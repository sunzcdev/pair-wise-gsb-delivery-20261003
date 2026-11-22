# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Resources
      # Describes an attribute that was supplied by more than one source with
      # different values while resources were merged.
      #
      # @!attribute key
      #   @return [String] the attribute key
      # @!attribute kept_value
      #   @return [Object] the value kept by the merged resource
      # @!attribute kept_source
      #   @return [Object, nil] the source label that supplied {kept_value}
      # @!attribute replaced_value
      #   @return [Object] the value that was replaced
      # @!attribute replaced_source
      #   @return [Object, nil] the source label that supplied {replaced_value}
      Conflict = Struct.new(:key, :kept_value, :kept_source, :replaced_value, :replaced_source, keyword_init: true)

      # Describes two different non-empty schema URLs meeting in a merge.
      #
      # @!attribute kept_schema_url
      #   @return [String] the schema URL kept by the merged resource
      # @!attribute kept_source
      #   @return [Object, nil] the source label that supplied {kept_schema_url}
      # @!attribute replaced_schema_url
      #   @return [String] the schema URL that was replaced
      # @!attribute replaced_source
      #   @return [Object, nil] the source label that supplied {replaced_schema_url}
      SchemaConflict = Struct.new(:kept_schema_url, :kept_source, :replaced_schema_url, :replaced_source, keyword_init: true)

      # A read-only view of how a resource's attributes and schema URL were
      # resolved by the merges that produced it.
      #
      # @!attribute attributes
      #   @return [Hash{String => Object}] the final attribute values
      # @!attribute sources
      #   @return [Hash{String => Object}] attribute key to source label
      # @!attribute schema_url
      #   @return [String, nil] the schema URL of the final resource
      # @!attribute schema_source
      #   @return [Object, nil] the source label that supplied {schema_url}
      # @!attribute conflicts
      #   @return [Array<Conflict>] attribute collisions resolved by the merge
      # @!attribute schema_conflict
      #   @return [SchemaConflict, nil] the schema URL collision, if any
      MergeReport = Struct.new(:attributes, :sources, :schema_url, :schema_source, :conflicts, :schema_conflict, keyword_init: true) do
        # Returns the source label that supplied +key+ in the final resource.
        #
        # @param [String] key the attribute key
        # @return [Object, nil]
        def source_of(key)
          sources[key]
        end
      end
    end
  end
end