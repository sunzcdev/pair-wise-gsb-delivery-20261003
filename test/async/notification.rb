# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2018-2026, by Samuel Williams.

require "sus/fixtures/async"
require "async/notification"

require "async/a_condition"

describe Async::Notification do
	include Sus::Fixtures::Async::ReactorContext
	
	let(:notification) {subject.new}
	
	it "should continue after notification is signalled" do
		sequence = []
		
		task = reactor.async do
			sequence << :waiting
			notification.wait
			sequence << :resumed
		end
		
		expect(task.status).to be == :running
		
		sequence << :running
		# This will cause the task to exit:
		notification.signal
		sequence << :signalled
		
		expect(task.status).to be == :running
		
		reactor.yield
		task.wait
		sequence << :finished
		
		expect(task.status).to be == :completed
		
		expect(sequence).to be == [
			:waiting,
			:running,
			:signalled,
			:resumed,
			:finished
		]
	end
	
	it_behaves_like Async::ACondition
	
	with "notification receipts" do
		let(:receipt) {notification.receipt}
		
		it "settles the receipt when the deferred notification is transferred" do
			task = reactor.async do
				notification.wait
			end
			
			expect(notification.signal(:value, receipt: receipt)).to be == true
			
			# The notification is delivered on the next iteration of the reactor:
			expect(receipt.states).to be == [Async::Condition::Receipt::WAITING]
			expect(receipt).not.to be(:settled?)
			
			expect(task.wait).to be == :value
			
			expect(receipt.states).to be == [Async::Condition::Receipt::SIGNALLED]
			expect(receipt).to be(:settled?)
		end
		
		it "settles the receipt immediately when there is nothing to notify" do
			expect(notification.signal(:value, receipt: receipt)).to be == false
			
			expect(receipt).to be(:settled?)
			expect(receipt.states).to be == []
		end
	end
end
