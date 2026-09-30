# frozen_string_literal: true

require 'ostruct'
require 'thread'

describe 'consistent representation (version protocol)' do
  # An associated object that has its own representation version, independent
  # of the aggregate that references it.
  class VersionedCustomer
    include Grape::Entity::Versioned

    def initialize(name)
      representation_initialize
      @name = name
    end

    attr_reader :name

    def rename(new_name)
      representation_updating { @name = new_name }
    end
  end

  # A model that implements the version protocol by including
  # Grape::Entity::Versioned. Each getter records how often it was read and can
  # run a one-shot barrier hook, allowing tests to place a committed update at
  # a precise point *between* two getters of a representation pass.
  class VersionedInvoice
    include Grape::Entity::Versioned

    attr_reader :lines, :customer

    def initialize(total:, lines:, customer:)
      representation_initialize
      @total = total
      @lines = Grape::Entity::VersionedArray.new(lines)
      @customer = customer
      @reads = Hash.new(0)
    end

    def total
      @reads[:total] += 1
      value = @total
      @barrier&.call(:total)
      value
    end

    def lines
      @reads[:lines] += 1
      value = @lines
      @barrier&.call(:lines)
      value
    end

    def customer
      @reads[:customer] += 1
      @customer
    end

    def read_count(name)
      @reads[name]
    end

    def with_barrier
      @barrier = proc { |getter| yield(getter) }
      self
    end

    def disarm_barrier!
      @barrier = nil
    end

    # Application update contract: every observable change to the invoice and
    # its collection must advance the corresponding representation version.
    def recalculate(new_total, new_line)
      representation_updating do
        yield :started if block_given?
        @total = new_total
        @lines << new_line
        yield :finished if block_given?
      end
    end

    # Change an associated object only; the invoice version stays untouched.
    def touch_customer(new_name)
      @customer.rename(new_name)
    end
  end

  # A versioned element of a nested collection, able to fire a hook while its
  # attribute is being read (after the collection token was captured).
  class HookedLine
    include Grape::Entity::Versioned

    attr_reader :amount

    def initialize(sku, amount, &hook)
      representation_initialize
      @sku = sku
      @amount = amount
      @hook = hook
    end

    def sku
      @hook&.call
      @sku
    end
  end

  class LineEntity < Grape::Entity
    expose :sku, reread: true
    expose :amount, reread: true
  end

  class CustomerEntity < Grape::Entity
    expose :name, reread: true
  end

  class InvoiceEntity < Grape::Entity
    expose :total, reread: true
    expose :lines, using: LineEntity, reread: true
    expose :customer, using: CustomerEntity, reread: true
  end

  class RigidInvoiceEntity < Grape::Entity
    expose :total
    expose :lines, using: LineEntity
  end

  def build_invoice
    VersionedInvoice.new(
      total: 100,
      lines: [{ sku: 'A', amount: 100 }],
      customer: VersionedCustomer.new('Acme')
    )
  end

  # Deterministic barrier: commit an update inside the +lines+ getter, i.e.
  # strictly after +total+ was read and while +lines+ is being read. The
  # getter still returns the array reference it captured earlier; because the
  # update mutates that array in place, a plain representation splices the old
  # total with the new membership.
  def commit_update_between_getters!(invoice, new_total, new_line)
    invoice.with_barrier do |getter|
      next unless getter == :lines

      invoice.disarm_barrier!
      invoice.recalculate(new_total, new_line) {}
    end
  end

  describe 'the version protocol' do
    it 'exposes a stable representation version and reports no update in flight' do
      invoice = build_invoice
      expect(invoice.representation_version).to be_an(Integer)
      expect(invoice).not_to be_representation_mutating
    end

    it 'advances the object version when an observable attribute changes' do
      invoice = build_invoice
      version_before = invoice.representation_version
      invoice.recalculate(200, { sku: 'B', amount: 100 }) {}
      expect(invoice.representation_version).to be > version_before
    end

    it 'advances the collection version when an associated collection changes' do
      invoice = build_invoice
      lines = invoice.lines
      version_before = lines.representation_version
      lines << { sku: 'B', amount: 100 }
      expect(lines.representation_version).to be > version_before
    end

    it 'reports a mutation in progress for the duration of an update' do
      invoice = build_invoice
      started = Queue.new
      release = Queue.new
      thread = Thread.new do
        invoice.recalculate(300, { sku: 'C', amount: 200 }) do |event|
          next unless event == :started

          started << true
          release.pop
        end
      end
      started.pop
      expect(invoice).to be_representation_mutating
      release << true
      thread.join
      expect(invoice).not_to be_representation_mutating
    end

    it 'is also usable on plain arrays through VersionedArray' do
      lines = Grape::Entity::VersionedArray.new([1, 2])
      expect(lines.representation_version).to be_an(Integer)
      version = lines.representation_version
      lines << 3
      expect(lines.representation_version).to be > version
      expect(lines.to_a).to eq [1, 2, 3]
    end
  end

  describe 'the explicit consistency entry point' do
    it 'renders identical output to .represent when nothing changes' do
      invoice = build_invoice
      output = InvoiceEntity.represent_consistent(invoice)
      expect(output).to eq InvoiceEntity.represent(invoice, serializable: true)
      expect(output).to eq(
        total: 100,
        lines: [{ sku: 'A', amount: 100 }],
        customer: { name: 'Acme' }
      )
    end

    it 'honours root options like .represent' do
      invoice = build_invoice
      output = InvoiceEntity.represent_consistent(invoice, root: 'invoice')
      expect(output).to eq('invoice' => {
                             total: 100,
                             lines: [{ sku: 'A', amount: 100 }],
                             customer: { name: 'Acme' }
                           })
    end

    it 'renders collections with independent versions per element' do
      invoices = [build_invoice, build_invoice]
      output = InvoiceEntity.represent_consistent(invoices)
      expect(output).to be_an(Array)
      expect(output.size).to eq 2
    end

    it 'works with objects that do not implement the protocol (no guarding)' do
      plain = OpenStruct.new(total: 1, lines: [], customer: VersionedCustomer.new('x'))
      output = InvoiceEntity.represent_consistent(plain)
      expect(output[:total]).to eq 1
    end
  end

  describe 'race between a background update and a representation' do
    it 'plain .represent can splice a pre-update total with post-update lines' do
      invoice = build_invoice
      commit_update_between_getters!(invoice, 200, { sku: 'B', amount: 100 })
      output = InvoiceEntity.represent(invoice, serializable: true)
      # The exact failure mode of the unguarded path: half old, half new.
      expect(output[:total]).to eq 100
      expect(output[:lines]).to eq [{ sku: 'A', amount: 100 }, { sku: 'B', amount: 100 }]
    end

    it 'rejects a half-old half-new hash when a selected collection changes mid-pass' do
      invoice = build_invoice
      commit_update_between_getters!(invoice, 200, { sku: 'B', amount: 100 })

      expect do
        RigidInvoiceEntity.represent_consistent(invoice)
      end.to raise_error(Grape::Entity::UnstableVersion) do |error|
        # The spliced Hash is not attached to or returned with the error.
        expect(error.message).not_to include('sku')
      end

      # No getter was ever invoked a second time: the retry was refused.
      expect(invoice.read_count(:total)).to eq 1
      expect(invoice.read_count(:lines)).to eq 1
      # The object really did move; only the representation was rejected.
      expect(invoice.total).to eq 200
    end

    it 're-reads and succeeds when every participating field allows re-reading' do
      invoice = build_invoice
      commit_update_between_getters!(invoice, 200, { sku: 'B', amount: 100 })

      output = InvoiceEntity.represent_consistent(invoice, max_attempts: 3)
      expect(output[:total]).to eq 200
      expect(output[:lines]).to eq [{ sku: 'A', amount: 100 }, { sku: 'B', amount: 100 }]
      expect(invoice.read_count(:total)).to eq 2
      expect(invoice.read_count(:lines)).to eq 2
    end

    it 'does not reject when an unselected association changes (control case)' do
      invoice = build_invoice
      # Mutate an associated object while total is being read. The customer
      # association is not selected by :only, so it is never registered.
      invoice.with_barrier do |getter|
        next unless getter == :total

        invoice.disarm_barrier!
        invoice.touch_customer('Globex')
      end

      output = InvoiceEntity.represent_consistent(invoice, only: %i[total lines])
      expect(output[:total]).to eq 100
      expect(output[:lines]).to eq [{ sku: 'A', amount: 100 }]
      expect(output).not_to have_key(:customer)
      # The unrelated change really happened and was simply not part of the output.
      expect(invoice.customer.name).to eq 'Globex'
    end

    it 'rejects if an object is captured while an update is in progress' do
      invoice = build_invoice
      started = Queue.new
      release = Queue.new
      thread = Thread.new do
        invoice.recalculate(300, { sku: 'C', amount: 200 }) do |event|
          next unless event == :started

          started << true
          release.pop
        end
      end
      started.pop
      begin
        expect do
          InvoiceEntity.represent_consistent(invoice, max_attempts: 3)
        end.to raise_error(Grape::Entity::UnstableVersion, /in progress|unstable/i)
      ensure
        release << true
        thread.join
      end
    end
  end

  describe 're-reading declared fields' do
    it 'reports attempts and read counts through an injected coordinator' do
      invoice = build_invoice
      coordinator = Grape::Entity::ConsistencyCoordinator.new
      commit_update_between_getters!(invoice, 200, { sku: 'B', amount: 100 })

      output = InvoiceEntity.represent_consistent(
        invoice,
        consistency_coordinator: coordinator,
        max_attempts: 3
      )

      expect(output[:total]).to eq 200
      # Exactly two full passes: one rejected, one accepted.
      expect(coordinator.attempts).to eq 2
      expect(invoice.read_count(:total)).to eq 2
      expect(invoice.read_count(:lines)).to eq 2
    end

    it 'refuses to re-read a field that was not declared reread-able' do
      invoice = build_invoice
      coordinator = Grape::Entity::ConsistencyCoordinator.new
      commit_update_between_getters!(invoice, 200, { sku: 'B', amount: 100 })

      expect do
        RigidInvoiceEntity.represent_consistent(
          invoice,
          consistency_coordinator: coordinator
        )
      end.to raise_error(Grape::Entity::UnstableVersion, /re-?read|total/i)
      expect(coordinator.attempts).to eq 1
      # The declared getters were never invoked a second time.
      expect(invoice.read_count(:total)).to eq 1
      expect(invoice.read_count(:lines)).to eq 1
    end

    it 'stops after the configured number of attempts even with reread fields' do
      invoice = build_invoice
      coordinator = Grape::Entity::ConsistencyCoordinator.new
      # Keep changing the root on every lines read so every pass loses.
      ticks = 0
      invoice.with_barrier do |getter|
        next unless getter == :lines

        ticks += 1
        invoice.recalculate(100 + ticks, { sku: "B#{ticks}", amount: ticks }) {}
      end

      expect do
        InvoiceEntity.represent_consistent(
          invoice,
          consistency_coordinator: coordinator,
          max_attempts: 3
        )
      end.to raise_error(Grape::Entity::UnstableVersion, /3 attempt/)
      expect(coordinator.attempts).to eq 3
    end

    it 'does not treat ordinary exceptions as version contention' do
      klass = Class.new(Grape::Entity) do
        expose :boom do |_object, _options|
          raise 'kaboom'
        end
      end
      invoice = build_invoice
      expect do
        klass.represent_consistent(invoice, max_attempts: 3)
      end.to raise_error(RuntimeError, 'kaboom')
    end
  end

  describe 'nested collection membership' do
    it 'detects a change to a selected nested collection between capture and verify' do
      invoice = VersionedInvoice.new(
        total: 100,
        lines: [],
        customer: VersionedCustomer.new('Acme')
      )
      line = HookedLine.new('A', 100)
      invoice.lines << line

      armed = true
      line.define_singleton_method(:sku) do
        if armed
          armed = false
          # Fires inside the element getter, strictly after the collection
          # token was captured, synchronously (no timing-dependent threads).
          invoice.lines << HookedLine.new('Z', 5)
        end
        'A'
      end

      expect do
        RigidInvoiceEntity.represent_consistent(invoice, max_attempts: 3)
      end.to raise_error(Grape::Entity::UnstableVersion)
    end
  end
end
