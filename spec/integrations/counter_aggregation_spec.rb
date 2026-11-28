# frozen_string_literal: true

require 'spec_helper'

RSpec.shared_examples 'Counter aggregation integration testing' do |single_thread|
  subject do
    Datadog::Statsd.new('localhost', 1234,
      telemetry_enable: false,
      origin_detection: false,
      single_thread: single_thread,
      counter_aggregation: true,
    )
  end

  let(:socket) { FakeUDPSocket.new(copy_message: true) }

  before do
    allow(Socket).to receive(:new).and_return(socket)
    allow(UDPSocket).to receive(:new).and_return(socket)
  end

  it 'only merges counters sharing the same name, tag set and sample rate' do
    subject.batch do |s|
      s.increment('page.views', tags: ['env:prod'], pre_sampled: true, sample_rate: 1)
      s.increment('page.views', tags: ['env:prod'], pre_sampled: true, sample_rate: 0.5)
      s.increment('page.views', tags: ['env:staging'], pre_sampled: true, sample_rate: 1)
      s.gauge('page.views', 42)
      s.count('page.views', 3, tags: ['env:prod'], pre_sampled: true, sample_rate: 1)
      s.count('page.views', 2, tags: ['env:prod'], pre_sampled: true, sample_rate: 0.5)
      s.increment('page.views', tags: ['env:staging'], pre_sampled: true, sample_rate: 1)
      s.histogram('page.views', 9)
    end

    packet = socket.recv[0]

    lines = packet.split("\n")

    # three counter groups (prod at two sample rates + staging at rate 1),
    # the gauge and the histogram are never part of the aggregation
    expect(lines.size).to eq(5)

    counters = lines.select { |line| line.end_with?('|c') || line.include?('|c|') }
    expect(counters).to match_array([
      'page.views:4|c|#env:prod',
      'page.views:3|c|@0.5|#env:prod',
      'page.views:2|c|#env:staging',
    ])

    # the two sample rates of the env:prod group stay separated
    prod_lines = counters.select { |line| line.include?('#env:prod') }
    expect(prod_lines).to contain_exactly(
      'page.views:4|c|#env:prod',
      'page.views:3|c|@0.5|#env:prod',
    )

    # the same-named gauge keeps its own, single value
    expect(lines).to include('page.views:42|g')
    # other metric types are sent untouched
    expect(lines).to include('page.views:9|h')
  end

  it 'renders each merged group as a single wire message with the summed count' do
    subject.batch do |s|
      s.count('requests', 1, tags: ['route:/a'])
      s.count('requests', 4, tags: ['route:/a'])
      s.count('requests', 5, tags: ['route:/b'])
    end

    packet = socket.recv[0]

    expect(packet).to eq(
      "requests:5|c|#route:/a\n" \
      "requests:5|c|#route:/b"
    )
  end

  it 'does not aggregate counters when the option is not enabled' do
    statsd = Datadog::Statsd.new('localhost', 1234,
      telemetry_enable: false,
      origin_detection: false,
      single_thread: single_thread,
    )

    statsd.batch do |s|
      s.increment('requests')
      s.increment('requests')
    end

    packet = socket.recv[0]

    expect(packet).to eq("requests:1|c\nrequests:1|c")
  end
end

describe 'Counter aggregation in single threaded mode' do
  it_behaves_like 'Counter aggregation integration testing', "true"
end

describe 'Counter aggregation in multi threaded mode' do
  it_behaves_like 'Counter aggregation integration testing', "false"
end
