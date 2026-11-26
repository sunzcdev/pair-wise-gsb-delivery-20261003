---
order: 1
---

# Brokers

To add a new broker you need to implement two methods `kick` and `listen` of the `taskiq.abc.broker.AsyncBroker` abstract class.
But along with them we have helper methods. Such as shutdown and startup.

Here is a template for new brokers:

@[code python](../examples/extending/broker.py)


# About kick and listen

The `kick` method takes a `BrokerMessage` as a parameter. The `BrokerMessage` class is a handy helper class for brokers. You can use information from the BrokerMessage to alter the delivery method.

::: warning "cool warning!"

As a broker developer, please send only raw bytes from the `message` field of a BrokerMessage if possible. Serializing it to the string may result in a problem if message bytes are not utf-8 compatible.

:::


## Acknowledgement

The `listen` method should yield raw bytes of a message.
But if your broker supports acking messages, the broker should return `taskiq.AckableMessage` with the required field.

For example:

```python

async def listen(self) -> AsyncGenerator[AckableMessage, None]:
   for message in self.my_channel:
      yield AckableMessage(
         data=message.bytes,
         # Ack is a function that takes no parameters.
         # So you either set here method of a message,
         # or you can make a closure.
         ack=message.ack,
      )
```

### Acknowledgement stage

The ack callable may accept one argument. In that case taskiq calls it with the stage at which the
delivery was actually acknowledged. The values come from `taskiq.AckStage` (for example
`when_received`, `when_executed`, `when_saved`, `no_result` or `manual`). Recording this stage lets
the broker build acknowledgement receipts that show how far a message got:

```python

def record_ack(stage: str) -> None:
    # Persist the real stage reached by the delivery.
    acknowledgement_receipts[message.id] = stage


yield AckableMessage(data=message.bytes, ack=record_ack)
```

A zero-argument ack callable remains fully supported, so existing broker integrations do not need
to change. Under the `when_saved_strict` policy, messages whose result write is unconfirmed (for
example a backend timeout) are never acknowledged, in which case the ack callable is not invoked at
all.

## Conventions

For brokers, we have several conventions. It's good if your broker implements them.
These rules are optional, and it's ok if your broker doesn't implement them.

1. If the message has the `delay` label with int or float number, this task's `execution` must be delayed
   with the same number of seconds as in the delay label.
2. If the message has the `priority` label, this message must be sent with priority. Tasks with
   higher priorities are executed sooner.
