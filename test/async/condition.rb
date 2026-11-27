# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2017, by Kent Gruber.
# Copyright, 2017-2026, by Samuel Williams.

require "sus/fixtures/async"
require "async/condition"

require "async/a_condition"

describe Async::Condition do
	include Sus::Fixtures::Async::ReactorContext
	
	let(:condition) {subject.new}
	
	it "should continue after condition is signalled" do
		task = reactor.async do
			condition.wait
		end
		
		expect(task).to be(:running?)
		
		condition.signal
		
		task.wait
		
		expect(task).to be(:completed?)
	end
	
	it "can stop nested task" do
		producer = nil
		
		consumer = reactor.async do |task|
			condition = Async::Condition.new
			
			producer = task.async do |subtask|
				subtask.yield
				condition.signal
				sleep(10)
			end
			
			condition.wait
			expect do
				producer.stop
			end.not.to raise_exception
		end
		
		consumer.wait
		producer.wait
		
		expect(producer).to be(:cancelled?)
		expect(consumer.status).to be == :completed
	end
	
	it_behaves_like Async::ACondition
	
	with "notification receipts" do
		let(:receipt) {condition.receipt}
		let(:order) {[]}
		
		it "reports the terminal state of each waiter in a round" do
			first = reactor.async do
				order << [:first, condition.wait]
				# Re-register immediately, binding this fiber to the next notification:
				order << [:first_again, condition.wait]
			end
			
			second = reactor.async do
				order << [:second, condition.wait]
			end
			
			third = reactor.async do
				order << [:third, condition.wait]
			end
			
			condition.signal(:round1, receipt: receipt)
			
			# Cancel the second waiter before it can consume the value:
			second.cancel
			
			reactor.yield
			
			expect(receipt).to be(:settled?)
			expect(receipt.states).to be == [
				Async::Condition::Receipt::RE_REGISTERED,
				Async::Condition::Receipt::CANCELLED,
				Async::Condition::Receipt::SIGNALLED,
			]
			
			expect(receipt.values).to be == [:round1, nil, :round1]
			
			expect(receipt.re_registered.size).to be == 1
			expect(receipt.cancelled.size).to be == 1
			expect(receipt.signalled.size).to be == 2
			
			# The cancelled waiter never received the value:
			expect(order).to be == [
				[:first, :round1],
				[:third, :round1],
			]
			
			second.wait
			expect(second).to be(:cancelled?)
		end
		
		it "binds waiters registered after the signal to the next round" do
			first = reactor.async do
				condition.wait
			end
			
			condition.signal(:round1, receipt: receipt)
			
			# This waiter registers after the first round started:
			second = reactor.async do
				condition.wait
			end
			
			expect(receipt.states).to be == [Async::Condition::Receipt::SIGNALLED]
			
			second_receipt = condition.receipt
			condition.signal(:round2, receipt: second_receipt)
			
			expect(first.wait).to be == :round1
			expect(second.wait).to be == :round2
			
			expect(receipt.states).to be == [Async::Condition::Receipt::SIGNALLED]
			expect(second_receipt.states).to be == [Async::Condition::Receipt::SIGNALLED]
		end
		
		it "does not block waiters when the observation is cancelled" do
			waiter = reactor.async do
				condition.wait
			end
			
			observer = reactor.async do
				receipt.wait
				:observed
			end
			
			expect(observer).to be(:running?)
			
			# Cancelling the observation does not affect the waiter:
			observer.cancel
			observer.wait
			
			expect(observer).to be(:cancelled?)
			expect(receipt).not.to be(:settled?)
			
			condition.signal(:value, receipt: receipt)
			
			expect(waiter.wait).to be == :value
			
			expect(receipt).to be(:settled?)
			expect(receipt.states).to be == [Async::Condition::Receipt::SIGNALLED]
		end
		
		it "settles immediately when a round has no waiters" do
			expect(receipt).not.to be(:settled?)
			
			condition.signal(:value, receipt: receipt)
			
			expect(receipt).to be(:settled?)
			expect(receipt).to be(:empty?)
			expect(receipt.states).to be == []
		end
	end
end
