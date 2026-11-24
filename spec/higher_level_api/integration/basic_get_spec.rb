require "spec_helper"

describe Bunny::Queue, "#pop" do
  let(:connection) do
    c = Bunny.new(username: "bunny_gem", password: "bunny_password", vhost: "bunny_testbed",
                  automatically_recover: false)
    c.start
    c
  end

  after :each do
    connection.close if connection.open?
  end

  context "with all defaults" do
    it "fetches a messages which is automatically acknowledged" do
      ch = connection.create_channel

      q  = ch.queue("", exclusive: true)
      x  = ch.default_exchange

      msg = "xyzzy"
      x.publish(msg, routing_key: q.name)

      sleep(0.5)
      get_ok, properties, content = q.pop
      expect(get_ok).to be_kind_of(Bunny::GetResponse)
      expect(properties).to be_kind_of(Bunny::MessageProperties)
      expect(properties.content_type).to eq("application/octet-stream")
      expect(get_ok.routing_key).to eq(q.name)
      expect(get_ok.delivery_tag).to be_kind_of(Integer)
      expect(content).to eq(msg)
      expect(q.message_count).to eq 0

      ch.close
    end
  end


  context "with an empty queue" do
    it "returns an empty response" do
      ch = connection.create_channel

      q  = ch.queue("", exclusive: true)
      q.purge

      get_empty, properties, content = q.pop
      expect(get_empty).to eq(nil)
      expect(properties).to eq(nil)
      expect(content).to eq(nil)
      expect(q.message_count).to eq 0

      ch.close
    end
  end
end


describe Bunny::Channel, "#basic_get" do
  let(:connection) do
    c = Bunny.new(username: "bunny_gem", password: "bunny_password", vhost: "bunny_testbed",
                  automatically_recover: false, continuation_timeout: 3000)
    c.start
    c
  end

  after :each do
    connection.close if connection.open?
  end

  context "with a non-existent queue" do
    it "throws a NOT_FOUND" do
      ch = connection.create_channel

      expect do
        ch.basic_get "non_existent_#{rand.to_s}"
      end.to raise_error(Bunny::NotFound)
    end
  end

  context "with a consumer on the same channel" do
    let(:ch) { connection.create_channel }
    let(:queue_name) { "bunny.basic_get.concurrent#{rand}" }

    after do
      ch.close if ch.open?
    end
    it "keeps pushed deliveries and synchronous gets apart" do
      q = ch.queue(queue_name, exclusive: true)
      pushed = Queue.new
      q.subscribe(manual_ack: true) do |_info, _properties, payload|
        pushed << payload
      end
      sleep 0.5

      q.publish("pushed-1")
      q.publish("pushed-2")
      expect(pushed.pop).to eq("pushed-1")
      expect(pushed.pop).to eq("pushed-2")

      q.publish("got-1")
      sleep 0.5
      get_ok, _properties, content = q.pop(manual_ack: true)
      expect(get_ok).to be_kind_of(Bunny::GetResponse)
      expect(content).to eq("got-1")
      ch.ack(get_ok.delivery_tag)

      expect(q.pop).to eq([nil, nil, nil])
      expect(pushed).to be_empty
    end

    it "delivers a message buffered while consumer callbacks were paused after resume" do
      q = ch.queue(queue_name, exclusive: true)
      pushed = Queue.new
      q.subscribe(manual_ack: false) do |_info, _properties, payload|
        pushed << payload
      end
      sleep 0.5

      ch.maybe_pause_consumer_work_pool!
      q.publish("while-paused")
      sleep 0.5
      expect(pushed).to be_empty

      # an empty get still completes while the consumer pool is paused
      expect(q.pop).to eq([nil, nil, nil])

      ch.work_pool.resume
      expect(pushed.pop).to eq("while-paused")
    end

    it "raises a client timeout when the get cannot complete and the channel stays usable" do
      q = ch.queue(queue_name, exclusive: true)
      q.purge

      timed_out_connection = Bunny.new(username: "bunny_gem", password: "bunny_password",
                                       vhost: "bunny_testbed", automatically_recover: false,
                                       continuation_timeout: 200)
      timed_out_connection.start
      timed_out_channel = timed_out_connection.create_channel
      timed_out_queue = timed_out_channel.queue("", exclusive: true)

      expect do
        timed_out_channel.basic_get(timed_out_queue.name)
      end.to raise_error(Timeout::Error)

      timed_out_queue.publish("after-timeout")
      get_ok, _properties, content = timed_out_channel.basic_get(timed_out_queue.name)
      expect(get_ok).to be_kind_of(Bunny::GetResponse)
      expect(content).to eq("after-timeout")
      timed_out_connection.close
    end

    it "unblocks a pending get when the channel is closed" do
      q = ch.queue(queue_name, exclusive: true)
      q.purge

      result = nil
      thread = Thread.new do
        begin
          ch.basic_get(q.name)
        rescue => e
          result = e
        end
      end
      sleep 0.2

      ch.close
      expect(thread.join(5)).to eq(thread)
      expect(result).to be_a(Bunny::ChannelAlreadyClosed)
    end

    it "unblocks a pending get when the server closes the channel" do
      q = ch.queue(queue_name, exclusive: true)

      result = nil
      thread = Thread.new do
        begin
          ch.basic_get(q.name)
        rescue => e
          result = e
        end
      end
      sleep 0.2

      ch.ack(999, false)
      expect(thread.join(5)).to eq(thread)
      expect(result).to be_a(Bunny::PreconditionFailed)
      expect(ch).to be_closed
    end
  end
end
