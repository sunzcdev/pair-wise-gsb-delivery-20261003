# frozen_string_literal: true

require "spec_helper"
require "monitor"

describe "Bunny server-named queue declaration timeout isolation" do
  class IsolationConnStub
    attr_accessor :write_timeout
    attr_reader :recorded

    def initialize
      @recorded = []
      @topology = Bunny::TopologyRegistry.new
      @write_timeout = 2
      @logger = Logger.new(IO::NULL)
    end

    attr_reader :topology

    def logger
      @logger
    end

    def mutex_impl
      Monitor
    end

    def threaded
      true
    end

    def continuation_timeout
      5_000
    end

    def transport_write_timeout
      @write_timeout
    end

    def register_channel(_channel); end

    def send_frame(_frame); end

    def record_queue_with(*args)
      @topology.record_queue_with(*args)
      @recorded << args
    end

    def server_has_capability?(_name)
      false
    end
  end

  let(:connection) { IsolationConnStub.new }
  let(:channel) do
    Bunny::Channel.new(connection, 1).tap { |ch| ch.instance_variable_set(:@status, :open) }
  end

  def declare_ok(name)
    method = AMQ::Protocol::Queue::DeclareOk.allocate
    method.define_singleton_method(:queue) { name }
    method
  end

  def wait_until(timeout: 2, &block)
    deadline = Time.now + timeout
    loop do
      value = block.call
      return value if value

      raise "condition was not met" if Time.now > deadline

      sleep 0.01
    end
  end

  it "does not give a late declare-ok to a later declaration and keeps other RPCs usable" do
    connection.write_timeout = 0.05
    expect { channel.queue("") }.to raise_error(Bunny::ClientTimeout)

    connection.write_timeout = 2

    queue = nil
    declare_thread = Thread.new { queue = channel.queue("") }

    wait_until { channel.instance_variable_get(:@queue_declare_attempts).size == 2 }

    qos = nil
    qos_thread = Thread.new { qos = channel.basic_qos(10) }

    channel.handle_method(declare_ok("old-generated-name"))

    wait_until { channel.late_queue_declare_oks.size == 1 }
    late = channel.late_queue_declare_oks.pop
    expect(late.queue).to eq("old-generated-name")

    channel.handle_method(AMQ::Protocol::Basic::QosOk.allocate)
    wait_until { qos }
    qos_thread.join(1)

    channel.handle_method(declare_ok("new-generated-name"))
    declare_thread.join(2)

    expect(declare_thread).not_to be_alive
    expect(queue.name).to eq("new-generated-name")
    expect(qos).to be_a(AMQ::Protocol::Basic::QosOk)
    expect(channel.find_queue("new-generated-name")).to eq(queue)
    expect(channel.find_queue("old-generated-name")).to be_nil
    expect(connection.recorded.map { |args| args[1] }).to eq(["new-generated-name"])
    expect(connection.topology.filtered_queues.map(&:name)).to eq(["new-generated-name"])
    expect(channel).to be_open
    expect(channel.instance_variable_get(:@continuations)).to be_empty
  end

  it "keeps a response that arrives before the retry out of the shared continuation queue" do
    connection.write_timeout = 0.05
    expect { channel.queue_declare("") }.to raise_error(Bunny::ClientTimeout)
    connection.write_timeout = 2

    channel.handle_method(declare_ok("old-generated-name"))
    expect(channel.late_queue_declare_oks.pop.queue).to eq("old-generated-name")

    qos = AMQ::Protocol::Basic::QosOk.allocate
    channel.handle_method(qos)

    result = nil
    thread = Thread.new { result = channel.queue_declare("") }
    wait_until { channel.instance_variable_get(:@queue_declare_attempts).size == 1 }

    channel.handle_method(declare_ok("new-generated-name"))
    thread.join(2)

    expect(result.queue).to eq("new-generated-name")
    expect(channel.instance_variable_get(:@continuations).pop).to eq(qos)
  end
end
