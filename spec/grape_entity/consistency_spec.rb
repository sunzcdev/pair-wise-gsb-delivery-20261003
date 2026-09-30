# frozen_string_literal: true

require 'ostruct'
require 'ostruct'
require 'thread'
require 'timeout'

describe Grape::Entity::Consistency do
  # ----- versioned domain objects ------------------------------------------------

  class ConsistentAccount
    include Grape::Entity::VersionProtocol

    attr_reader :total, :total_reads
    attr_accessor :details, :note

    def initialize(total:, details: [], note: nil)
      @total = total
      @details = details
      @note = note
      @total_reads = 0
    end

    # The barrier coordinates a background mutation to land at a precise
    # point of the representation (e.g. between the :total getter and the
    # :details getter) instead of racing at random.
    attr_writer :total_barrier

    def total
      @total_reads += 1
      value = @total
      @total_barrier&.gate(:total)
      value
    end

    def deposit(amount)
      changing { @total += amount }
    end
  end

  class ConsistentDetail
    include Grape::Entity::VersionProtocol

    attr_accessor :label

    def initialize(label:, amount:)
      @label = label
      @amount = amount
      @amount_reads = 0
    end

    attr_accessor :amount_barrier

    def amount
      @amount_reads += 1
      value = @amount
      @amount_barrier&.gate(:amount)
      value
    end

    def set_amount(value)
      changing { @amount = value }
    end

    attr_reader :amount_reads
  end

  # A versioned association collection: membership changes advance ITS token;
  # the owner is deliberately not bumped (the protocol lets the application
  # choose which granularity protects membership).
  class VersionedDetailList
    include Grape::Entity::VersionProtocol

    def initialize(items = [])
      @items = items
    end

    def to_ary
      @items
    end

    def add(item)
      changing { @items << item }
    end
  end

  # Returns version tokens from a scripted sequence, so a representation can
  # be made to observe "stable -> change in progress -> stable" without a
  # racy thread.
  class ScriptedObject
    def initialize(versions)
      @versions = versions
      @total_reads = 0
    end

    def representation_version
      return @exhausted if defined?(@exhausted)

      token = @versions.shift
      if token.nil? && @versions.empty?
        @exhausted = stable_version
      else
        token
      end
    end

    def stable_version
      1
    end

    def total
      @total_reads += 1
      100
    end

    attr_reader :total_reads
  end

  class ExplodingAccount
    include Grape::Entity::VersionProtocol

    attr_reader :total_reads

    def total
      @total_reads = (@total_reads || 0) + 1
      raise 'ordinary getter failure'
    end
  end

  # ----- entities ---------------------------------------------------------------

  class StrictAccountEntity < Grape::Entity
    expose :total
    expose :details, using: Class.new(Grape::Entity) { expose :label; expose :amount }
  end

  class RereadAccountEntity < Grape::Entity
    expose :total, reread: true
    expose :details, reread: true, using: Class.new(Grape::Entity) {
      expose :label, reread: true
      expose :amount, reread: true
    }
  end

  class VersionedListEntity < Grape::Entity
    expose :details, reread: true, using: Class.new(Grape::Entity) {
      expose :label, reread: true
      expose :amount, reread: true
    }
  end

  class StrictScriptedEntity < Grape::Entity
    expose :total
  end

  class RereadScriptedEntity < Grape::Entity
    expose :total, reread: true
  end

  class ExplodingEntity < Grape::Entity
    expose :total, reread: true
  end

  # A barrier that arms once: at the gated getter the reader parks, the
  # background mutator performs its change, signals completion, and the reader
  # is released - so the read is guaranteed to observe the pre-update value
  # while the token captured afterwards belongs to the post-update state.
  class MutationBarrier
    def initialize
      @at_gate = Queue.new
      @release = Queue.new
      @done = Queue.new
      @armed = true
    end

    def gate(_name)
      return unless @armed

      @armed = false
      @at_gate.push(true)
      @release.pop
    end

    # Run a mutation exactly when the representation reaches the gated getter:
    # between that getter and the next one (or the final version re-check).
    def mutate
      Thread.new do
        Thread.current.abort_on_exception = false
        @at_gate.pop
        yield
        @release.push(true)
      ensure
        @done.push(true)
      end
    end

    def wait!
      Timeout.timeout(5) { @done.pop }
    end
  end

  describe Grape::Entity::VersionProtocol do
    it 'starts on a stable token and advances it for every bracketed change' do
      account = ConsistentAccount.new(total: 10)

      expect(account.representation_version).to eq 0
      account.deposit(5)
      expect(account.representation_version).to eq 1
      account.deposit(2)
      expect(account.representation_version).to eq 2
      expect(account.total).to eq 17
    end

    it 'reports nil while a change is in progress and a fresh token afterwards' do
      account = ConsistentAccount.new(total: 10)
      seen_inside = nil
      account.changing do
        seen_inside = account.representation_version
        account.instance_variable_set(:@total, 20)
      end

      expect(seen_inside).to be_nil
      expect(account.representation_version).to eq 1
      expect(account.total).to eq 20
    end

    it 'advances the token even when the mutation raises' do
      account = ConsistentAccount.new(total: 10)

      expect do
        account.changing { raise 'boom' }
      end.to raise_error('boom')

      expect(account.representation_version).to eq 1
    end
  end

  describe '.represent_consistently' do
    context 'when nothing moves' do
      it 'returns an ordinary serializable hash' do
        account = ConsistentAccount.new(
          total: 100,
          details: [ConsistentDetail.new(label: 'a', amount: 40),
                    ConsistentDetail.new(label: 'b', amount: 60)]
        )

        result = StrictAccountEntity.represent_consistently(account)

        expect(result).to eq(
          total: 100,
          details: [{ label: 'a', amount: 40 }, { label: 'b', amount: 60 }]
        )
      end

      it 'represents collections element by element without a wrapper' do
        accounts = [ConsistentAccount.new(total: 1), ConsistentAccount.new(total: 2)]
        entity = Class.new(Grape::Entity) { expose :total }

        result = entity.represent_consistently(accounts)

        expect(result).to eq([{ total: 1 }, { total: 2 }])
      end

      it 'does not protect objects opting out of the version protocol' do
        plain = OpenStruct.new(total: 5)
        entity = Class.new(Grape::Entity) { expose :total }

        expect(entity.represent_consistently(plain)).to eq(total: 5)
      end
    end

    context 'when a change lands exactly between two getters' do
      let(:details) { [ConsistentDetail.new(label: 'a', amount: 60)] }

      it 'rejects the half-old/half-new hash when no field allows rereading' do
        account = ConsistentAccount.new(total: 100, details: details)
        barrier = MutationBarrier.new
        account.total_barrier = barrier

        # The :total getter reads 100, parks; the background thread advances
        # the account token; then :details and the final check run.
        mutator = barrier.mutate { account.deposit(10) }

        expect do
          StrictAccountEntity.represent_consistently(account)
        end.to raise_error(Grape::Entity::Consistency::InconsistentVersionError) do |error|
          expect(error).not_to be_retriable
          expect(error.participant).to equal account
          expect(error.expected_version).to eq 0
          expect(error.actual_version).to eq 1
        end

        barrier.wait!
        mutator.join
        # The rejected Hash is never delivered; the getter ran only once.
        expect(account.total_reads).to eq 1
        expect(account.total).to eq 110
      end

      it 'shows the plain entry point would have delivered a half-old half-new hash' do
        # Without the version gate the barrier reproduces the original bug:
        # the total getter observes the old value, the mutation lands, then the
        # detail getter observes the new graph.
        detail = ConsistentDetail.new(label: 'a', amount: 60)
        account = ConsistentAccount.new(total: 100, details: [detail])
        account.deposit(0) # advance to token 1 so a later change moves 1 -> 2

        barrier = MutationBarrier.new
        account.total_barrier = barrier
        mutator = barrier.mutate do
          account.deposit(10)
          detail.set_amount(70)
        end

        plain = StrictAccountEntity.represent(account, serializable: true)

        # total was read BEFORE the update while details were serialized AFTER:
        # exactly the inconsistent splice the consistent entry point rejects.
        expect(plain).to eq(total: 100, details: [{ label: 'a', amount: 70 }])

        barrier.wait!
        mutator.join
      end

      it 're-reads and succeeds when every touched exposure declares reread' do
        account = ConsistentAccount.new(total: 100, details: details)
        barrier = MutationBarrier.new
        account.total_barrier = barrier

        mutator = barrier.mutate { account.deposit(10) }

        result = RereadAccountEntity.represent_consistently(account)

        # The first reading (total=100, token 0) is discarded entirely; the
        # delivered Hash comes from the stable post-update state.
        expect(result).to eq(total: 110, details: [{ label: 'a', amount: 60 }])
        expect(account.total_reads).to eq 2

        barrier.wait!
        mutator.join
      end

      it 'rejects when the reread budget is exhausted' do
        account = ConsistentAccount.new(total: 100, details: details)
        barrier = MutationBarrier.new
        account.total_barrier = barrier

        mutator = barrier.mutate { account.deposit(10) }

        expect do
          RereadAccountEntity.represent_consistently(account, max_rereads: 0)
        end.to raise_error(Grape::Entity::Consistency::InconsistentVersionError, &:retriable?)

        barrier.wait!
        mutator.join
        # Zero rereads allowed: the conflicting getter is never invoked again.
        expect(account.total_reads).to eq 1
      end

      it 'defaults the reread budget to two extra readings' do
        attempts = { count: 0 }
        object = Object.new
        object.define_singleton_method(:representation_version) { attempts[:count] }
        object.define_singleton_method(:total) do
          attempts[:count] += 1
          attempts[:count]
        end

        expect do
          Class.new(Grape::Entity) { expose :total, reread: true }
               .represent_consistently(object)
        end.to raise_error(Grape::Entity::Consistency::InconsistentVersionError)

        # initial + 2 rereads, then the budget is exhausted
        expect(attempts[:count]).to eq 3
      end
    end

    context 'when a selected nested object changes on its own' do
      it 'rejects because the nested object advanced, even though the parent did not' do
        detail = ConsistentDetail.new(label: 'a', amount: 60)
        account = ConsistentAccount.new(total: 100, details: [detail])
        account_version = account.representation_version

        # Park inside the detail's :amount getter (after :label), then advance
        # ONLY the detail's token. The account token is untouched.
        barrier = MutationBarrier.new
        detail.amount_barrier = barrier
        mutator = barrier.mutate { detail.set_amount(61) }

        expect do
          StrictAccountEntity.represent_consistently(account)
        end.to raise_error(Grape::Entity::Consistency::InconsistentVersionError) do |error|
          expect(error.participant).to equal detail
          expect(error.expected_version).to eq 0
          expect(error.actual_version).to eq 1
          expect(error).not_to be_retriable
        end

        barrier.wait!
        mutator.join
        expect(account.representation_version).to eq account_version
        expect(detail.amount_reads).to eq 1
      end
    end

    context 'when a versioned association collection changes membership' do
      it 'rejects the stale membership and delivers the new set on reread' do
        added = ConsistentDetail.new(label: 'c', amount: 5)
        first = ConsistentDetail.new(label: 'a', amount: 60)
        list = VersionedDetailList.new([first])
        account = OpenStruct.new(details: list)

        # Park after the first member was serialized, then add a member. The
        # COLLECTION token (not the member's) advances.
        barrier = MutationBarrier.new
        first.amount_barrier = barrier
        mutator = barrier.mutate { list.add(added) }

        result = VersionedListEntity.represent_consistently(account)

        expect(result).to eq(
          details: [{ label: 'a', amount: 60 }, { label: 'c', amount: 5 }]
        )
        expect(first.amount_reads).to eq 2 # once on the rejected reading, once on reread

        barrier.wait!
        mutator.join
      end
    end

    context 'when an unselected association object changes' do
      it 'is ignored: no rejection, no reread' do
        note = ConsistentAccount.new(total: 7)
        account = ConsistentAccount.new(total: 100, note: note)
        entity = Class.new(Grape::Entity) { expose :total }

        output = entity.represent_consistently(account)
        note.deposit(3) # the entity never selects :note

        expect(output).to eq(total: 100)
        expect(account.total_reads).to eq 1
        expect(note.total).to eq 10
      end
    end

    context 'when a participant is caught mid-change (nil version)' do
      it 'rejects a nil starting version on a non-reread field' do
        object = Object.new
        reads = 0
        object.define_singleton_method(:representation_version) { nil }
        object.define_singleton_method(:total) { reads += 1 }

        expect do
          StrictScriptedEntity.represent_consistently(object)
        end.to raise_error(Grape::Entity::Consistency::InconsistentVersionError) do |error|
          expect(error).not_to be_retriable
          expect(error.actual_version).to be_nil
        end
        expect(reads).to eq 0
      end

      it 'rereads a declared field once the object is stable again' do
        # register -> 0, first commit -> nil (in progress, retriable),
        # second register -> 1, second commit -> 1 (stable)
        object = ScriptedObject.new([0, nil, 1])

        result = RereadScriptedEntity.represent_consistently(object)

        expect(result).to eq(total: 100)
        expect(object.total_reads).to eq 2
      end

      it 'does not reread when the object stays in progress past the budget' do
        object = Object.new
        object.define_singleton_method(:representation_version) { nil }

        expect do
          RereadScriptedEntity.represent_consistently(object)
        end.to raise_error(Grape::Entity::Consistency::InconsistentVersionError) do |error|
          expect(error).to be_retriable
          expect(error.actual_version).to be_nil
        end
      end
    end

    context 'when a getter raises an ordinary exception' do
      it 'propagates it and never treats it as version contention' do
        account = ExplodingAccount.new

        expect do
          ExplodingEntity.represent_consistently(account)
        end.to raise_error(RuntimeError, 'ordinary getter failure')

        expect(account.total_reads).to eq 1
      end
    end
  end

  describe described_class::Session do
    subject(:session) { described_class.new }

    def versioned_object
      object = Object.new
      object.instance_variable_set(:@version, 0)
      object.define_singleton_method(:representation_version) { @version }
      object.define_singleton_method(:bump) { @version += 1 }
      object
    end

    describe '#register / #commit!' do
      it 'commits when versions are unchanged' do
        object = versioned_object
        session.register(object)
        expect { session.commit! }.not_to raise_error
      end

      it 'rejects a moved captured version, non-retriable by default' do
        object = versioned_object
        session.register(object)
        object.bump

        expect { session.commit! }.to raise_error(Grape::Entity::Consistency::InconsistentVersionError) do |error|
          expect(error).not_to be_retriable
          expect(error.expected_version).to eq 0
          expect(error.actual_version).to eq 1
        end
      end

      it 'defers a nil starting root version until a getter or the joint commit' do
        unstable = Object.new
        unstable.define_singleton_method(:representation_version) { nil }

        expect { session.register(unstable) }.not_to raise_error
        expect { session.commit! }
          .to raise_error(Grape::Entity::Consistency::InconsistentVersionError) { |e| expect(e).not_to be_retriable }
      end

      it 'rejects a nil association at resolution unless it allows rereading' do
        unstable = Object.new
        unstable.define_singleton_method(:representation_version) { nil }

        expect { session.register(unstable, reread: false) }
          .to raise_error(Grape::Entity::Consistency::InconsistentVersionError) { |e| expect(e).not_to be_retriable }
      end

      it 'marks a nil association at resolution retriable when it allows rereading' do
        unstable = Object.new
        unstable.define_singleton_method(:representation_version) { nil }

        expect { session.register(unstable, reread: true) }
          .to raise_error(Grape::Entity::Consistency::InconsistentVersionError, &:retriable?)
      end

      it 'ignores objects opting out of the protocol' do
        expect { session.register(OpenStruct.new(a: 1)) }.not_to raise_error
        expect { session.register(nil) }.not_to raise_error
      end
    end

    describe 'reread voting' do
      let(:reread_exposure) { Grape::Entity::Exposure.new(:total, reread: true) }
      let(:strict_exposure) { Grape::Entity::Exposure.new(:total, {}) }
      let(:nesting_exposure) { Grape::Entity::Exposure.new(:group, { nesting: true }) }

      it 'retries only when every read involved declared reread' do
        object = versioned_object
        session.register(object)
        session.around_read(object, reread_exposure) { :ok }
        object.bump

        expect { session.commit! }.to raise_error(Grape::Entity::Consistency::InconsistentVersionError, &:retriable?)
      end

      it 'does not retry when any involved read was a normal getter' do
        object = versioned_object
        session.register(object)
        session.around_read(object, reread_exposure) { :ok }
        session.around_read(object, strict_exposure) { :ok }
        object.bump

        expect { session.commit! }.to raise_error(Grape::Entity::Consistency::InconsistentVersionError) do |error|
          expect(error).not_to be_retriable
        end
      end

      it 'does not let a nesting exposure vote as a reread getter' do
        object = versioned_object
        session.register(object)
        session.around_read(object, nesting_exposure) { :ok }
        object.bump

        expect { session.commit! }.to raise_error(Grape::Entity::Consistency::InconsistentVersionError) do |error|
          expect(error).not_to be_retriable
        end
      end
    end

    describe '#reset!' do
      it 'drops captured participants between readings' do
        object = versioned_object
        session.register(object)
        session.reset!
        object.bump

        expect { session.commit! }.not_to raise_error
      end

      it 'keeps the running reread tally so the retry budget can be exhausted' do
        session.reread_count = 2
        session.reset!

        expect(session.reread_count).to eq 2
      end
    end
  end
end


