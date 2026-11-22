# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Resources
      # @api private
      #
      # Provenance tracks where each attribute and the schema URL of a
      # {Resource} came from, and which collisions were resolved while merging.
      #
      # Instances are immutable: a merge builds a new Provenance instead of
      # mutating an existing one, so a {MergeReport} built from one stays valid
      # after the resource is merged again.
      class Provenance
        EMPTY_SOURCES = {}.freeze
        EMPTY_CONFLICTS = [].freeze
        private_constant :EMPTY_SOURCES, :EMPTY_CONFLICTS

        attr_reader :sources, :schema_url, :schema_source, :conflicts, :schema_conflict

        # @param [Hash] sources attribute key to source label
        # @param [String, nil] schema_url the resource's schema URL
        # @param [Object, nil] schema_source the label that supplied +schema_url+
        # @param [Array<Conflict>] conflicts attribute collisions resolved so far
        # @param [SchemaConflict, nil] schema_conflict the schema URL collision
        def initialize(sources: EMPTY_SOURCES, schema_url: nil, schema_source: nil, conflicts: EMPTY_CONFLICTS, schema_conflict: nil)
          @sources = sources.freeze
          @schema_url = schema_url
          @schema_source = schema_source
          @conflicts = conflicts.freeze
          @schema_conflict = schema_conflict
        end

        # Returns a new Provenance for the result of merging +other+ on top of
        # the receiver.
        #
        # @param [Provenance] other the provenance of the incoming resource
        # @param [Hash] own_attributes the receiver's attributes before merging
        # @param [Hash] incoming_attributes the incoming resource's attributes
        # @param [Object, nil] source a label for incoming values that do not
        #   already carry one
        # @return [Provenance]
        def merge(other, own_attributes:, incoming_attributes:, source:)
          incoming_sources = label_incoming(other, incoming_attributes, source)
          resolved_url, resolved_source, resolved_conflict = resolve_schema(other, source)
          resolved_conflicts = (conflicts + other.conflicts +
                                collisions(own_attributes, incoming_attributes, incoming_sources)).uniq

          Provenance.new(
            sources: sources.merge(incoming_sources),
            schema_url: resolved_url,
            schema_source: resolved_source,
            schema_conflict: resolved_conflict,
            conflicts: resolved_conflicts
          )
        end

        # Returns a frozen {MergeReport} describing +attributes+ and this
        # provenance.
        #
        # @param [Hash] attributes the resource's final attributes
        # @return [MergeReport]
        def to_report(attributes)
          MergeReport.new(
            attributes: attributes.dup.freeze,
            sources: sources,
            schema_url: schema_url,
            schema_source: schema_source,
            conflicts: conflicts,
            schema_conflict: schema_conflict
          ).freeze
        end

        private

        # Labels incoming attributes that do not carry a source yet. An explicit
        # +source+ labels only the unlabeled ones, so nested merges keep the
        # more specific provenance already recorded.
        def label_incoming(other, incoming_attributes, source)
          incoming = other.sources.dup
          incoming_attributes.each_key { |key| incoming[key] = source unless incoming.key?(key) } unless source.nil?
          incoming
        end

        def collisions(own_attributes, incoming_attributes, incoming_sources)
          incoming_attributes.each_with_object([]) do |(key, value), memo|
            next unless own_attributes.key?(key)
            next if own_attributes[key] == value

            memo << Conflict.new(
              key: key,
              kept_value: value,
              kept_source: incoming_sources[key],
              replaced_value: own_attributes[key],
              replaced_source: sources[key]
            )
          end
        end

        # A non-empty schema URL from the incoming resource wins, but a genuine
        # mismatch is never silent: it is recorded as a {SchemaConflict} so the
        # decision can be inspected later.
        def resolve_schema(other, source)
          incoming = other.schema_url
          incoming_source = other.schema_source || source

          return [schema_url, schema_source, schema_conflict] if incoming.nil?
          return [incoming, incoming_source, schema_conflict] if schema_url.nil?

          if incoming == schema_url
            [schema_url, schema_source || incoming_source, schema_conflict]
          else
            [incoming, incoming_source,
             SchemaConflict.new(kept_schema_url: incoming, kept_source: incoming_source,
                                replaced_schema_url: schema_url, replaced_source: schema_source)]
          end
        end
      end
    end
  end
end