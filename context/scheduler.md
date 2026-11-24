# Scheduler

This guide gives an overview of how the scheduler is implemented.

## Overview

The {ruby Async::Scheduler} uses an event loop to execute tasks. When tasks are waiting on blocking operations like IO, the scheduler will use the operating system's native event system to wait for the operation to complete. This allows the scheduler to efficiently handle many tasks.

### Tasks

Tasks are the building blocks of concurrent programs. They are lightweight and can be scheduled by the event loop. Tasks can be nested, and the parent task is used to determine the current reactor. Tasks behave like promises, in the sense you can wait on them to complete, and they might fail with an exception.

~~~ ruby
require 'async'

def sleepy(duration, task: Async::Task.current)
	task.async do |subtask|
		subtask.annotate "I'm going to sleep #{duration}s..."
		sleep duration
		puts "I'm done sleeping!"
	end
end

def nested_sleepy(task: Async::Task.current)
	task.async do |subtask|
		subtask.annotate "Invoking sleepy 5 times..."
		5.times do |index|
			sleepy(index, task: subtask)
		end
	end
end

Async do |task|
	task.annotate "Invoking nested_sleepy..."
	subtask = nested_sleepy
	
	# Print out all running tasks in a tree:
	task.print_hierarchy($stderr)
	
	# Kill the subtask
	subtask.cancel
end
~~~

### Thread Safety

Most methods of the reactor and related tasks are not thread-safe, so you'd typically have [one reactor per thread or process](https://github.com/socketry/async-container).

### Embedding Schedulers

{ruby Async::Scheduler#run} will run until the reactor runs out of work to do. To run a single iteration of the reactor, use {ruby Async::Scheduler#run_once}.

~~~ ruby
require 'async'

Console.logger.debug!
reactor = Async::Scheduler.new

# Run the reactor for 1 second:
reactor.async do |task|
	sleep 1
	puts "Finished!"
end

while reactor.run_once
	# Round and round we go!
end
~~~

You can use this approach to embed the reactor in another event loop. For some integrations, you may want to specify the maximum time to wait to {ruby Async::Scheduler#run_once}.

### Bounding Ready Work Per Iteration

Some tasks keep pushing their next chunk of work back onto the ready list instead of waiting on an event. Because the selector drains the entire ready list before polling for I/O, a group of such tasks can monopolise the event loop and starve sockets and timers of processing time.

Pass `max_ready_count:` to {ruby Async::Scheduler#initialize} to cap how many ready fibers are resumed per iteration. Any ready fibers beyond the cap keep their relative order and are deferred to the next iteration; the selector is still entered on every iteration with a zero duration, so registered I/O and timers continue to be serviced promptly.

```ruby
# Resume at most 8 ready fibers per iteration of the event loop:
scheduler = Async::Scheduler.new(max_ready_count: 8)
```

The cap is only applied when the attached selector's `#select` accepts a `max_ready_count:` keyword. Selectors which only accept a duration — including the selectors bundled with `io-event` — ignore the option and retain their default behaviour.

### Cancelling a Scheduler

{ruby Async::Scheduler#cancel} will cancel the current scheduler and all children tasks.

### Fiber Scheduler Integration

In order to integrate with native Ruby blocking operations, the {ruby Async::Scheduler} uses a {ruby Fiber::Scheduler} interface.

```ruby
require "async"

scheduler = Async::Scheduler.new
Fiber.set_scheduler(scheduler)

Fiber.schedule do
	puts "Hello World!"
end
```

## Design

### Optimistic vs Pessimistic Scheduling

There are two main strategies for scheduling tasks: optimistic and pessimistic. An optimistic scheduler is usually greedy and will try to execute tasks as soon as they are scheduled using a direct transfer of control flow. A pessimistic scheduler will schedule tasks into the event loop ready list and will only execute them on the next iteration of the event loop.

```ruby
Async do
	puts "Hello "
	
	Async do
		puts "World"
	end
	
	puts "!"
end
```

An optimstic scheduler will print "Hello World!", while a pessimistic scheduler will print "Hello !World". In practice you should not design your code to rely on the order of execution, but it's important to understand the difference. It is an unspecifed implementation detail of the scheduler.
