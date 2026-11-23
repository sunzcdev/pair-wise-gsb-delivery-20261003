require 'vcr/util/logger'
require 'vcr/cassette/http_interaction_list'
require 'vcr/request_matcher_registry'
require 'vcr/structs'
require 'support/configuration_stubbing'

module VCR
  class Cassette
    ::RSpec.describe HTTPInteractionList do
      include_context "configuration stubbing"

      ::RSpec::Matchers.define :respond_with do |expected|
        match { |a| expected.nil? ? a.nil? : a.body == expected }
      end

      before(:each) do
        allow(VCR).to receive(:request_matchers).and_return(VCR::RequestMatcherRegistry.new)
        allow(config).to receive(:logger).and_return(double.as_null_object)
      end

      def request_with(values)
        VCR::Request.new.tap do |request|
          values.each do |name, value|
            request.send("#{name}=", value)
          end
        end
      end

      def response(body)
        VCR::Response.new.tap do |r|
          r.body = body
          r.status = VCR::ResponseStatus.new(200)
        end
      end

      def interaction(body, request_values)
        VCR::HTTPInteraction.new \
          request_with(request_values),
          response(body)
      end

      def grouped_interaction(body, group, request_values)
        interaction(body, request_values).tap do |i|
          i.playback_group = group
        end
      end

      let(:original_list_array) do [
        interaction('put response', method: :put),
        interaction('post response 1', method: :post),
        interaction('post response 2', method: :post)
      ] end

      let(:allow_playback_repeats) { false } # the default
      let(:list) { HTTPInteractionList.new(original_list_array, [:method], allow_playback_repeats) }

      describe "#has_used_interaction_matching?" do
        it 'returns false when no interactions have been used' do
          expect(list).not_to have_used_interaction_matching(request_with(method: :put))
        end

        it 'returns true when there is a matching used interaction (even if there is also an unused one that matches)' do
          list.response_for(request_with(method: :post))
          expect(list).to have_used_interaction_matching(request_with(method: :post))
        end

        it 'returns false when none of the used interactions match' do
          list.response_for(request_with(method: :put))
          expect(list).not_to have_used_interaction_matching(request_with(method: :post))
        end
      end

      describe "#remaining_unused_interaction_count" do
        it 'returns the number of unused interactions' do
          expect(list.remaining_unused_interaction_count).to eq(3)

          list.response_for(request_with(method: :get))
          expect(list.remaining_unused_interaction_count).to eq(3)

          list.response_for(request_with(method: :put))
          expect(list.remaining_unused_interaction_count).to eq(2)

          list.response_for(request_with(method: :put))
          expect(list.remaining_unused_interaction_count).to eq(2)

          list.response_for(request_with(method: :post))
          expect(list.remaining_unused_interaction_count).to eq(1)

          list.response_for(request_with(method: :post))
          expect(list.remaining_unused_interaction_count).to eq(0)

          list.response_for(request_with(method: :post))
          expect(list.remaining_unused_interaction_count).to eq(0)
        end
      end

      describe "#assert_no_unused_interactions?" do
        it 'should raise a SkippedHTTPRequestError when there are unused interactions left' do
          expect {
            list.assert_no_unused_interactions!
          }.to raise_error(Errors::UnusedHTTPInteractionError)

          list.response_for(request_with(method: :put))
          expect {
            list.assert_no_unused_interactions!
          }.to raise_error(Errors::UnusedHTTPInteractionError)
        end

        it 'should raise nothing when there are no unused interactions left' do
          [:put, :post, :post].each do |method|
            list.response_for(request_with(method: method))
          end

          expect {
            list.assert_no_unused_interactions!
          }.not_to raise_error
        end

        context 'when the null logger is in use' do
          before { allow(config).to receive(:logger).and_return(Logger::Null) }

          it 'includes formatted request details in the error message' do
            expect {
              list.assert_no_unused_interactions!
            }.to raise_error(/\[put/)
          end

          it 'includes formatted response details in the error message' do
            expect {
              list.assert_no_unused_interactions!
            }.to raise_error(/\[200 "put response"\]/)
          end
        end
      end

      describe "has_interaction_matching?" do
        it 'returns false when the list is empty' do
          expect(HTTPInteractionList.new([], [:method])).not_to have_interaction_matching(double)
        end

        it 'returns false when there is no matching interaction' do
          expect(list).not_to have_interaction_matching(request_with(method: :get))
        end

        it 'returns true when there is a matching interaction' do
          expect(list).to have_interaction_matching(request_with(method: :post))
        end

        it 'does not consume the interactions when they match' do
          expect(list).to have_interaction_matching(request_with(method: :post))
          expect(list.remaining_unused_interaction_count).to eq(3)
          expect(list).to have_interaction_matching(request_with(method: :post))
          expect(list.remaining_unused_interaction_count).to eq(3)
        end

        it 'invokes each matcher block to find the matching interaction' do
          VCR.request_matchers.register(:foo) { |r1, r2| true }
          VCR.request_matchers.register(:bar) { |r1, r2| true }

          calls = 0
          VCR.request_matchers.register(:baz) { |r1, r2| calls += 1; calls == 2 }

          list = HTTPInteractionList.new([
            interaction('response', method: :put)
          ], [:foo, :bar, :baz])

          expect(list).not_to have_interaction_matching(request_with(method: :post))
          expect(list).to     have_interaction_matching(request_with(method: :post))
        end

        it "delegates to the parent list when it can't find a matching interaction" do
          parent_list = double(has_interaction_matching?: true)
          expect(HTTPInteractionList.new( [], [:method], false, parent_list)).to have_interaction_matching(double)
          parent_list = double(has_interaction_matching?: false)
          expect(HTTPInteractionList.new( [], [:method], false, parent_list)).not_to have_interaction_matching(double)
        end

        context 'when allow_playback_repeats is set to true' do
          let(:allow_playback_repeats) { true }

          it 'considers used interactions' do
            list.response_for(request_with(method: :put))

            results = 10.times.map do
              list.has_interaction_matching?(request_with(method: :put))
            end

            expect(results).to eq([true] * 10)
          end
        end

        context 'when allow_playback_repeats is set to false' do
          let(:allow_playback_repeats) { false }

          it 'does not consider used interactions' do
            list.response_for(request_with(method: :put))

            result = 10.times.map do
              list.has_interaction_matching?(request_with(method: :put))
            end

            expect(result).to eq([false] * 10)
          end
        end
      end

      describe "#response_for" do
        it 'returns nil when the list is empty' do
          expect(HTTPInteractionList.new([], [:method]).response_for(double)).to respond_with(nil)
        end

        it 'returns nil when there is no matching interaction' do
          response = HTTPInteractionList.new([
            interaction('foo', method: :post),
            interaction('foo', method: :put)
          ], [:method]).response_for(
            request_with(method: :get)
          )

          expect(response).to respond_with(nil)
        end

        it 'returns the first matching interaction' do
          list = HTTPInteractionList.new([
            interaction('put response', method: :put),
            interaction('post response 1', method: :post),
            interaction('post response 2', method: :post)
          ], [:method])

          expect(list.response_for(request_with(method: :post))).to respond_with("post response 1")
        end

        it 'invokes each matcher block to find the matching interaction' do
          VCR.request_matchers.register(:foo) { |r1, r2| true }
          VCR.request_matchers.register(:bar) { |r1, r2| true }

          calls = 0
          VCR.request_matchers.register(:baz) { |r1, r2| calls += 1; calls == 2 }

          list = HTTPInteractionList.new([
            interaction('response', method: :put)
          ], [:foo, :bar, :baz])

          expect(list.response_for(request_with(method: :post))).to respond_with(nil)
          expect(list.response_for(request_with(method: :post))).to respond_with('response')
        end

        it "delegates to the parent list when it can't find a matching interaction" do
          parent_list = double(response_for: response('parent'))
          result = HTTPInteractionList.new(
            [], [:method], false, parent_list
          ).response_for(double)

          expect(result).to respond_with('parent')
        end

        it 'consumes the first matching interaction so that it will not be used again' do
          expect(list.response_for(request_with(method: :post)).body).to eq("post response 1")
          expect(list.response_for(request_with(method: :post)).body).to eq("post response 2")
        end

        context 'when allow_playback_repeats is set to true' do
          let(:allow_playback_repeats) { true }

          it 'continues to return the response from the last matching interaction when there are no more' do
            list.response_for(request_with(method: :post))

            results = 10.times.map do
              response = list.response_for(request_with(method: :post))
              response ? response.body : nil
            end

            expect(results).to eq(["post response 2"] * 10)
          end
        end

        context 'when allow_playback_repeats is set to false' do
          let(:allow_playback_repeats) { false }

          it 'returns nil when there are no more unused interactions' do
            list.response_for(request_with(method: :post))
            list.response_for(request_with(method: :post))

            results = 10.times.map do
              list.response_for(request_with(method: :post))
            end

            expect(results).to eq([nil] * 10)
          end
        end

        it 'does not modify the original interaction array the list was initialized with' do
          original_dup = original_list_array.dup
          list.response_for(request_with(method: :post))
          expect(original_list_array).to eq original_dup
        end
      end

      context "with a playback group selected" do
        let(:grouped_list_array) do [
          grouped_interaction('group a response 1', 'a', method: :post),
          grouped_interaction('group b response 1', 'b', method: :post),
          grouped_interaction('group a response 2', 'a', method: :post),
          grouped_interaction('group b response 2', 'b', method: :post)
        ] end

        def list_for(group, repeats = false)
          HTTPInteractionList.new(grouped_list_array, [:method], repeats,
                                  HTTPInteractionList::NullList, '', group)
        end

        it 'only plays back unused interactions from the selected group, in order' do
          list = list_for('a')
          expect(list.response_for(request_with(method: :post))).to respond_with('group a response 1')
          expect(list.response_for(request_with(method: :post))).to respond_with('group a response 2')
          expect(list.response_for(request_with(method: :post))).to respond_with(nil)
        end

        it 'keeps each group\'s playback sequence independent' do
          list_a = list_for('a')
          list_b = list_for('b')

          expect(list_a.response_for(request_with(method: :post))).to respond_with('group a response 1')
          expect(list_b.response_for(request_with(method: :post))).to respond_with('group b response 1')
          expect(list_b.response_for(request_with(method: :post))).to respond_with('group b response 2')
          expect(list_a.response_for(request_with(method: :post))).to respond_with('group a response 2')
        end

        it 'treats symbol and string groups as equivalent' do
          list = list_for(:a)
          expect(list.response_for(request_with(method: :post))).to respond_with('group a response 1')
        end

        it 'does not consider used interactions from other groups when repeats are allowed' do
          list = list_for('a', true)
          list.response_for(request_with(method: :post))
          list.response_for(request_with(method: :post))

          results = 3.times.map { list.response_for(request_with(method: :post)) }
          expect(results.map(&:body)).to eq(['group a response 2'] * 3)
        end

        it 'scopes #has_used_interaction_matching? to the selected group' do
          list = list_for('a')
          list.response_for(request_with(method: :post))
          expect(list).to have_used_interaction_matching(request_with(method: :post))

          list.playback_group = 'b'
          expect(list).not_to have_used_interaction_matching(request_with(method: :post))
        end

        it 'scopes #remaining_unused_interaction_count to the selected group' do
          list = list_for('a')
          expect(list.remaining_unused_interaction_count).to eq(2)
          list.response_for(request_with(method: :post))
          expect(list.remaining_unused_interaction_count).to eq(1)
        end

        it 'scopes #assert_no_unused_interactions! to the selected group' do
          list = list_for('a')
          2.times { list.response_for(request_with(method: :post)) }
          expect { list.assert_no_unused_interactions! }.not_to raise_error
        end

        it 'only lists the selected group\'s unused interactions in the assertion error' do
          allow(config).to receive(:logger).and_return(Logger::Null)
          list = list_for('a')
          list.response_for(request_with(method: :post))

          expect { list.assert_no_unused_interactions! }.to raise_error(Errors::UnusedHTTPInteractionError) { |error|
            expect(error.message).to include('group a response 2')
            expect(error.message).not_to include('group b response')
          }
        end

        it 'does not play back interactions from other groups even when they are the only ones left' do
          list = list_for('a')
          2.times { list.response_for(request_with(method: :post)) }
          expect(list.remaining_unused_interaction_count).to eq(0)
          expect(list.response_for(request_with(method: :post))).to respond_with(nil)
        end

        it 'allows the group to be changed after the list is built' do
          list = list_for('a')
          expect(list.response_for(request_with(method: :post))).to respond_with('group a response 1')

          list.playback_group = 'b'
          expect(list.response_for(request_with(method: :post))).to respond_with('group b response 1')
        end
      end

      context "without a playback group" do
        it 'considers interactions from all groups, as before' do
          list = HTTPInteractionList.new([
            grouped_interaction('group a response', 'a', method: :post),
            grouped_interaction('group b response', 'b', method: :post),
            interaction('ungrouped response', method: :post)
          ], [:method])

          expect(list.remaining_unused_interaction_count).to eq(3)
          expect(list.response_for(request_with(method: :post))).to respond_with('group a response')
          expect(list.response_for(request_with(method: :post))).to respond_with('group b response')
          expect(list.response_for(request_with(method: :post))).to respond_with('ungrouped response')
        end
      end
    end
  end
end

