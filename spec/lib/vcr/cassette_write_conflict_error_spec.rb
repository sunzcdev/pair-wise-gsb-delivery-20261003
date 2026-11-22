require 'spec_helper'

RSpec.describe VCR::Errors::CassetteWriteConflictError do
  it 'exposes the cassette, rejected-file and structured conflict details' do
    conflicts = [{
      request_summary: '[get http://example.com/shared]',
      kept_response_summary: '[200 "first"]',
      rejected_response_summary: '[200 "second"]'
    }]

    error = described_class.new(
      cassette_file: '/tmp/cassettes/shared.yml',
      rejected_interactions_file: '/tmp/cassettes/shared.rejected-1.yml',
      conflicts: conflicts
    )

    expect(error.cassette_file).to eq('/tmp/cassettes/shared.yml')
    expect(error.rejected_interactions_file).to eq('/tmp/cassettes/shared.rejected-1.yml')
    expect(error.conflicts).to eq(conflicts)
  end

  it 'builds a diagnostic message naming both responses and the rejected file' do
    error = described_class.new(
      cassette_file: '/tmp/cassettes/shared.yml',
      rejected_interactions_file: '/tmp/cassettes/shared.rejected-1.yml',
      conflicts: [{
        request_summary: '[get http://example.com/shared]',
        kept_response_summary: '[200 "first"]',
        rejected_response_summary: '[200 "second"]'
      }]
    )

    message = error.message
    expect(message).to include("VCR refused to overwrite '/tmp/cassettes/shared.yml'")
    expect(message).to include('1 conflicting request(s)')
    expect(message).to include('[get http://example.com/shared]')
    expect(message).to include('kept response:')
    expect(message).to include('[200 "first"]')
    expect(message).to include('rejected response:')
    expect(message).to include('[200 "second"]')
    expect(message).to include('/tmp/cassettes/shared.rejected-1.yml')
  end

  it 'is a VCR error' do
    expect(described_class.superclass).to eq(VCR::Errors::Error)
  end
end
