require "spec_helper"

# Exercises Bunny::Channel#wait_for_confirms_until against a real broker.
# The nack scenario needs the channel to be closed or the message to be
# rejected by the broker, so nacks are simulated through the same method the
# reader loop uses (Bunny::Channel#handle_ack_or_nack) where a broker cannot
# produce one.
describe Bunny::Channel do
  let(:connection) do
    c = Bunny.new(username: "bunny_gem", password: "bunny_password", vhost: "bunny_testbed",
                  continuation_timeout: 10_000)
    c.start
    c
  end

  after :each do
    connection.close if connection.open?
  end

  describe "#wait_for_confirms_until" do
    it "waits only for messages published before the call" do
      ch = connection.create_channel
      ch.confirm_select
      q  = ch.queue("", exclusive: true)
      x  = ch.default_exchange

      2.times { x.publish("msg", routing_key: q.name) }

      # records the sequence number of the last message published so far
      first_batch = ch.last_publish_seq_no
      expect(first_batch).to eq 2

      expect(ch.wait_for_confirms_until(first_batch, timeout: 5_000)).to eq true

      # publishing more afterwards must not block a later call either
      x.publish("msg", routing_key: q.name)
      expect(ch.wait_for_confirms_until(ch.last_publish_seq_no, timeout: 5_000)).to eq true

      ch.close
    end

    it "returns immediately when no message has been published" do
      ch = connection.create_channel
      ch.confirm_select

      expect(ch.last_publish_seq_no).to eq 0
      expect(ch.wait_for_confirms_until).to eq true

      ch.close
    end

    it "raises MessageNacked for a nacked message" do
      ch = connection.create_channel
      ch.confirm_select
      q  = ch.queue("", exclusive: true)
      x  = ch.default_exchange

      2.times { x.publish("msg", routing_key: q.name) }
      ch.wait_for_confirms_until(2, timeout: 5_000)

      # simulate a nack of the second message, as the reader loop would deliver it
      ch.handle_ack_or_nack(2, false, true)

      expect { ch.wait_for_confirms_until(2, timeout: 1_000) }
        .to raise_error(Bunny::MessageNacked) { |e| expect(e.delivery_tag).to eq 2 }

      ch.close
    end

    it "keeps wait_for_confirms working as before" do
      ch = connection.create_channel
      ch.confirm_select
      q  = ch.queue("", exclusive: true)
      x  = ch.default_exchange

      10.times { x.publish("msg", routing_key: q.name) }

      expect(ch.wait_for_confirms).to eq true

      ch.close
    end

    it "finishes an earlier round while a later message is still unconfirmed" do
      ch = connection.create_channel
      ch.confirm_select
      q  = ch.queue("", exclusive: true)
      x  = ch.default_exchange

      2.times { x.publish("msg", routing_key: q.name) }
      first_round = ch.last_publish_seq_no

      # a third message published after the first round was captured
      x.publish("msg", routing_key: q.name)
      third = ch.last_publish_seq_no

      expect(ch.wait_for_confirms_until(first_round, timeout: 5_000)).to eq true
      expect(ch.wait_for_confirms_until(third, timeout: 5_000)).to eq true

      ch.close
    end
  end
end
