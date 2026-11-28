require "flipper/adapters/actor_limit"

RSpec.describe Flipper::Adapters::ActorLimit do
  it_should_behave_like 'an adapter that stores its sync version' do
    subject { Flipper::Adapters::ActorLimit.new(Flipper::Adapters::Memory.new, 5) }
  end

  it_should_behave_like 'a flipper adapter' do
    let(:limit) { 5 }
    let(:adapter) { Flipper::Adapters::ActorLimit.new(Flipper::Adapters::Memory.new, limit) }

    subject { adapter }

    describe '#enable' do
      it "fails when limit exceeded" do
        5.times { |i| feature.enable Flipper::Actor.new("User;#{i}") }

        expect {
          feature.enable Flipper::Actor.new("User;6")
        }.to raise_error(Flipper::Adapters::ActorLimit::LimitExceeded)
      end

      it "allows enabling an existing actor when the limit is reached" do
        5.times { |i| feature.enable Flipper::Actor.new("User;#{i}") }

        expect {
          feature.enable Flipper::Actor.new("User;0")
        }.not_to raise_error
        expect(feature.actors_value.size).to eq(5)
        expect(feature.enabled?(Flipper::Actor.new("User;0"))).to be(true)
      end

      it "allows exceeding limit when in sync mode" do
        5.times { |i| feature.enable Flipper::Actor.new("User;#{i}") }

        described_class.with_sync_mode do
          expect {
            feature.enable Flipper::Actor.new("User;6")
          }.not_to raise_error
        end
      end
    end

    describe '#replace' do
      let(:limit) { 2 }

      before do
        feature.enable_actor Flipper::Actor.new('old')
        feature.enable_actor Flipper::Actor.new('kept')
      end

      it 'swaps a member of a full set and keeps the total bounded' do
        expect(feature.replace_actor(Flipper::Actor.new('old'), Flipper::Actor.new('new'))).to be(true)

        expect(feature.actors_value).to eq(Set['new', 'kept'])
        expect(feature.actors_value.size).to eq(limit)
        expect(feature.enabled?(Flipper::Actor.new('new'))).to be(true)
        expect(feature.enabled?(Flipper::Actor.new('old'))).to be(false)
        expect(feature.enabled?(Flipper::Actor.new('kept'))).to be(true)
      end

      it 'refuses a change that is not a swap instead of growing the set' do
        expect {
          feature.replace_actor(Flipper::Actor.new('ghost'), Flipper::Actor.new('other'))
        }.to raise_error(Flipper::ActorSetConflict)

        expect(feature.actors_value).to eq(Set['old', 'kept'])
      end

      it 'raises a retryable conflict and writes nothing when expected is stale' do
        expect {
          feature.replace_actor(
            Flipper::Actor.new('old'),
            Flipper::Actor.new('new'),
            expected: %w[old ghost],
          )
        }.to raise_error(Flipper::ActorSetConflict) { |error| expect(error.retryable?).to be(true) }

        expect(feature.actors_value).to eq(Set['old', 'kept'])
      end

      it 'settles two racing replacements at zero slots with one winner and one retry' do
        full = Flipper::Adapters::ActorLimit.new(Flipper::Adapters::Memory.new, 1)
        contest = Flipper.new(full)
        contest.enable_actor :pilot, Flipper::Actor.new('old')
        observed = contest[:pilot].actors_value.to_a

        gate = Queue.new
        results = Queue.new
        threads = 2.times.map do |index|
          Thread.new do
            gate.pop
            begin
              actor = Flipper::Actor.new("new#{index}")
              contest.replace_actor(:pilot, Flipper::Actor.new('old'), actor, expected: observed)
              results << [actor, :ok]
            rescue Flipper::ActorSetConflict
              results << [Flipper::Actor.new("new#{index}"), :retry]
            end
          end
        end
        2.times { gate << true }
        threads.each(&:join)

        outcomes = []
        outcomes << results.pop until results.empty?
        winner = outcomes.detect { |_actor, status| status == :ok }
        loser = outcomes.detect { |_actor, status| status == :retry }

        expect(winner).not_to be_nil
        expect(loser).not_to be_nil
        expect(outcomes.size).to eq(2)

        # The real gate results say who won and who needs to retry.
        expect(contest[:pilot].actors_value).to eq(Set[winner.first.flipper_id])
        expect(contest.enabled?(:pilot, winner.first)).to be(true)
        expect(contest.enabled?(:pilot, loser.first)).to be(false)
        expect(contest.enabled?(:pilot, Flipper::Actor.new('old'))).to be(false)
      end
    end
  end

  describe '.sync_mode' do
    after do
      described_class.sync_mode = nil
    end

    it 'defaults to nil/falsy' do
      expect(described_class.sync_mode).to be_falsy
    end

    it 'can be set and read' do
      described_class.sync_mode = true
      expect(described_class.sync_mode).to be true
    end
  end

  describe '.with_sync_mode' do
    after do
      described_class.sync_mode = nil
    end

    it 'sets sync_mode to true within block' do
      described_class.with_sync_mode do
        expect(described_class.sync_mode).to be true
      end
    end

    it 'restores previous value after block' do
      expect(described_class.sync_mode).to be_falsy
      described_class.with_sync_mode { }
      expect(described_class.sync_mode).to be_falsy
    end

    it 'restores previous value even on exception' do
      expect {
        described_class.with_sync_mode { raise "boom" }
      }.to raise_error("boom")
      expect(described_class.sync_mode).to be_falsy
    end

    it 'returns the block result' do
      result = described_class.with_sync_mode { 42 }
      expect(result).to eq(42)
    end
  end
end
