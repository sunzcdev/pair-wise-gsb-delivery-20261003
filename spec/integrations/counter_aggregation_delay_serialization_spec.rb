require "spec_helper"

describe "Counter aggregation with delayed serialization" do
  it "merges counters serialized lazily in the message buffer" do
    socket = FakeUDPSocket.new(copy_message: true)
    allow(UDPSocket).to receive(:new).and_return(socket)
    dogstats = Datadog::Statsd.new("localhost", 1234,
      delay_serialization: true,
      origin_detection: false,
      telemetry_enable: false,
      single_thread: true,
      counter_aggregation: true,
    )

    dogstats.increment("boo", tags: {tag1: "val1"})
    dogstats.count("boo", 4, tags: {tag1: "val1"})
    dogstats.gauge("boo", 9)
    dogstats.flush(sync: true)

    expect(socket.recv[0]).to eq("boo:5|c|#tag1:val1\nboo:9|g")
  end
end
