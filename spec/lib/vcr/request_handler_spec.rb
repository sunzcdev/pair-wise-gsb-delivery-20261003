require 'rspec'
require 'vcr'
require 'vcr/request_handler'

# Coverage for the playback consumption protocol in VCR::RequestHandler.
# Deliberately does not use spec_helper, which needs HTTP libraries that are
# not part of every environment; the handler only needs the interaction list.
RSpec.describe VCR::RequestHandler do
  module DeliverySpecHandler
    class Handler < VCR::RequestHandler
      class << self
        attr_accessor :response, :error
      end

      def vcr_request
        @vcr_request ||= VCR::Request.new(:get, 'http://example.com/reserved')
      end

      private

      def on_stubbed_by_vcr_request
        raise self.class.error if self.class.error
        self.class.response
      end

      def on_recordable_request
        raise self.class.error if self.class.error
        'real response'
      end

      def request_summary
        'GET http://example.com/reserved'
      end

      def log_prefix
        '[delivery-spec] '
      end
    end
  end

  def reset_vcr!
    VCR.instance_variables.each { |ivar| VCR.instance_variable_set(ivar, nil) }
    VCR.send(:initialize_ivars)
    VCR.turn_on!
  end

  let(:interaction) do
    VCR::HTTPInteraction.new(
      VCR::Request.new(:get, 'http://example.com/reserved'),
      VCR::Response.new(VCR::ResponseStatus.new(200, 'OK'), {}, 'body', '1.1')
    )
  end
  let(:list) { VCR::Cassette::HTTPInteractionList.new([interaction], [:method, :uri]) }
  let(:handler) { DeliverySpecHandler::Handler.new }

  before do
    reset_vcr!
    allow(VCR).to receive(:http_interactions).and_return(list)
    DeliverySpecHandler::Handler.response = nil
    DeliverySpecHandler::Handler.error = nil
  end

  it 'consumes the matched interaction once the response is returned' do
    DeliverySpecHandler::Handler.response = 'stubbed'

    expect(handler.handle).to eq('stubbed')
    expect(handler.playback).to be_committed
    expect(list.remaining_unused_interaction_count).to eq(0)
  end

  it 'returns the interaction when the handler raises before returning the response' do
    DeliverySpecHandler::Handler.error = 'boom'

    expect { handler.handle }.to raise_error('boom')
    expect(handler.playback).to be_released
    expect(list.remaining_unused_interaction_count).to eq(1)

    DeliverySpecHandler::Handler.error = nil
    DeliverySpecHandler::Handler.response = 'stubbed'

    expect(handler.handle).to eq('stubbed')
  end

  it 'leaves the reservation pending for handlers that defer the commit' do
    def handler.defer_playback_commit?
      true
    end
    DeliverySpecHandler::Handler.response = 'stubbed'

    expect(handler.handle).to eq('stubbed')
    expect(handler.playback).to be_pending
    expect(list.remaining_unused_interaction_count).to eq(1)

    handler.commit_playback
    expect(list.remaining_unused_interaction_count).to eq(0)
  end

  it 'exposes idempotent commit and release helpers' do
    DeliverySpecHandler::Handler.response = 'stubbed'
    handler.handle

    expect(handler.release_playback).to be(false)
    expect(handler.playback).to be_committed
  end
end
