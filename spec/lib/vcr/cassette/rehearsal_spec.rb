require 'tmpdir'
require 'fileutils'
require 'vcr/cassette/rehearsal'
require 'vcr/cassette/http_interaction_list'
require 'vcr/request_matcher_registry'
require 'vcr/structs'

module VCR
  class Cassette
    ::RSpec.describe Rehearsal do
      before(:each) do
        # A null logger keeps the rehearsal quiet without replacing the whole
        # configuration (monkey patches and the URI parser stay real).
        allow(VCR.configuration).to receive(:logger).and_return(Logger::Null)
      end

      def request(uri = 'http://example.com/foo', method = :get)
        VCR::Request.new(method, uri, '', {})
      end

      def interaction(body, options = {})
        VCR::HTTPInteraction.new(
          VCR::Request.new(options.fetch(:method, :get), options.fetch(:uri, 'http://example.com/foo'), '', {}),
          VCR::Response.new(VCR::ResponseStatus.new(options.fetch(:code, 200), 'OK'), {}, body, '1.1'),
          Time.utc(2025, 1, 1)
        )
      end

      def list(interactions, repeats = false, parent = HTTPInteractionList::NullList, label = 'cassette')
        HTTPInteractionList.new(interactions, [:method, :uri], repeats, parent, '', label: label)
      end

      let(:the_request) { request }

      describe 'the acceptance sequence' do
        # A child cassette with one matching record and a parent cassette that
        # can be fallen back to, also with one matching record for the same
        # request. The child record is consumed for real first, then the same
        # request is rehearsed twice.
        let(:parent_interaction) { interaction('parent body', code: 503) }
        let(:child_interaction)  { interaction('child body', code: 202) }
        let(:parent) { list([parent_interaction], false, HTTPInteractionList::NullList, 'outer') }
        let(:child)  { list([child_interaction], false, parent, 'inner') }

        before { expect(child.response_for(the_request).status.code).to eq(202) }

        context 'when playback repeats are not allowed' do
          let(:transcript) { Rehearsal.new(child).rehearse_all([the_request, the_request]) }

          it 'uses the parent interaction for the first rehearsal' do
            result = transcript.results[0]
            expect(result).to be_available
            expect(result).to be_from_parent
            expect(result.source).to eq(:parent)
            expect(result.label).to eq('outer')
            expect(result.response.status.code).to eq(503)
          end

          it 'reports the second rehearsal as exhausted' do
            result = transcript.results[1]
            expect(result).to be_exhausted
            expect(result.source).to eq(:none)
            expect(result.response).to be_nil
          end

          it 'reports the outcome of each step in order' do
            expect(transcript.map(&:outcome)).to eq([:available, :exhausted])
            expect(transcript.available_count).to eq(1)
            expect(transcript.exhausted_count).to eq(1)
            expect(transcript).not_to be_all_available
          end

          it 'only hands out the parent interaction once' do
            expect(transcript.results.count { |r| r.source == :parent }).to eq(1)
          end

          it 'leaves the parent interaction available for the real request afterwards' do
            transcript
            expect(parent.remaining_unused_interaction_count).to eq(1)
            expect(child.response_for(the_request).status.code).to eq(503)
          end

          it 'exposes the virtual remaining count after the rehearsal' do
            expect(transcript.initial_remaining_unused_interaction_count).to eq(1)
            expect(transcript.remaining_unused_interaction_count).to eq(0)
            expect(transcript.virtual_remaining).to eq(0)
          end
        end

        context 'when the parent allows playback repeats' do
          let(:parent) { list([parent_interaction], true, HTTPInteractionList::NullList, 'outer') }
          let(:transcript) { Rehearsal.new(child).rehearse_all([the_request, the_request]) }

          it 'lets both rehearsals pass through the parent' do
            expect(transcript.map(&:outcome)).to eq([:available, :available])
            expect(transcript.map(&:source)).to eq([:parent, :parent])
          end

          it 'marks the second pass as a repeated playback' do
            expect(transcript.results.map(&:repeated?)).to eq([false, true])
          end

          it 'still leaves the parent interaction available for the real request afterwards' do
            transcript
            expect(child.response_for(the_request).status.code).to eq(503)
          end
        end

        it 'does not consume the real interaction list at all' do
          expect(child).not_to receive(:response_for)
          expect(parent).not_to receive(:response_for)
          expect(VCR.configuration).not_to receive(:invoke_hook)

          Rehearsal.new(child).rehearse_all([the_request, the_request])
        end
      end

      describe '#rehearse' do
        let(:the_interaction) { interaction('first') }

        it 'does not consume the real interaction' do
          list = list([the_interaction])
          Rehearsal.new(list).rehearse(the_request)

          expect(list.remaining_unused_interaction_count).to eq(1)
          expect(list).not_to have_used_interaction_matching(the_request)
        end

        it 'reports the interaction and response that would be used' do
          result = Rehearsal.new(list([the_interaction])).rehearse(the_request)
          expect(result.interaction).to be(the_interaction)
          expect(result.response).to be(the_interaction.response)
          expect(result.depth).to eq(0)
          expect(result.label).to eq('cassette')
          expect(result).not_to be_from_parent
          expect(result).not_to be_repeated
        end

        it 'sequences identical requests like real playback' do
          interactions = [interaction('one'), interaction('two')]
          transcript = Rehearsal.new(list(interactions)).rehearse_all([the_request, the_request, the_request])
          expect(transcript.map { |r| r.response && r.response.body }).to eq(['one', 'two', nil])
        end

        it 'uses the matchers given to the rehearsal' do
          rehearsal = Rehearsal.new(list([interaction('only')]), match_requests_on: [:method])
          expect(rehearsal.rehearse(request('http://example.com/other'))).to be_available
        end

        it "uses each list's own matchers by default" do
          rehearsal = Rehearsal.new(list([interaction('only')]))
          expect(rehearsal.request_matchers).to eq([:method, :uri])
          expect(rehearsal.rehearse(request('http://example.com/foo', :post))).to be_exhausted
        end

        it 'raises when a requested matcher is not registered' do
          expect {
            Rehearsal.new(list([]), match_requests_on: [:not_a_matcher])
          }.to raise_error(Errors::UnregisteredMatcherError)
        end

        it 'shares one virtual state across successive calls' do
          rehearsal = Rehearsal.new(list([interaction('only')]))
          expect(rehearsal.rehearse(the_request)).to be_available
          expect(rehearsal.rehearse(the_request)).to be_exhausted
          expect(rehearsal.results.size).to eq(2)
        end

        it 'returns an exhausted result when there is no cassette' do
          expect(Rehearsal.new(HTTPInteractionList::NullList).rehearse(the_request)).to be_exhausted
        end

        it 'is aliased as #preflight and #dry_run' do
          rehearsal = Rehearsal.new(list([interaction('one'), interaction('two')]))
          expect(rehearsal.preflight(the_request)).to be_available
          expect(rehearsal.dry_run(the_request)).to be_available
        end
      end

      describe '#rehearse!' do
        it 'raises a RehearsalExhaustedError when nothing is playable' do
          expect {
            Rehearsal.new(list([])).rehearse!(the_request)
          }.to raise_error(Errors::RehearsalExhaustedError, /GET http:\/\/example.com\/foo/)
        end

        it 'returns the result when the request is playable' do
          result = Rehearsal.new(list([interaction('one')])).rehearse!(the_request)
          expect(result).to be_available
        end
      end

      describe 'Transcript#assert_all_available!' do
        it 'returns the transcript when every request is playable' do
          transcript = Rehearsal.new(list([interaction('one')])).rehearse_all([the_request])
          expect(transcript.assert_all_available!).to be(transcript)
        end

        it 'names the first request that ran out of interactions' do
          transcript = Rehearsal.new(list([interaction('one')])).rehearse_all([the_request, the_request])
          expect {
            transcript.assert_all_available!
          }.to raise_error(Errors::RehearsalExhaustedError, /request 2 of 2/)
        end
      end

      describe '#remaining_unused_interaction_counts' do
        it 'reports the virtual count per cassette in the chain' do
          parent = list([interaction('p1'), interaction('p2')], false, HTTPInteractionList::NullList, 'outer')
          child = list([interaction('c1')], false, parent, 'inner')
          rehearsal = Rehearsal.new(child)
          rehearsal.rehearse(the_request)

          expect(rehearsal.remaining_unused_interaction_counts).to eq([0, 2])
          expect(rehearsal.remaining_unused_interaction_count).to eq(0)
          expect(rehearsal.total_remaining_unused_interaction_count).to eq(2)
        end

        it 'does not change the real playback counters' do
          child = list([interaction('c1'), interaction('c2')])
          Rehearsal.new(child).rehearse(the_request)

          expect(child.remaining_unused_interaction_count).to eq(2)
          expect(child).not_to have_used_interaction_matching(the_request)
        end

        it 'does not double count an interaction the real playback later consumes' do
          child = list([interaction('c1'), interaction('c2')])
          rehearsal = Rehearsal.new(child)
          rehearsal.rehearse(the_request)
          expect(rehearsal.remaining_unused_interaction_count).to eq(1)

          # Real playback then consumes the very interaction the rehearsal had
          # virtually consumed; it must not be subtracted twice.
          child.response_for(the_request)
          expect(child.remaining_unused_interaction_count).to eq(1)
          expect(rehearsal.remaining_unused_interaction_count).to eq(1)
        end

        it 'reflects real playback of a different interaction' do
          foo = interaction('c1')
          bar = VCR::HTTPInteraction.new(
            VCR::Request.new(:get, 'http://example.com/bar', '', {}),
            VCR::Response.new(VCR::ResponseStatus.new(200, 'OK'), {}, 'c2', '1.1'),
            Time.utc(2025, 1, 1)
          )
          child = list([foo, bar])
          rehearsal = Rehearsal.new(child)
          rehearsal.rehearse(the_request)
          expect(rehearsal.remaining_unused_interaction_count).to eq(1)

          child.response_for(VCR::Request.new(:get, 'http://example.com/bar', '', {}))
          expect(rehearsal.remaining_unused_interaction_count).to eq(0)
        end
      end

      describe '#summary' do
        it 'describes each list and the virtual total' do
          parent = list([interaction('p1')], false, HTTPInteractionList::NullList, 'outer')
          child = list([interaction('c1')], false, parent, 'inner')
          rehearsal = Rehearsal.new(child)

          expect(rehearsal.summary).to include('inner: 1 unused', 'outer: 1 unused', 'total virtual remaining: 2')
        end
      end

      describe '#reset!' do
        it 'forgets all virtual consumption' do
          rehearsal = Rehearsal.new(list([interaction('only')]))
          rehearsal.rehearse(the_request)
          expect(rehearsal.rehearse(the_request)).to be_exhausted

          rehearsal.reset!
          expect(rehearsal.rehearse(the_request)).to be_available
          expect(rehearsal.results.size).to eq(1)
        end
      end

      describe '#levels' do
        it 'follows the parent chain, current cassette first' do
          parent = list([], false, HTTPInteractionList::NullList, 'outer')
          child = list([], false, parent, 'inner')

          expect(Rehearsal.new(child).levels).to eq([child, parent])
          expect(Rehearsal.new(child).list_count).to eq(2)
        end
      end

      describe 'entry points on HTTPInteractionList' do
        let(:parent) { list([interaction('parent', code: 503)], false, HTTPInteractionList::NullList, 'outer') }
        let(:child)  { list([interaction('child', code: 202)], false, parent, 'inner') }

        before { child.response_for(the_request) }

        it 'rehearses step by step through #rehearse, keeping one virtual state' do
          first  = child.rehearse(the_request)
          second = child.rehearse(the_request)

          expect([first.outcome, first.source]).to eq([:available, :parent])
          expect([second.outcome, second.source]).to eq([:exhausted, :none])
          expect(child.virtual_remaining_unused_interaction_count).to eq(0)
        end

        it 'rehearses a whole sequence through #dry_run' do
          transcript = child.dry_run([the_request, the_request])
          expect(transcript.map(&:outcome)).to eq([:available, :exhausted])
        end

        it 'returns a result for a single request when given #dry_run(request)' do
          expect(child.dry_run(the_request)).to be_a(Rehearsal::Result)
        end

        it 'leaves real playback untouched' do
          child.rehearse(the_request)
          expect(child.response_for(the_request).status.code).to eq(503)
        end

        it 'can forget its virtual state' do
          child.rehearse(the_request)
          expect(child.rehearse(the_request)).to be_exhausted

          child.reset_rehearsal!
          expect(child.rehearse(the_request)).to be_available
        end
      end
    end
  end
end

::RSpec.describe 'VCR::Cassette#rehearsal' do
  before(:each) do
    allow(VCR.configuration).to receive(:logger).and_return(VCR::Logger::Null)
    @original_dir = VCR.configuration.cassette_library_dir
    VCR.configuration.cassette_library_dir = Dir.mktmpdir
  end

  after(:each) do
    dir = VCR.configuration.cassette_library_dir
    VCR.configuration.cassette_library_dir = @original_dir
    FileUtils.remove_entry(dir) if dir && ::File.directory?(dir)
  end

  def interaction(body, code = 200)
    VCR::HTTPInteraction.new(
      VCR::Request.new(:get, 'http://example.com/foo', '', {}),
      VCR::Response.new(VCR::ResponseStatus.new(code, 'OK'), {}, body, '1.1'),
      Time.utc(2025, 1, 1)
    )
  end

  let(:the_request) { interaction('ignored').request }

  it 'rehearses against the current cassette, including outer cassettes' do
    VCR.use_cassette('outer_rehearsal', record: :none, allow_unused_http_interactions: true) do
      VCR.use_cassette('inner_rehearsal', record: :none, allow_unused_http_interactions: true) do |cassette|
        cassette.http_interactions.interactions << interaction('inner', 202)

        rehearsal = VCR.rehearsal
        expect(rehearsal.levels.map(&:label)).to eq(%w[inner_rehearsal outer_rehearsal])
        expect(rehearsal.rehearse(the_request).response.status.code).to eq(202)
      end
    end
  end

  it 'rehearses a whole sequence through VCR.rehearse_all' do
    VCR.use_cassette('vcr_rehearse_all', record: :none) do |cassette|
      cassette.http_interactions.interactions << interaction('only')

      transcript = VCR.rehearse_all([the_request, the_request])
      expect(transcript.map(&:outcome)).to eq([:available, :exhausted])
      expect(transcript.remaining_unused_interaction_count).to eq(0)
      expect(cassette.http_interactions.remaining_unused_interaction_count).to eq(1)
    end
  end

  it 'reports every request as exhausted when no cassette is in use' do
    expect(VCR.rehearsal.rehearse(the_request)).to be_exhausted
    expect(VCR.rehearse_all([the_request]).map(&:outcome)).to eq([:exhausted])
  end

  it 'does not change playback counters, hooks or connection state' do
    VCR.use_cassette('rehearsal_side_effects', record: :none) do |cassette|
      cassette.http_interactions.interactions << interaction('recorded')

      recording_before   = cassette.recording?
      connections_before = VCR.real_http_connections_allowed?
      hooks_before       = VCR.configuration.hooks.dup

      cassette.rehearsal.rehearse_all([the_request, the_request, the_request])

      expect(cassette.http_interactions.remaining_unused_interaction_count).to eq(1)
      expect(cassette.http_interactions).not_to have_used_interaction_matching(the_request)
      expect(cassette.new_recorded_interactions).to be_empty
      expect(cassette.recording?).to eq(recording_before)
      expect(VCR.real_http_connections_allowed?).to eq(connections_before)
      expect(VCR.configuration.hooks).to eq(hooks_before)
    end
  end
end