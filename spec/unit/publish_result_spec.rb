# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"

describe Bunny::Channel do
  # Publish result tracking without a running RabbitMQ node: the session
  # stands in for the broker. Its wire-level methods are stubbed and
  # basic.return/basic.ack are injected straight into the channel.

  let(:logger) { Logger.new(IO::NULL) }

  let(:session) do
    Bunny.new({
      host: "127.0.0.1",
      port: 65_534,
      logger: logger,
      automatically_recover: false
    })
  end

  let(:channel) do
    allow(session).to receive(:open_channel)
    allow(session).to receive(:frame_max).and_return(131_072)
    allow(session).to receive(:transport_write_timeout).and_return(15)
    allow(session).to receive(:send_frameset)

    ch = Bunny::Channel.new(session, 1)
    ch.open
    ch
  end

  def confirm_select(channel, *args, **opts)
    allow(session).to receive(:send_frame) do |*_frame_args|
      channel.handle_method(AMQ::Protocol::Confirm::SelectOk.new)
    end
    channel.confirm_select(*args, **opts)
  end

  describe "#basic_publish with publish result tracking" do
    it "correlates a basic.return and a summary ack to publishes by sequence number, not payload" do
      confirm_select(channel, publish_results: true)

      # identical payloads on the same channel, only one of them routes
      routed   = channel.basic_publish("same body", "amq.direct", "routable.key",   mandatory: true)
      unrouted = channel.basic_publish("same body", "amq.direct", "unroutable.key", mandatory: true)

      expect(routed.sequence_number).to eq 1
      expect(unrouted.sequence_number).to eq 2
      expect(routed).not_to be_confirmed
      expect(unrouted).not_to be_confirmed

      # the stand-in broker returns the unroutable one, then confirms both with one ack
      channel.handle_basic_return(
        AMQ::Protocol::Basic::Return.new(312, "NO_ROUTE", "amq.direct", "unroutable.key"),
        {},
        "same body"
      )
      channel.handle_method(AMQ::Protocol::Basic::Ack.new(2, true))

      expect(routed).to be_acked
      expect(routed).not_to be_returned
      expect(routed.return_info).to be_nil

      expect(unrouted).to be_acked
      expect(unrouted).to be_returned
      expect(unrouted.return_info.reply_code).to eq 312
      expect(unrouted.return_info.reply_text).to eq "NO_ROUTE"
    end

    it "still invokes the plain confirm callback for every confirmed tag" do
      confirms = []
      confirm_select(channel, ->(tag, multiple, nack) { confirms << [tag, multiple, nack] }, publish_results: true)

      channel.basic_publish("same body", "amq.direct", "routable.key",   mandatory: true)
      channel.basic_publish("same body", "amq.direct", "unroutable.key", mandatory: true)

      channel.handle_basic_return(
        AMQ::Protocol::Basic::Return.new(312, "NO_ROUTE", "amq.direct", "unroutable.key"),
        {},
        "same body"
      )
      channel.handle_method(AMQ::Protocol::Basic::Ack.new(2, true))

      expect(confirms).to eq [[1, false, false], [2, false, false]]
    end

    it "marks results as nacked when the broker nacks" do
      confirm_select(channel, publish_results: true)

      result = channel.basic_publish("same body", "amq.direct", "routable.key", mandatory: true)
      channel.handle_method(AMQ::Protocol::Basic::Nack.new(1, false, false))

      expect(result).to be_confirmed
      expect(result).to be_nacked
      expect(result).not_to be_returned
    end

    it "ignores a basic.return that matches no pending publish" do
      confirm_select(channel, publish_results: true)

      channel.basic_publish("same body", "amq.direct", "routable.key", mandatory: true)

      expect do
        channel.handle_basic_return(
          AMQ::Protocol::Basic::Return.new(312, "NO_ROUTE", "amq.direct", "some.other.key"),
          {},
          "same body"
        )
      end.not_to raise_error
    end

    it "returns self when publish result tracking is not enabled" do
      confirm_select(channel)

      expect(channel.basic_publish("same body", "amq.direct", "routable.key", mandatory: true)).to eq channel
    end
  end
end
