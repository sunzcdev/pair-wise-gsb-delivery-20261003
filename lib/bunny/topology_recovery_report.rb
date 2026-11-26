# -*- coding: utf-8 -*-
# frozen_string_literal: true

module Bunny
  # Collects the outcome of a single topology recovery pass: every entity
  # that was actually recovered and every entity that was intentionally
  # skipped, together with a machine-readable reason for the skip.
  #
  # This makes partial, dependency-aware recovery explainable: when an
  # exchange is filtered out, the bindings (and, where applicable, consumers)
  # that depend on it are reported with a reason instead of either failing
  # opaquely or silently disappearing.
  #
  # @see Bunny::Session#recover_topology
  class TopologyRecoveryReport
    # Entity kinds
    EXCHANGE          = :exchange
    QUEUE             = :queue
    QUEUE_BINDING     = :queue_binding
    EXCHANGE_BINDING  = :exchange_binding
    CONSUMER          = :consumer

    # Skip reasons
    # Filtered out by the configured {Bunny::TopologyRecoveryFilter}.
    FILTERED_OUT                       = :filtered_out
    # A pre-defined broker exchange (amq.*, the default exchange): the
    # broker always provides it, so it is never declared by the client.
    PREDECLARED                        = :predeclared
    # Marked as externally predeclared: another component owns the entity
    # and guarantees it still exists, so this connection does not declare it.
    EXTERNALLY_PREDECLARED             = :externally_predeclared
    # A binding whose source exchange is neither recovered nor known to
    # exist externally, so issuing it would fail against a missing entity.
    MISSING_SOURCE_EXCHANGE            = :missing_source_exchange
    # A binding whose destination exchange is neither recovered nor known
    # to exist externally.
    MISSING_DESTINATION_EXCHANGE       = :missing_destination_exchange
    # A binding whose destination queue is neither recovered nor known to
    # exist externally.
    MISSING_DESTINATION_QUEUE          = :missing_destination_queue

    Entry = Struct.new(:kind, :entity, :reason, keyword_init: true) do
      def skipped?
        !reason.nil?
      end

      def recovered?
        reason.nil?
      end
    end

    def initialize
      @entries = []
    end

    # @return [Array<Entry>]
    attr_reader :entries

    # Records an entity that was actually recovered (a command was issued).
    # @param kind [Symbol]
    # @param entity [Bunny::RecordedEntity]
    def record_recovered(kind, entity)
      @entries << Entry.new(kind: kind, entity: entity, reason: nil)
      self
    end

    # Records an entity that was intentionally skipped.
    # @param kind [Symbol]
    # @param entity [Bunny::RecordedEntity]
    # @param reason [Symbol] one of the reason constants on this class
    def record_skipped(kind, entity, reason)
      @entries << Entry.new(kind: kind, entity: entity, reason: reason)
      self
    end

    # Re-classifies an existing skip entry for an entity (for example turning a
    # {TopologyRecoveryReport::FILTERED_OUT} entry into an
    # {TopologyRecoveryReport::EXTERNALLY_PREDECLARED} one), or records a new
    # skip entry when the entity had not been reported yet. This keeps exactly
    # one, authoritative entry per entity.
    # @param kind [Symbol]
    # @param entity [Bunny::RecordedEntity]
    # @param reason [Symbol]
    def replace_skip(kind, entity, reason)
      existing = @entries.find { |e| e.kind == kind && e.entity == entity }
      if existing
        existing.reason = reason
      else
        record_skipped(kind, entity, reason)
      end
      self
    end

    # @return [Array<Entry>] entities for which a recovery command was issued
    def recovered
      @entries.select(&:recovered?)
    end

    # @return [Array<Entry>] entities that were intentionally skipped
    def skipped
      @entries.select(&:skipped?)
    end

    # @param kind [Symbol]
    # @return [Array<Entry>]
    def recovered_of_kind(kind)
      recovered.select { |e| e.kind == kind }
    end

    # @param kind [Symbol]
    # @return [Array<Entry>]
    def skipped_of_kind(kind)
      skipped.select { |e| e.kind == kind }
    end

    # @param kind [Symbol]
    # @param reason [Symbol]
    # @return [Array<Entry>]
    def skipped_with(kind, reason)
      skipped.select { |e| e.kind == kind && e.reason == reason }
    end
  end
end
