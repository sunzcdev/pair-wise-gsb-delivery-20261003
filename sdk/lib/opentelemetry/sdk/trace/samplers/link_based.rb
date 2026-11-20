# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Trace
      module Samplers
        # @api private
        #
        # LinkBased is a root-span sampler for batch jobs and other operations
        # that are triggered by multiple upstream spans, where no single upstream
        # span can be chosen as the parent. Those upstreams are instead carried as
        # {OpenTelemetry::Trace::Link}s, and this sampler derives the sampling
        # decision for the new root span from the sampled flags of their remote
        # span contexts.
        #
        # Only links whose span context is remote? participate in the decision;
        # local links cannot signal an inbound sampling decision and are ignored.
        # The aggregation is order-independent: reordering the same links never
        # changes the decision.
        #
        # This sampler only acts on root spans (span contexts without a valid
        # parent). Compose it with {Samplers.parent_based} as its +root+ sampler
        # so that spans with a real parent continue to follow the parent-based
        # sampling rules:
        #
        #   Samplers.parent_based(root: Samplers.link_based)
        class LinkBased
          # Attribute attached to a root span whose decision was derived from
          # links. Its value is a human-readable explanation of the decision.
          SAMPLING_REASON_ATTRIBUTE = 'opentelemetry.sampling.reason'

          POLICIES = {
            # Sample when at least one remote link is sampled.
            any_sampled: :any_sampled,
            # Sample only when every remote link is sampled.
            all_sampled: :all_sampled
          }.freeze

          # @param [Symbol] policy How to combine the sampled flags of the remote
          #   linked span contexts. Either +:any_sampled+ (the default) or
          #   +:all_sampled+.
          # @param [Sampler] fallback The sampler to delegate to for root spans
          #   without any remote links (including no links at all).
          # @raise [ArgumentError] if policy is not a supported policy
          def initialize(policy: :any_sampled, fallback: Samplers::ALWAYS_ON)
            raise ArgumentError, "unsupported link sampling policy: #{policy.inspect}" unless POLICIES.key?(policy)

            @policy = policy
            @fallback = fallback
          end

          def ==(other)
            other.is_a?(LinkBased) && @policy == other.policy && @fallback == other.fallback
          end

          # @api private
          #
          # See {Samplers}.
          def description
            "LinkBased{policy=#{@policy}, fallback=#{@fallback.description}}"
          end

          # @api private
          #
          # See {Samplers}.
          def should_sample?(trace_id:, parent_context:, links:, name:, kind:, attributes:)
            parent_span_context = OpenTelemetry::Trace.current_span(parent_context).context
            return fallback_result(trace_id, parent_context, links, name, kind, attributes) if parent_span_context.valid?

            remote_links = remote_link_contexts(links)
            return fallback_result(trace_id, parent_context, links, name, kind, attributes) if remote_links.empty?

            decision, reason = decide(remote_links)
            Result.new(
              decision: decision,
              tracestate: parent_span_context.tracestate,
              attributes: { SAMPLING_REASON_ATTRIBUTE => reason }.freeze
            )
          end

          protected

          attr_reader :policy, :fallback

          private

          def remote_link_contexts(links)
            return [] if links.nil?

            links.map(&:span_context).select { |span_context| span_context.valid? && span_context.remote? }
          end

          def decide(remote_links)
            sampled_count = remote_links.count { |span_context| span_context.trace_flags.sampled? }
            case @policy
            when :any_sampled
              any_sampled_decision(remote_links.size, sampled_count)
            else
              all_sampled_decision(remote_links.size, sampled_count)
            end
          end

          def any_sampled_decision(total, sampled_count)
            if sampled_count.positive?
              [Decision::RECORD_AND_SAMPLE, 'LinkBased: any_sampled policy matched a sampled remote link']
            else
              [Decision::DROP, 'LinkBased: any_sampled policy found no sampled remote link']
            end
          end

          def all_sampled_decision(total, sampled_count)
            if sampled_count == total
              [Decision::RECORD_AND_SAMPLE, 'LinkBased: all_sampled policy matched all sampled remote links']
            else
              [Decision::DROP, 'LinkBased: all_sampled policy found a not sampled remote link']
            end
          end

          def fallback_result(trace_id, parent_context, links, name, kind, attributes)
            @fallback.should_sample?(
              trace_id: trace_id,
              parent_context: parent_context,
              links: links,
              name: name,
              kind: kind,
              attributes: attributes
            )
          end
        end
      end
    end
  end
end
