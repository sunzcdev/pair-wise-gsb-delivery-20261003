# frozen_string_literal: true

require_relative '../spec_helper'

RSpec.describe Mail::Thread do
  def message(id:, subject: id, references: nil, in_reply_to: nil, body: id)
    references = format_message_ids(references) if references
    in_reply_to = format_message_ids(in_reply_to) if in_reply_to

    Mail.new do |mail|
      mail.message_id = "<#{id}>"
      mail.subject = subject
      mail.references = references if references
      mail.in_reply_to = in_reply_to if in_reply_to
      mail.body = body
    end
  end

  def format_message_ids(value)
    return value if value.include?('<')

    value.split(/\s+/).map { |message_id| "<#{message_id}>" }.join(' ')
  end

  def dump(thread)
    thread.roots.map { |node| dump_node(node) }
  end

  def dump_node(node)
    {
      id: node.id,
      placeholder: node.placeholder?,
      duplicate: node.duplicate?,
      parent: node.parent && node.parent.id,
      children: node.children.map { |child| dump_node(child) },
      messages: node.messages.map(&:subject)
    }
  end

  def dump_conflicts(thread)
    thread.conflicts.map do |conflict|
      {
        type: conflict.type,
        message_id: conflict.message_id,
        parent_id: conflict.parent_id,
        rejected_parent_id: conflict.rejected_parent_id,
        details: conflict.details
      }
    end
  end

  def fixed_shuffle(values)
    shuffled = values.dup
    steps = [3, 6, 2, 7, 4, 0, 5, 1]

    (shuffled.length - 1).downto(1) do |index|
      target = steps.fetch(index, index * 3 + 1) % (index + 1)
      shuffled[index], shuffled[target] = shuffled[target], shuffled[index]
    end

    shuffled
  end

  let(:root) { message(id: 'a', subject: 'topic') }
  let(:reply) { message(id: 'b', subject: 're topic', references: 'a', in_reply_to: 'a') }
  let(:grandchild) do
    message(id: 'c', subject: 're re topic', references: 'a b', in_reply_to: 'b')
  end
  let(:other_reply) do
    message(id: 'd', subject: 'unrelated', references: 'a', in_reply_to: 'a')
  end
  let(:orphan) do
    message(id: 'e', subject: 'late child', references: '<missing> <a>', in_reply_to: '<missing>')
  end
  let(:cyclic_one) { message(id: 'x', subject: 'cycle x', in_reply_to: 'y') }
  let(:cyclic_two) { message(id: 'y', subject: 'cycle y', in_reply_to: 'x') }
  let(:duplicate_first) { message(id: 'dup', subject: 'dup first', body: 'first') }
  let(:duplicate_second) { message(id: 'dup', subject: 'dup second', body: 'second') }
  let(:duplicate_child) do
    message(id: 'dup-child', subject: 'dup child', references: 'dup', in_reply_to: 'dup')
  end

  let(:messages) do
    [
      root,
      reply,
      grandchild,
      other_reply,
      orphan,
      cyclic_one,
      cyclic_two,
      duplicate_first,
      duplicate_second,
      duplicate_child
    ]
  end

  it 'builds the same tree for every fixed input order' do
    expected = dump(described_class.build(messages))

    [
      messages.reverse,
      messages.rotate(4),
      messages.rotate(-3),
      fixed_shuffle(messages)
    ].each do |order|
      expect(dump(described_class.build(order))).to eq(expected)
      expect(dump_conflicts(described_class.build(order))).to eq(dump_conflicts(described_class.build(messages)))
    end
  end

  it 'promotes a placeholder when its parent arrives and matches a full build' do
    order = [orphan, reply, grandchild, other_reply, cyclic_one, cyclic_two, duplicate_first, duplicate_second, duplicate_child]
    incremental = described_class.new

    order.each do |mail|
      incremental.add(mail)
    end

    expect(incremental.find('missing')).to be_placeholder
    incremental.add(root)

    full = described_class.build(messages)

    expect(dump(incremental)).to eq(dump(full))
    expect(dump_conflicts(incremental)).to eq(dump_conflicts(full))
    expect(incremental.find('missing')).to be_placeholder
    expect(incremental.find('missing').parent).to be_nil
    expect(incremental.find('a').parent.id).to eq('missing')
    expect(incremental.find('a')).not_to be_placeholder
  end

  it 'matches a full build when messages are incrementally added in fixed orders' do
    full_structure = dump(described_class.build(messages))
    full_conflicts = dump_conflicts(described_class.build(messages))

    [messages.reverse, messages.rotate(4), fixed_shuffle(messages)].each do |order|
      thread = described_class.new

      order.each do |mail|
        thread.add(mail)
      end

      expect(dump(thread)).to eq(full_structure)
      expect(dump_conflicts(thread)).to eq(full_conflicts)
    end
  end

  it 'uses References before In-Reply-To without joining similar subjects' do
    unrelated = message(id: 'u', subject: 'topic', body: 'different')
    conflicting = message(
      id: 'f',
      subject: 're topic',
      references: 'a b',
      in_reply_to: 'u'
    )
    thread = described_class.build(conflicting, unrelated, reply, root)

    expect(thread.find('f').parent.id).to eq('b')
    expect(thread.roots.map(&:id)).to eq(['a', 'u'])
    expect(thread.find('u').children).to be_empty
  end

  it 'creates stable placeholders for missing ancestors' do
    thread = described_class.build(orphan)

    expect(thread.find('missing')).to be_placeholder
    expect(thread.find('missing').children.map(&:id)).to eq(['a'])
    expect(thread.find('a')).to be_placeholder
    expect(thread.find('a').children.map(&:id)).to eq(['e'])
    expect(thread.find('e').parent.id).to eq('a')
  end

  it 'deduplicates repeated Message-IDs canonically and retains conflict details' do
    thread = described_class.build(duplicate_second, duplicate_first, duplicate_child)

    expect(thread.find('dup').message.body.to_s).to eq('second')
    expect(thread.find('dup').messages).to eq([duplicate_second, duplicate_first])
    expect(thread.find('dup').children.map(&:id)).to eq(['dup-child'])

    conflict = thread.conflicts.find { |item| item.type == :duplicate_message_id }
    expect(conflict.message_id).to eq('dup')
    expect(conflict.details.fetch(:identical)).to eq(false)
    expect(conflict.description).to include('Duplicate Message-ID')
  end

  it 'breaks reference cycles at the deterministic largest child ID' do
    thread = described_class.build(cyclic_one, cyclic_two)

    expect(thread.find('y').parent).to be_nil
    expect(thread.find('x').parent.id).to eq('y')

    conflict = thread.conflicts.find { |item| item.type == :reference_cycle }
    expect(conflict.message_id).to eq('y')
    expect(conflict.rejected_parent_id).to eq('x')
    expect(conflict.details.fetch(:cycle)).to eq(%w[x y])
    expect(conflict.description).to include('Reference cycle')
  end

  it 'removes self references from relation chains while recording them' do
    first = message(id: 'z', references: 'p1 q z')
    second = message(id: 'w', references: 'p2 q w')
    thread = described_class.build(second, first)

    expect(thread.find('q').parent.id).to eq('p1')
    expect(thread.find('z').parent.id).to eq('q')
    expect(thread.find('w').parent.id).to eq('q')

    self_conflicts = thread.conflicts.select { |conflict| conflict.type == :self_reference }
    expect(self_conflicts.map(&:message_id)).to eq(%w[w z])

    parent_conflict = thread.conflicts.find { |conflict| conflict.type == :conflicting_parent }
    expect(parent_conflict.message_id).to eq('q')
    expect(parent_conflict.parent_id).to eq('p1')
    expect(parent_conflict.rejected_parent_id).to eq('p2')
    expect(parent_conflict.details.fetch(:sources)).to eq(['w'])
  end

  it 'exposes immutable graph results' do
    thread = described_class.build(reply, root)

    expect(thread.roots).to be_frozen
    expect(thread.nodes).to be_frozen
    expect(thread.conflicts).to be_frozen
    expect(thread.find('a').children).to be_frozen
    expect(thread.find('a').messages).to be_frozen
    expect { thread.roots << nil }.to raise_error(FrozenError)
    expect { thread.find('a').children << nil }.to raise_error(FrozenError)
  end

  it 'does not depend on Date or wall-clock time for ordering' do
    dated_root = Mail.new do |mail|
      mail.message_id = '<a>'
      mail.date = Time.at(0).utc.rfc2822
      mail.subject = 'a'
    end
    undated_reply = Mail.new do |mail|
      mail.message_id = '<b>'
      mail.references = 'a'
      mail.subject = 'b'
    end

    expect(dump(described_class.build(undated_reply, dated_root))).to eq(
      dump(described_class.build(dated_root, undated_reply))
    )
  end

  it 'uses a synthetic stable node for a missing Message-ID without generating one' do
    parent = message(id: 'parent', subject: 'parent')
    reply_without_id = Mail.new do |mail|
      mail.subject = 'reply without id'
      mail.in_reply_to = '<parent>'
      mail.body = 'reply'
    end
    thread = described_class.build(reply_without_id, parent)

    expect(reply_without_id.header.has_message_id?).to eq(false)
    expect(thread.find('parent').children.first.message).to equal(reply_without_id)
    expect(thread.find('parent').children.first.id).to start_with('mail-thread:synthetic:')
  end
end
