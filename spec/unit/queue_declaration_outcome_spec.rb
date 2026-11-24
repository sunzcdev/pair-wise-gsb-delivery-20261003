# frozen_string_literal: true

require "spec_helper"

describe Bunny::QueueDeclaration do
  let(:ok) { AMQ::Protocol::Queue::DeclareOk.new("amq.gen-current", 0, 0) }

  def allocated_channel(id)
    channel = Bunny::Channel.allocate
    channel.instance_variable_set(:@id, id)
    channel.instance_variable_set(:@frame_max, 4096)
    channel.instance_variable_set(:@logger, Logger.new(IO::NULL))
    channel.instance_variable_set(:@status, :open)
    channel.define_singleton_method(:raise_if_no_longer_open!) { nil }
    channel.define_singleton_method(:raise_if_continuation_resulted_in_a_channel_error!) { nil }
    channel.define_singleton_method(:with_continuation_timeout) { |&block| block.call }

    recorded = []
    channel.define_singleton_method(:record_queue_with) do |_ch, name, *_args|
      recorded << name
    end
    channel.instance_variable_set(:@recorded_queue_names, recorded)
    channel
  end

  it "distinguishes definite failure, unknown server-generated name, and success" do
    channel = allocated_channel(1)
    ok_response = ok

    channel.define_singleton_method(:queue_declare_without_recording_topology) do |*_args|
      raise Bunny::NotFound.new("queue declare rejected", channel, nil)
    end
    failed = channel.declare_queue("")
    expect(failed).to be_definite_failure
    expect(failed).not_to be_name_unknown
    expect(failed).not_to be_success

    channel.define_singleton_method(:queue_declare_without_recording_topology) do |*_args|
      raise Timeout::Error, "declare-ok was lost"
    end
    unknown = channel.declare_queue("")
    expect(unknown).to be_name_unknown
    expect(unknown.name).to be_nil
    expect(unknown).not_to be_name_known

    channel.define_singleton_method(:queue_declare_without_recording_topology) { |*_args| ok_response }
    success = channel.declare_queue("")
    expect(success).to be_success
    expect(success.name).to eq("amq.gen-current")
    expect(channel.instance_variable_get(:@recorded_queue_names)).to eq(["amq.gen-current"])

    channel.define_singleton_method(:queue_declare_without_recording_topology) do |*_args|
      raise Timeout::Error, "declare-ok was lost again"
    end
    expect { channel.declare_queue!("") }.to raise_error(Bunny::QueueDeclarationOutcomeUnknown)

    begin
      channel.queue_declare("")
    rescue Timeout::Error => error
      expect(error.queue_declaration).to be_name_unknown
    else
      raise "expected queue_declare to reraise the timeout"
    end
  end

  it "keeps a delayed reply on the old record without making it a recovery candidate" do
    old_channel = allocated_channel(1)
    other_channel = allocated_channel(2)
    reused_channel = allocated_channel(1)

    old_channel.define_singleton_method(:queue_declare_without_recording_topology) do |*_args|
      raise Timeout::Error, "connection lost before declare-ok"
    end
    old_declaration = old_channel.declare_queue("")
    expect(old_declaration).to be_name_unknown

    other_ok = AMQ::Protocol::Queue::DeclareOk.new("other-confirmed", 0, 0)
    other_channel.define_singleton_method(:queue_declare_without_recording_topology) { |*_a| other_ok }
    other_declaration = other_channel.declare_queue("")
    expect(other_declaration.name).to eq("other-confirmed")

    reused_ok = AMQ::Protocol::Queue::DeclareOk.new("reused-generation", 0, 0)
    reused_channel.define_singleton_method(:queue_declare_without_recording_topology) { |*_a| reused_ok }
    reused_declaration = reused_channel.declare_queue("")
    expect(reused_declaration.name).to eq("reused-generation")

    old_reply = AMQ::Protocol::Queue::DeclareOk.new("late-old-generation", 0, 0)
    old_declaration.deliver_reply!(old_reply)
    second_late_reply = AMQ::Protocol::Queue::DeclareOk.new("late-old-frame", 0, 0)
    old_channel.handle_method(second_late_reply)

    expect(old_declaration).to be_name_unknown
    expect(old_declaration.name).to be_nil
    expect(old_declaration).to be_late
    expect(old_declaration.late_name).to eq("late-old-frame")
    expect(reused_declaration).to be_success
    expect(reused_declaration.name).to eq("reused-generation")

    registry = Bunny::TopologyRegistry.new
    registry.record_queue_with(other_channel, "other-confirmed", true, false, false, false, nil)
    registry.record_queue_with(reused_channel, "reused-generation", true, false, false, false, nil)
    registry.record_queue_with(reused_channel, "unconfirmed-record", false, false, false, false, nil, confirmed: false)

    expect(registry.filtered_queues.map(&:name)).to contain_exactly("other-confirmed", "reused-generation")
    expect(registry.queue_declarations_for(other_channel).map(&:token)).to eq([])
  end

  it "scopes server-named queue propagation to the owning channel" do
    registry = Bunny::TopologyRegistry.new
    first = allocated_channel(7)
    second = allocated_channel(8)
    callable = proc { |*_args| nil }

    registry.record_queue_with(first, "generated-name", true, false, true, false, nil)
    registry.record_consumer_with(first, "ctag-first", "generated-name", callable, true, true, nil)
    registry.record_consumer_with(second, "ctag-second", "generated-name", callable, true, true, nil)

    registry.record_queue_name_change("generated-name", "actual-name")

    expect(registry.consumers["ctag-first"].queue_name).to eq("actual-name")
    expect(registry.consumers["ctag-second"].queue_name).to eq("generated-name")
  end

  it "recovers only recorded confirmed queue names for the selected topology" do
    session = Bunny::Session.allocate
    session.instance_variable_set(:@topology_registry, Bunny::TopologyRegistry.new)
    session.instance_variable_set(:@logger, Logger.new(IO::NULL))

    channel = allocated_channel(3)
    session.topology_registry.record_queue_with(channel, "confirmed-name", false, true, false, false, nil)

    recovered = []
    session.define_singleton_method(:recover_queue) { |q| recovered << q.name }
    session.define_singleton_method(:recover_exchange) { |_x| nil }
    session.define_singleton_method(:recover_queue_binding) { |_b| nil }
    session.define_singleton_method(:recover_exchange_binding) { |_b| nil }
    session.define_singleton_method(:recover_consumer) { |_c| nil }

    session.recover_topology_with
    expect(recovered).to eq(["confirmed-name"])
  end
end
