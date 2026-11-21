from __future__ import annotations

import inspect

import pytest

import dramatiq
from dramatiq import Message, MessageContract, MessageContractError


def add_v1(x, y):
    """The old calling convention."""


def add_v2(x, y, z=0):
    """The new calling convention, backwards-compatible via a default."""


def test_contract_match_determines_which_versions_arguments_are_valid_for():
    # Given a contract with two versions
    contract = MessageContract({1: add_v1, 2: add_v2})

    # Old-format positional arguments are valid for both versions
    assert contract.match((1, 2), {}) == [1, 2]

    # Keyword arguments that use the new parameter are only valid for v2
    assert contract.match((1, 2), {"z": 3}) == [2]

    # Arguments missing a required parameter match no version
    assert contract.match((1,), {}) == []

    # And the declared versions are introspectable
    assert contract.versions == (1, 2)
    assert contract.current_version == 2


def test_contract_accepts_signature_objects_and_unversioned_specs():
    # Given a contract built from inspect.Signature objects
    signature = inspect.signature(add_v2)
    contract = MessageContract({"current": signature})
    assert contract.match((1, 2), {"z": 3}) == ["current"]

    # And an unversioned contract built from a single callable
    contract = MessageContract(add_v1)
    assert contract.match((1, 2), {}) == [None]
    assert contract.match((1, 2, 3), {}) == []

    # But an empty version mapping is rejected
    with pytest.raises(ValueError):
        MessageContract({})


def test_contract_validate_reports_missing_parameter_positions():
    contract = MessageContract({1: add_v1, 2: add_v2})

    with pytest.raises(MessageContractError) as exc_info:
        contract.validate("add", args=(1,))

    error = exc_info.value
    assert error.actor_name == "add"
    assert (1, "y") in error.missing
    assert "add" in str(error)
    assert "'y'" in str(error)
    assert "position 1" in str(error)


def test_actors_without_contracts_keep_their_old_behavior(stub_broker, stub_worker):
    # Given an actor that declares no contract
    received = []

    @dramatiq.actor
    def do_work(x, y=0):
        received.append((x, y))

    # When I send it messages the old way, both positionally and by keyword
    do_work.send(1, 2)
    do_work.send(1, y=3)
    do_work.send_with_options(args=(4,), kwargs={"y": 5})

    # Then all of them are admitted and run
    stub_broker.join(do_work.queue_name)
    assert received == [(1, 2), (1, 3), (4, 5)]


def test_actor_with_contract_admits_old_and_new_format_messages(stub_broker, stub_worker):
    # Given an actor that declares a versioned contract
    received = []

    @dramatiq.actor(contract={1: add_v1, 2: add_v2})
    def add(x, y, z=0):
        received.append((x, y, z))

    # When I send an old-format message (positional, without the new argument)
    add.send(1, 2)

    # And a message that fills the new argument's default by keyword
    add.send(1, y=2)

    # And a new-format message using the new keyword
    add.send(1, 2, z=3)

    # Then all of them are admitted and runnable
    stub_broker.join(add.queue_name)
    assert received == [(1, 2, 0), (1, 2, 0), (1, 2, 3)]


def test_actor_with_contract_rejects_incompatible_messages(stub_broker):
    # Given an actor that declares a contract
    @dramatiq.actor(contract={1: add_v1, 2: add_v2})
    def add(x, y, z=0):
        pass

    # When I try to send a message that is missing a required argument
    with pytest.raises(MessageContractError) as exc_info:
        add.send(1)

    # Then the error names the actor and the missing position
    error = exc_info.value
    assert error.actor_name == "add"
    assert (1, "y") in error.missing
    assert "add" in str(error)
    assert "'y'" in str(error)

    # And nothing was admitted to the queue
    assert stub_broker.queues[add.queue_name].qsize() == 0


def test_actor_with_contract_rejects_unknown_keyword_arguments(stub_broker):
    @dramatiq.actor(contract={1: add_v1})
    def add(x, y):
        pass

    with pytest.raises(MessageContractError):
        add.send(1, 2, nonexistent=3)

    assert stub_broker.queues[add.queue_name].qsize() == 0


def test_broker_enqueue_validates_messages_directly(stub_broker):
    # Given an actor with a contract, retries re-enter the broker
    # through broker.enqueue, so that entry point must validate too.
    @dramatiq.actor(contract={1: add_v1, 2: add_v2})
    def add(x, y, z=0):
        pass

    # When I enqueue an incompatible message directly on the broker
    message = add.message(1)
    with pytest.raises(MessageContractError):
        stub_broker.enqueue(message)

    # Then it is not admitted
    assert stub_broker.queues[add.queue_name].qsize() == 0


def test_old_format_wire_messages_are_admitted_and_runnable(stub_broker, stub_worker):
    # Given an actor whose new version adds a defaulted parameter
    received = []

    @dramatiq.actor(contract={1: add_v1, 2: add_v2})
    def add(x, y, z=0):
        received.append((x, y, z))

    # When an old-format message arrives off the wire (as produced by
    # an old producer during a rolling deploy)
    old_format_message = Message(
        queue_name=add.queue_name,
        actor_name=add.actor_name,
        args=(1, 2),
        kwargs={},
        options={},
    )
    decoded = Message.decode(old_format_message.encode())
    stub_broker.enqueue(decoded)

    # Then it is admitted and the actor runs with the default in place
    stub_broker.join(add.queue_name)
    assert received == [(1, 2, 0)]


def test_valid_messages_can_be_retried_through_the_broker(stub_broker, stub_worker):
    # Given an actor with a contract that fails once to begin with
    attempts = []

    @dramatiq.actor(contract={1: add_v1, 2: add_v2}, max_retries=2)
    def add(x, y, z=0):
        attempts.append((x, y, z))
        if len(attempts) < 2:
            raise dramatiq.Retry(delay=10)

    # When I send it a valid message and it is retried
    add.send(1, 2, z=3)

    # Then the retried message is re-admitted and eventually runs
    stub_broker.join(add.queue_name)
    assert attempts == [(1, 2, 3), (1, 2, 3)]


def test_contract_with_keyword_only_parameters(stub_broker, stub_worker):
    # Given a contract whose new version adds a keyword-only parameter
    def work_v1(x):
        pass

    def work_v2(x, *, flag=False):
        pass

    received = []

    @dramatiq.actor(contract={1: work_v1, 2: work_v2})
    def do_work(x, flag=False):
        received.append((x, flag))

    # Old-format messages and new keyword messages are both admitted
    do_work.send(1)
    do_work.send(2, flag=True)

    stub_broker.join(do_work.queue_name)
    assert received == [(1, False), (2, True)]
