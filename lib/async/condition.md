A synchronization primitive, which allows fibers to wait until a particular condition is (edge) triggered. Zero or more fibers can wait on a condition. When the condition is signalled, the fibers will be resumed in order.

Each call to `signal` is a *round*: the fibers which are waiting at that moment are resumed with the signalled value, and fibers which register afterwards wait for the next notification. A notification is never queued up for a fiber which has not started waiting yet.

## Example

~~~ ruby
require 'async'

Sync do
	condition = Async::Condition.new
	
	Async do
		Console.info "Waiting for condition..."
		value = condition.wait
		Console.info "Condition was signalled: #{value}"
	end
	
	Async do |task|
		sleep(1)
		Console.info "Signalling condition..."
		condition.signal("Hello World")
	end
end
~~~

### Output

~~~
0.0s     info: Waiting for condition...
1.0s     info: Signalling condition...
1.0s     info: Condition was signalled: Hello World
~~~

## Notification receipts

The caller can optionally observe what actually happened to the waiters of a round by passing a receipt to `signal`. The receipt does not change the behaviour of the waiters: it only records whether each of them received the value, was cancelled before receiving it, or received it and immediately registered for the next notification.

~~~ ruby
require 'async'

Sync do |task|
	condition = Async::Condition.new
	
	first = task.async do
		value = condition.wait
		# Register for the next notification:
		next_value = condition.wait
	end
	
	second = task.async do
		condition.wait
	end
	
	third = task.async do
		condition.wait
	end
	
	# Yield once so all three fibers are blocked:
	task.yield
	
	receipt = condition.receipt
	condition.signal("Hello World", receipt: receipt)
	
	# Stop the second fiber before it can consume the value:
	second.stop
	
	receipt.wait
	
	receipt.states # => [:re_registered, :cancelled, :signalled]
	
	# Only the fiber which registered again is waiting now:
	condition.waiting_count # => 1
	
	second_receipt = condition.receipt
	condition.signal("Goodbye World", receipt: second_receipt)
	
	second_receipt.wait
	
	second_receipt.states # => [:signalled]
end
~~~

The receipt settles once every waiter it observed has reached a terminal state. Waiting for it is independent of the waiters themselves, so cancelling the task which is observing the receipt does not disturb any of them, and any number of tasks may observe the same receipt.

Note that a waiter is only reported as `:re_registered` once it actually comes back to wait; a fiber which receives the value and then performs work first will report `:signalled` until it registers again.
