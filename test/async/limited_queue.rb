# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025-2026, by Samuel Williams.
# Copyright, 2025, by Shopify Inc.
# Copyright, 2026, by Kyle Tate.

require "async/queue"

require "sus/fixtures/async"
require "async/a_queue"

limited_queue_new = Async::LimitedQueue.method(:new)

require "async/limited_queue"

describe Async::LimitedQueue do
	include Sus::Fixtures::Async::ReactorContext
	
	it_behaves_like Async::AQueue
	
	it "warns when constructed through async/queue" do
		verbose = $VERBOSE
		$VERBOSE = true
		
		warning = nil
		Async::LimitedQueue.define_singleton_method(:warn) do |*arguments, **options|
			warning = arguments.first
		end
		
		queue = limited_queue_new.call(1)
		
		expect(queue.limit).to be == 1
		expect(warning).to be =~ /require 'async\/limited_queue'/
	ensure
		singleton_class = class << Async::LimitedQueue; self; end
		singleton_class.remove_method(:warn) if singleton_class.instance_methods(false).include?(:warn)
		$VERBOSE = verbose
	end
	
	let(:queue) {subject.new}
	
	with "#limit"	do
		it "has a default limit of 1" do
			expect(queue.limit).to be == 1
		end
	end
	
	it "should become limited" do
		expect(queue).not.to be(:limited?)
		queue.enqueue(10)
		expect(queue).to be(:limited?)
	end
	
	it "enqueues items up to a limit" do
		items = Array.new(2){rand(10)}
		reactor.async do
			queue.enqueue(*items)
		end
		
		expect(queue.size).to be == 1
		expect(queue.dequeue).to be == items.first
	end
	
	with "#enqueue" do
		let(:queue) {subject.new(2)}
		
		it "commits a batch larger than the limit in segments without exceeding it" do
			items = [1, 2, 3, 4, 5]
			committed = []
			
			task = reactor.async do
				queue.enqueue(*items) do |count, item|
					committed << [count, item]
					# The queue must never hold more than its limit:
					expect(queue.size).to be <= queue.limit
				end
			end
			
			# Only the first segment fits before a consumer makes room:
			expect(queue.size).to be == 2
			expect(committed).to be == [[1, 1], [2, 2]]
			
			3.times { queue.dequeue }
			expect(task.wait).to be == 5
			expect(committed).to be == items.each_with_index.map { |item, index| [index + 1, item] }
			
			2.times { queue.dequeue }
			expect(queue).to be(:empty?)
		end
		
		it "returns the number of committed items" do
			queue = subject.new(10)
			
			expect(queue.enqueue(1, 2, 3)).to be == 3
			expect(queue.enqueue).to be == 0
		end
		
		it "keeps the committed segment when a blocked batch is cancelled" do
			batch = [1, 2, 3, 4, 5]
			committed = []
			
			producer = reactor.async do
				queue.enqueue(*batch) do |count, item|
					committed << [count, item]
				end
			rescue Async::Cancel
				:cancelled
			end
			
			# The first two items fit, then the batch blocks waiting for room:
			expect(queue.size).to be == 2
			expect(committed).to be == [[1, 1], [2, 2]]
			
			# A concurrent, ordinary sender queues up behind the blocked batch:
			normal = reactor.async do
				queue.enqueue(:normal)
			end
			
			expect(queue.waiting_count).to be == 2
			
			# Cancel the batch while it is blocked; nothing already accepted is lost:
			producer.stop
			expect(producer.wait).to be == :cancelled
			expect(queue.size).to be == 2
			expect(committed).to be == [[1, 1], [2, 2]]
			
			# Free a slot so the ordinary sender can proceed:
			expect(queue.dequeue).to be == 1
			normal.wait
			
			# The remaining order matches the commit receipts and the capacity is not over-counted:
			expect(queue.dequeue).to be == 2
			expect(queue.dequeue).to be == :normal
			expect(queue).to be(:empty?)
			expect(queue.size).to be == 0
			expect(queue).not.to be(:limited?)
		end
		
		it "does not let a cancelled batch reorder other senders" do
			producer = reactor.async do
				queue.enqueue(1, 2, 3, 4)
			rescue Async::Cancel
				:cancelled
			end
			
			expect(queue.size).to be == 2
			
			normal = reactor.async do
				queue.enqueue(:normal)
			end
			
			producer.stop
			producer.wait
			
			# Drain the two committed batch items, then the ordinary sender gets the slot:
			expect(queue.dequeue).to be == 1
			expect(queue.dequeue).to be == 2
			normal.wait
			expect(queue.dequeue).to be == :normal
			expect(queue).to be(:empty?)
		end
		
		it "raises ClosedError and keeps committed items when closed mid-batch" do
			committed = []
			
			task = reactor.async do
				queue.enqueue(1, 2, 3, 4) do |count, item|
					committed << [count, item]
				end
			end
			
			expect(queue.size).to be == 2
			
			queue.close
			
			expect do
				task.wait
			end.to raise_exception(Async::Queue::ClosedError)
			
			expect(committed).to be == [[1, 1], [2, 2]]
			expect(queue.dequeue).to be == 1
			expect(queue.dequeue).to be == 2
		end
	end
	
	it "should resume waiting tasks in order" do
		total_resumed = 0
		total_dequeued = 0
		
		Async do |producer|
			10.times do
				producer.async do
					queue.enqueue("foo")
					total_resumed += 1
				end
			end
		end
		
		10.times do
			queue.dequeue
			total_dequeued += 1
			
			expect(total_resumed).to be == total_dequeued
		end
	end
	
	with "#<<" do
		with "a limited queue" do
			def before
				queue << :item1
				expect(queue.size).to be == 1
				expect(queue).to be(:limited?)
				
				super
			end
			
			it "waits until a queue is dequeued" do
				reactor.async do
					queue << :item2
				end
				
				expect(queue.dequeue).to be == :item1
				expect(queue.dequeue).to be == :item2
			end
			
			with "#pop" do
				it "waits until a queue is dequeued" do
					reactor.async do
						queue << :item2
					end
					
					expect(queue.pop).to be == :item1
					expect(queue.pop).to be == :item2
				end
			end
		end
	end
	
	with "#close" do
		it "signals tasks waiting to enqueue items when closed" do
			queue.enqueue(:item1)
			
			# This task will block as the queue is full:
			waiting_task = reactor.async do
				queue.enqueue(:item2)
			end
			
			queue.close
			
			expect do
				waiting_task.wait
			end.to raise_exception(Async::Queue::ClosedError)
		end
	end
end
