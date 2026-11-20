# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'

describe OpenTelemetry::SDK::Trace::Samplers::LinkBased do
  Samplers = OpenTelemetry::SDK::Trace::Samplers
  Decision = OpenTelemetry::SDK::Trace::Samplers::Decision
  Link = OpenTelemetry::Trace::Link
  TraceFlags = OpenTelemetry::Trace::TraceFlags

  let(:trace_id) { OpenTelemetry::Trace.generate_trace_id }
  let(:sampled_flags) { TraceFlags.from_byte(1) }
  let(:not_sampled_flags) { TraceFlags.from_byte(0) }
  let(:root_context) { OpenTelemetry::Context::ROOT }

  let(:remote_sampled_link) do
    Link.new(OpenTelemetry::Trace::SpanContext.new(trace_id: trace_id, remote: true, trace_flags: sampled_flags))
  end
  let(:remote_not_sampled_link) do
    Link.new(OpenTelemetry::Trace::SpanContext.new(trace_id: trace_id, remote: true, trace_flags: not_sampled_flags))
  end
  let(:local_sampled_link) do
    Link.new(OpenTelemetry::Trace::SpanContext.new(trace_id: trace_id, remote: false, trace_flags: sampled_flags))
  end
  let(:local_not_sampled_link) do
    Link.new(OpenTelemetry::Trace::SpanContext.new(trace_id: trace_id, remote: false, trace_flags: not_sampled_flags))
  end

  let(:sampled_parent_context) do
    span_context = OpenTelemetry::Trace::SpanContext.new(trace_id: trace_id, trace_flags: sampled_flags)
    OpenTelemetry::Trace.context_with_span(OpenTelemetry::Trace.non_recording_span(span_context))
  end
  let(:not_sampled_parent_context) do
    span_context = OpenTelemetry::Trace::SpanContext.new(trace_id: trace_id, trace_flags: not_sampled_flags)
    OpenTelemetry::Trace.context_with_span(OpenTelemetry::Trace.non_recording_span(span_context))
  end

  describe '.link_based' do
    it 'returns a LinkBased sampler' do
      sampler = Samplers.link_based
      _(sampler).must_be_instance_of(Samplers::LinkBased)
    end

    it 'rejects unknown policies' do
      _(proc { Samplers.link_based(policy: :sometimes) }).must_raise(ArgumentError)
    end
  end

  describe '#description' do
    it 'describes the default policy and fallback sampler' do
      sampler = Samplers.link_based
      _(sampler.description).must_equal('LinkBased{policy=any_sampled, fallback=AlwaysOnSampler}')
    end

    it 'describes a custom policy and fallback sampler' do
      sampler = Samplers.link_based(policy: :all_sampled, fallback: Samplers::ALWAYS_OFF)
      _(sampler.description).must_equal('LinkBased{policy=all_sampled, fallback=AlwaysOffSampler}')
    end
  end

  describe '#==' do
    it 'is equal to another sampler with the same policy and fallback' do
      _(Samplers.link_based(policy: :all_sampled)).must_equal(Samplers.link_based(policy: :all_sampled))
    end

    it 'is not equal when policies differ' do
      _(Samplers.link_based).wont_equal(Samplers.link_based(policy: :all_sampled))
    end

    it 'is not equal when fallbacks differ' do
      _(Samplers.link_based).wont_equal(Samplers.link_based(fallback: Samplers::ALWAYS_OFF))
    end
  end

  describe 'default any_sampled policy' do
    subject { Samplers.link_based }

    it 'samples when any remote link is sampled' do
      result = call_sampler?(subject, links: [remote_not_sampled_link, remote_sampled_link])
      _(result).must_be :sampled?
      _(result.attributes[Samplers::LinkBased::SAMPLING_REASON_ATTRIBUTE])
        .must_equal('LinkBased: any_sampled policy matched a sampled remote link')
    end

    it 'does not sample when no remote link is sampled' do
      result = call_sampler?(subject, links: [remote_not_sampled_link, local_sampled_link])
      _(result).wont_be :sampled?
      _(result.attributes[Samplers::LinkBased::SAMPLING_REASON_ATTRIBUTE])
        .must_equal('LinkBased: any_sampled policy found no sampled remote link')
    end

    it 'ignores local links when no remote links are present and delegates to the fallback' do
      result = call_sampler?(subject, links: [local_sampled_link])
      _(result).must_be :sampled?
      _(result.attributes).wont_include(Samplers::LinkBased::SAMPLING_REASON_ATTRIBUTE)
    end

    it 'drops when there are only not-sampled remote links' do
      result = call_sampler?(subject, links: [remote_not_sampled_link])
      _(result).wont_be :sampled?
      _(result).wont_be :recording?
    end
  end

  describe 'all_sampled policy' do
    subject { Samplers.link_based(policy: :all_sampled) }

    it 'samples when every remote link is sampled' do
      result = call_sampler?(subject, links: [remote_sampled_link, remote_sampled_link])
      _(result).must_be :sampled?
      _(result.attributes[Samplers::LinkBased::SAMPLING_REASON_ATTRIBUTE])
        .must_equal('LinkBased: all_sampled policy matched all sampled remote links')
    end

    it 'does not sample when a remote link is not sampled' do
      result = call_sampler?(subject, links: [remote_sampled_link, remote_not_sampled_link])
      _(result).wont_be :sampled?
      _(result.attributes[Samplers::LinkBased::SAMPLING_REASON_ATTRIBUTE])
        .must_equal('LinkBased: all_sampled policy found a not sampled remote link')
    end

    it 'ignores local links when evaluating remote links' do
      result = call_sampler?(subject, links: [remote_sampled_link, local_not_sampled_link])
      _(result).must_be :sampled?
    end
  end

  describe 'fallback delegation' do
    subject { Samplers.link_based }

    it 'delegates to the fallback sampler when links are nil' do
      on_sampler = Samplers.link_based
      off_sampler = Samplers.link_based(fallback: Samplers::ALWAYS_OFF)
      _(call_sampler?(on_sampler, links: nil)).must_be :sampled?
      _(call_sampler?(off_sampler, links: nil)).wont_be :sampled?
    end

    it 'delegates to the fallback sampler when links are empty' do
      _(call_sampler?(subject, links: [])).must_be :sampled?
    end

    it 'does not attach a reason attribute when delegating' do
      result = call_sampler?(Samplers.link_based, links: nil)
      _(result.attributes).wont_include(Samplers::LinkBased::SAMPLING_REASON_ATTRIBUTE)
    end

    it 'passes the original arguments through to the fallback sampler' do
      mock_sampler = Minitest::Mock.new
      expected = Samplers::Result.new(decision: Decision::DROP, tracestate: nil)
      mock_sampler.expect(:should_sample?, expected, [], trace_id: trace_id, parent_context: root_context, links: nil, name: 'job', kind: nil, attributes: nil)
      sampler = Samplers.link_based(fallback: mock_sampler)
      call_sampler?(sampler, trace_id: trace_id, parent_context: root_context, links: nil, name: 'job')
      mock_sampler.verify
    end
  end

  describe 'parent spans' do
    subject { Samplers.link_based }

    it 'ignores links and delegates when a parent span is present' do
      result = call_sampler?(subject, parent_context: not_sampled_parent_context, links: [remote_sampled_link])
      _(result).must_be :sampled?
    end

    it 'composes with a parent_based sampler for not sampled parents' do
      sampler = Samplers.parent_based(root: subject)
      result = call_sampler?(sampler, parent_context: not_sampled_parent_context, links: [remote_sampled_link])
      _(result).wont_be :sampled?
    end

    it 'composes with a parent_based sampler for sampled parents' do
      sampler = Samplers.parent_based(root: subject)
      result = call_sampler?(sampler, parent_context: sampled_parent_context, links: [remote_not_sampled_link])
      _(result).must_be :sampled?
    end

    it 'composes with a parent_based sampler for root spans with links' do
      sampler = Samplers.parent_based(root: subject)
      result = call_sampler?(sampler, parent_context: root_context, links: [remote_sampled_link])
      _(result).must_be :sampled?
      _(result.attributes[Samplers::LinkBased::SAMPLING_REASON_ATTRIBUTE]).wont_be_nil
    end
  end

  describe 'link ordering' do
    let(:links) { [remote_sampled_link, remote_not_sampled_link, local_sampled_link] }

    it 'makes the same decision regardless of link order' do
      sampled_decisions = 10.times.map do
        call_sampler?(Samplers.link_based, links: links.shuffle).sampled?
      end
      _(sampled_decisions.uniq).must_equal([true])
    end

    it 'reports the same reason regardless of link order' do
      reasons = 10.times.map do
        call_sampler?(Samplers.link_based, links: links.shuffle)
          .attributes[Samplers::LinkBased::SAMPLING_REASON_ATTRIBUTE]
      end
      _(reasons.uniq.size).must_equal(1)
    end

    it 'makes the same all_sampled decision regardless of link order' do
      sampled_decisions = 10.times.map do
        call_sampler?(Samplers.link_based(policy: :all_sampled), links: links.shuffle).sampled?
      end
      _(sampled_decisions.uniq).must_equal([false])
    end
  end

  describe 'propagated trace flags' do
    let(:provider) { OpenTelemetry::SDK::Trace::TracerProvider.new(sampler: Samplers.parent_based(root: Samplers.link_based)) }
    let(:tracer) { provider.tracer('test') }
    let(:links) { [remote_not_sampled_link, remote_sampled_link] }

    it 'propagates the sampled flag consistently regardless of link order' do
      flags = 10.times.map do
        span = tracer.start_root_span('job', links: links.shuffle)
        span.context.trace_flags.sampled?
      ensure
        span&.finish
      end
      _(flags.uniq).must_equal([true])
    end

    it 'propagates a not sampled flag when no remote link is sampled' do
      span = tracer.start_root_span('job', links: [remote_not_sampled_link])
      _(span.context.trace_flags.sampled?).must_equal(false)
    ensure
      span&.finish
    end
  end

  def call_sampler?(sampler, trace_id: nil, parent_context: OpenTelemetry::Context.current, links: nil, name: nil, kind: nil, attributes: nil)
    sampler.should_sample?(
      trace_id: trace_id || OpenTelemetry::Trace.generate_trace_id,
      parent_context: parent_context,
      links: links,
      name: name,
      kind: kind,
      attributes: attributes
    )
  end
end
