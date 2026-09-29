# encoding: utf-8
# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Mail::Thread do
  def build_message(id, references: nil, in_reply_to: nil, subject: nil)
    lines = []
    lines << "Message-ID: <#{id}>" if id
    lines << "References: #{Array(references).map { |ref| "<#{ref}>" }.join(' ')}" if references
    lines << "In-Reply-To: #{Array(in_reply_to).map { |ref| "<#{ref}>" }.join(' ')}" if in_reply_to
    lines << "Subject: #{subject}" if subject
    Mail.new(lines.join("\r\n") + "\r\n\r\nbody")
  end

  def node_structure(node)
    [node.message_id, node.children.map { |child| node_structure(child) }]
  end

  def structure_of(thread)
    thread.roots.map { |root| node_structure(root) }
  end

  describe 'parent resolution' do
    it 'threads a References chain into ancestor order' do
      thread = Mail::Thread.new([
        build_message('a'),
        build_message('b', references: %w[a]),
        build_message('c', references: %w[a b])
      ])
      expect(structure_of(thread)).to eq [['a', [['b', [['c', []]]]]]]
    end

    it 'falls back to In-Reply-To when References is missing' do
      thread = Mail::Thread.new([
        build_message('p'),
        build_message('m', in_reply_to: %w[p])
      ])
      expect(structure_of(thread)).to eq [['p', [['m', []]]]]
    end

    it 'prefers References over In-Reply-To' do
      thread = Mail::Thread.new([
        build_message('x'),
        build_message('y'),
        build_message('m', references: %w[x], in_reply_to: %w[y])
      ])
      expect(thread['m'].parent.message_id).to eq 'x'
    end

    it 'uses the last id of a multi-id In-Reply-To and records a note' do
      thread = Mail::Thread.new([build_message('m', in_reply_to: %w[p q])])
      expect(thread['m'].parent.message_id).to eq 'q'
      expect(thread['m'].notes.join).to match(/multiple In-Reply-To/)
    end

    it 'never merges messages by subject similarity' do
      thread = Mail::Thread.new([
        build_message('m1', subject: 'Quarterly report'),
        build_message('m2', subject: 'Re: Quarterly report')
      ])
      expect(thread.roots.size).to eq 2
    end

    it 'threads by references even when Date headers are absent or bogus' do
      a = build_message('a')
      b = build_message('b', references: %w[a])
      a['Date'] = 'not a date at all'
      b['Date'] = 'definitely not a date either'
      thread = Mail::Thread.new([b, a])
      expect(structure_of(thread)).to eq [['a', [['b', []]]]]
    end

    it 'treats messages without Message-ID as unreferenceable nodes' do
      thread = Mail::Thread.new([build_message(nil), build_message(nil)])
      expect(thread.size).to eq 2
      expect(thread.roots.size).to eq 2
      expect(thread.find(nil)).to be_nil
    end
  end

  describe 'duplicate Message-IDs' do
    it 'keeps the first copy and records a note' do
      first = build_message('dup')
      second = build_message('dup')
      thread = Mail::Thread.new([first, second])
      expect(thread.size).to eq 1
      expect(thread['dup'].message).to equal first
      expect(thread['dup'].notes.join).to match(/duplicate Message-ID/)
    end
  end

  describe 'placeholder nodes' do
    it 'creates placeholders for missing ancestors' do
      thread = Mail::Thread.new([build_message('c', references: %w[a b])])
      expect(thread['a']).to be_placeholder
      expect(thread['b']).to be_placeholder
      expect(structure_of(thread)).to eq [['a', [['b', [['c', []]]]]]]
    end

    it 'fills placeholders when the parents arrive late' do
      thread = Mail::Thread.new
      thread.add(build_message('c', references: %w[a b]))
      thread.add(build_message('b', references: %w[a]))
      thread.add(build_message('a'))
      expect(thread['a']).not_to be_placeholder
      expect(thread['b']).not_to be_placeholder
      expect(structure_of(thread)).to eq [['a', [['b', [['c', []]]]]]]
    end

    it 'prunes placeholders that end up with neither message nor children' do
      thread = Mail::Thread.new
      thread.add(build_message('m', references: %w[x a]))
      thread.add(build_message('a'))
      expect(thread['x']).to be_nil
      expect(structure_of(thread)).to eq [['a', [['m', []]]]]
    end
  end

  describe 'conflict resolution' do
    it 'lets a real message\'s own headers override an inferred position' do
      thread = Mail::Thread.new
      thread.add(build_message('m', references: %w[x a]))
      thread.add(build_message('a'))
      expect(thread['a']).to be_root
      expect(thread['a'].notes.join).to match(/inferred parent/)
    end

    it 'resolves conflicting inferred parents by smaller parent id' do
      set = [
        build_message('m1', references: %w[x a]),
        build_message('m2', references: %w[y a])
      ]
      structures = [set, set.reverse].map { |messages| structure_of(Mail::Thread.new(messages)) }
      expect(structures.first).to eq structures.last
      expect(structures.first).to eq [['x', [['a', [['m1', []], ['m2', []]]]]]]
    end

    it 'aggregates conflict notes via #conflicts' do
      thread = Mail::Thread.new([
        build_message('dup'),
        build_message('dup'),
        build_message('a', references: %w[b]),
        build_message('b', references: %w[a])
      ])
      expect(thread.conflicts).not_to be_empty
      expect(thread.conflicts.join("\n")).to match(/duplicate Message-ID/)
      expect(thread.conflicts.join("\n")).to match(/cycle/)
    end
  end

  describe 'reference cycles' do
    it 'breaks cycles deterministically, keeping the smaller id as ancestor' do
      orders = [
        [build_message('a', references: %w[b]), build_message('b', references: %w[a])],
        [build_message('b', references: %w[a]), build_message('a', references: %w[b])]
      ]
      structures = orders.map { |messages| structure_of(Mail::Thread.new(messages)) }
      expect(structures.first).to eq structures.last
      expect(structures.first).to eq [['a', [['b', []]]]]
    end

    it 'records a note when breaking a cycle' do
      thread = Mail::Thread.new([
        build_message('a', references: %w[b]),
        build_message('b', references: %w[a])
      ])
      expect(thread.conflicts.join("\n")).to match(/reference cycle/)
    end

    it 'ignores self references' do
      thread = Mail::Thread.new([build_message('a', references: %w[a])])
      expect(thread['a']).to be_root
      expect(thread['a'].notes.join).to match(/self-reference/)
    end
  end

  describe 'order independence' do
    def sample_set
      [
        build_message('root'),
        build_message('child1', references: %w[root]),
        build_message('child2', references: %w[root child1]),
        build_message('grandchild', references: %w[root child1 child2]),
        build_message('orphan', references: %w[missing-parent]),
        build_message('via-irt', in_reply_to: %w[root]),
        build_message('dup'),
        build_message('dup'),
        build_message('cycle-x', references: %w[cycle-y]),
        build_message('cycle-y', references: %w[cycle-x]),
        build_message(nil)
      ]
    end

    it 'produces the same structure for shuffled input' do
      expected = structure_of(Mail::Thread.new(sample_set))
      5.times do |seed|
        shuffled = sample_set.shuffle(random: Random.new(seed))
        expect(structure_of(Mail::Thread.new(shuffled))).to eq expected
      end
    end

    it 'matches a full build when messages are added incrementally' do
      expected = structure_of(Mail::Thread.new(sample_set))
      5.times do |seed|
        thread = Mail::Thread.new
        sample_set.shuffle(random: Random.new(seed + 100)).each { |message| thread.add(message) }
        expect(structure_of(thread)).to eq expected
      end
    end
  end

  describe 'read-only result' do
    it 'returns frozen roots and children arrays' do
      thread = Mail::Thread.new([
        build_message('a'),
        build_message('b', references: %w[a])
      ])
      expect(thread.roots).to be_frozen
      expect(thread['a'].children).to be_frozen
    end

    it 'iterates over every node including placeholders' do
      thread = Mail::Thread.new([build_message('c', references: %w[a b])])
      expect(thread.map(&:message_id)).to eq %w[a b c]
    end
  end
end