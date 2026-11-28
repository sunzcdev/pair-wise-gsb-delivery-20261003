# frozen_string_literal: true

RSpec.describe Shoryuken::Polling::WeightedRoundRobin do
  let(:queue1) { 'shoryuken' }
  let(:queue2) { 'uppercut' }
  let(:queues) { [] }
  subject { Shoryuken::Polling::WeightedRoundRobin.new(queues) }

  describe '#next_queue' do
    it 'cycles' do
      # [shoryuken, 2]
      # [uppercut,  1]
      queues << queue1
      queues << queue1
      queues << queue2

      expect(subject.next_queue).to eq(queue1)
      expect(subject.next_queue).to eq(queue2)
      expect(subject.next_queue).to eq(queue1)
    end

    it 'returns nil if there are no active queues' do
      expect(subject.next_queue).to eq(nil)
    end

    it 'unpauses queues whose pause is expired' do
      # [shoryuken, 2]
      # [uppercut,  1]
      queues << queue1
      queues << queue1
      queues << queue2

      allow(subject).to receive(:delay).and_return(10)

      now = Time.now
      allow(Time).to receive(:now).and_return(now)

      # pause the first queue
      subject.messages_found(queue1, 0)
      expect(subject.next_queue).to eq(queue2)

      now += 5
      allow(Time).to receive(:now).and_return(now)

      # pause the second queue
      subject.messages_found(queue2, 0)
      expect(subject.next_queue).to eq(nil)

      # queue1 should be unpaused now
      now += 6
      allow(Time).to receive(:now).and_return(now)
      expect(subject.next_queue).to eq(queue1)

      # queue1 should be unpaused and added to the end of queues now
      now += 6
      allow(Time).to receive(:now).and_return(now)
      expect(subject.next_queue).to eq(queue1)
      expect(subject.next_queue).to eq(queue2)
    end
  end

  describe '#messages_found' do
    it 'pauses a queue if there are no messages found' do
      # [shoryuken, 2]
      # [uppercut,  1]
      queues << queue1
      queues << queue1
      queues << queue2

      subject.messages_found(queue1, 0)
      expect(subject.active_queues).to eq([[queue2, 1]])
    end

    it 'increased the weight if message is found' do
      # [shoryuken, 2]
      # [uppercut,  1]
      queues << queue1
      queues << queue1
      queues << queue2

      expect(subject.active_queues).to eq([[queue1, 1], [queue2, 1]])
      subject.messages_found(queue1, 1)
      expect(subject.active_queues).to eq([[queue1, 2], [queue2, 1]])
    end

    it 'respects the maximum queue weight' do
      # [shoryuken, 2]
      # [uppercut,  1]
      queues << queue1
      queues << queue1
      queues << queue2

      subject.messages_found(queue1, 1)
      subject.messages_found(queue1, 1)
      expect(subject.active_queues).to eq([[queue1, 2], [queue2, 1]])
    end
  end

  describe '#poll_skipped' do
    it 'keeps the weight of a queue that was not polled' do
      queues << queue1
      queues << queue1
      queues << queue2

      selection = subject.next_queue
      expect(selection.name).to eq(queue1)

      subject.poll_skipped(selection)

      expect(subject.active_queues.to_h).to eq(queue1 => 1, queue2 => 1)
    end

    it 'does not pause the queue' do
      queues << queue1
      queues << queue2

      selection = subject.next_queue
      subject.poll_skipped(selection)

      expect(subject.active_queues.map(&:first)).to include(queue1)
      expect(subject.next_queue).not_to be_nil
    end

    it 'gives the unused turn back, so the skipped queue is the next candidate' do
      queues << queue1
      queues << queue2

      selection = subject.next_queue
      subject.poll_skipped(selection)

      expect(subject.next_queue.name).to eq(queue1)
      expect(subject.next_queue.name).to eq(queue2)
    end

    it 'does not lower the weight grown by an earlier poll' do
      queues << queue1
      queues << queue1
      queues << queue2

      subject.messages_found(queue1, 2)
      expect(subject.active_queues).to eq([[queue1, 2], [queue2, 1]])

      selection = subject.next_queue
      subject.poll_skipped(selection)

      expect(subject.active_queues.to_h).to eq(queue1 => 2, queue2 => 1)
    end

    it 'keeps the recovered weight available for the next polls of the queue' do
      queues << queue1
      queues << queue1
      queues << queue2

      subject.messages_found(queue1, 2)

      # The group runs out of capacity right after the queue recovered its weight
      skipped = subject.next_queue
      expect(skipped.name).to eq(queue1)
      subject.poll_skipped(skipped)

      # The skipped turn is given back: the queue still leads with its recovered weight of 2
      names = Array.new(3) { subject.next_queue.name }
      expect(names).to eq([queue1, queue2, queue1])
    end
  end

  describe 'stale reports' do
    it 'ignores an empty report for a queue that has already been selected again' do
      queues << queue1
      queues << queue2

      first = subject.next_queue
      subject.messages_found(first, 2) # queue1 recovers and grows to its weight

      second = subject.next_queue # a newer selection of queue2
      expect(second.name).to eq(queue2)

      # A late answer for the first selection must not change the newer state
      subject.messages_found(first, 0)

      expect(subject.active_queues.to_h).to eq(queue1 => 1, queue2 => 1)
    end

    it 'does not pause a queue that recovered after an empty answer was requested' do
      queues << queue1
      queues << queue1
      queues << queue2

      stale = subject.next_queue
      expect(stale.name).to eq(queue1)

      # queue1 recovers in a later selection before the first answer arrives
      recovered = subject.next_queue
      expect(recovered.name).to eq(queue2)
      subject.messages_found(queue1, 2)

      subject.messages_found(stale, 0)

      expect(subject.active_queues.to_h).to eq(queue1 => 2, queue2 => 1)
    end

    it 'ignores a report applied twice for the same selection' do
      queues << queue1
      queues << queue1

      selection = subject.next_queue
      subject.messages_found(selection, 2)
      weight_after_first = subject.active_queues.to_h[queue1]

      subject.messages_found(selection, 2)

      expect(subject.active_queues.to_h[queue1]).to eq(weight_after_first)
    end

    it 'ignores an empty report from before a processed message resumed the queue' do
      queues << queue1
      queues << queue2

      selection = subject.next_queue
      subject.messages_found(selection, 0) # queue1 is paused by its own empty answer
      expect(subject.active_queues.to_h).to eq(queue2 => 1)

      subject.message_processed(queue1) # a message already in flight was processed
      expect(subject.next_queue).not_to be_nil # implicitly unpauses queue1
      expect(subject.active_queues.to_h).to eq(queue1 => 1, queue2 => 1)

      # A second, older empty report for the same selection must not pause queue1 again
      subject.messages_found(selection, 0)
      expect(subject.active_queues.to_h).to eq(queue1 => 1, queue2 => 1)
    end

    it 'does not restart the backoff of an unpaused queue from the report that paused it' do
      queues << queue1
      queues << queue2

      allow(subject).to receive(:delay).and_return(10)
      now = Time.now
      allow(Time).to receive(:now).and_return(now)

      selection = subject.next_queue
      subject.messages_found(selection, 0)
      expect(subject.active_queues.to_h).to eq(queue2 => 1)

      now += 11
      allow(Time).to receive(:now).and_return(now)
      expect(subject.next_queue).not_to be_nil # the backoff has passed

      # Replaying the old empty answer must not pause the queue for another delay
      subject.messages_found(selection, 0)
      expect(subject.active_queues.map(&:first)).to include(queue1)
    end
  end

  describe '#delay' do
    it 'sets delay based on group' do
      delay_polling = Shoryuken::Polling::WeightedRoundRobin.new(queues, 25)
      expect(delay_polling.delay).to eq(25.0)
      expect(subject.delay).to eq(1.0)
    end
  end

  describe '#message_processed' do
    it 'removes delay from paused queue' do
      queues << queue1
      queues << queue2

      expect(subject.next_queue).to eq(queue1)
      subject.messages_found(queue1, 0) # pauses queue1

      expect(subject.active_queues).to eq([[queue2, 1]])

      subject.message_processed(queue1) # marks queue1 to be unpaused

      expect(subject.next_queue).to eq(queue2) # implicitly unpauses queue1
      expect(subject.active_queues).to eq([[queue1, 1], [queue2, 1]])
    end

    it 'preserves weight of queues when unpausing' do
      queues << queue1
      queues << queue1
      queues << queue2

      expect(subject.next_queue).to eq(queue1)
      subject.messages_found(queue1, 1)

      expect(subject.next_queue).to eq(queue2)
      subject.messages_found(queue2, 0) # pauses queue2

      expect(subject.active_queues).to eq([[queue1, 2]])
      subject.message_processed(queue2) # marks queue2 to be unpaused

      expect(subject.next_queue).to eq(queue1) # implicitly unpauses queue2
      expect(subject.active_queues).to eq([[queue1, 2], [queue2, 1]])
    end

    it 'unpauses a processed queue stuck behind an earlier-paused queue' do
      queues << queue1
      queues << queue2

      allow(subject).to receive(:delay).and_return(10)
      now = Time.now
      allow(Time).to receive(:now).and_return(now)

      subject.messages_found(queue1, 0) # queue1 paused until now + 10

      now += 1
      allow(Time).to receive(:now).and_return(now)
      subject.messages_found(queue2, 0) # queue2 paused until now + 10 (after queue1)

      # both queues paused, nothing pollable yet
      expect(subject.next_queue).to eq(nil)

      # queue2 finishes processing and is marked ready while queue1 is still paused
      subject.message_processed(queue2)

      # queue2 must be pollable even though the earlier-paused queue1 is still paused
      expect(subject.next_queue).to eq(queue2)
    end
  end

  describe 'thread safety' do
    it 'serializes concurrent next_queue, messages_found and message_processed' do
      queues << queue1
      queues << queue2

      errors = []
      errors_mutex = Mutex.new

      threads = Array.new(6) do
        Thread.new do
          50.times do |i|
            subject.next_queue
            subject.messages_found(queue1, i.even? ? 0 : 1)
            subject.message_processed(queue1)
            subject.active_queues
          end
        rescue => e
          errors_mutex.synchronize { errors << e }
        end
      end
      threads.each(&:join)

      expect(errors).to be_empty
    end
  end
end
