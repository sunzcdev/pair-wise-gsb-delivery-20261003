import pytest

from taskiq.acks import (
    AckController,
    AcknowledgeType,
    AckStage,
    parse_acknowledge_type,
)


def test_parse_acknowledge_type_from_enum() -> None:
    assert (
        parse_acknowledge_type(AcknowledgeType.WHEN_RECEIVED)
        == AcknowledgeType.WHEN_RECEIVED
    )


def test_parse_acknowledge_type_from_string() -> None:
    assert parse_acknowledge_type("when_executed") == AcknowledgeType.WHEN_EXECUTED


def test_parse_manual_acknowledge_type_from_string() -> None:
    assert parse_acknowledge_type("manual") == AcknowledgeType.MANUAL


def test_parse_strict_acknowledge_type_from_string() -> None:
    assert (
        parse_acknowledge_type("when_saved_strict") == AcknowledgeType.WHEN_SAVED_STRICT
    )


def test_parse_acknowledge_type_unknown_string() -> None:
    with pytest.raises(ValueError, match="Unknown acknowledge type value"):
        parse_acknowledge_type("when_save")


def test_parse_acknowledge_type_unsupported_value() -> None:
    with pytest.raises(ValueError, match="Unsupported acknowledge type"):
        parse_acknowledge_type(1)


async def test_ack_controller_passes_stage_to_aware_callable() -> None:
    recorded: list[str | None] = []

    async def ack(stage: str | None) -> None:
        recorded.append(stage)

    controller = AckController(ack)

    await controller.ack(AckStage.WHEN_SAVED)
    await controller.ack(AckStage.MANUAL)

    assert controller.is_acked
    assert controller.ack_stage == AckStage.WHEN_SAVED.value
    assert recorded == [AckStage.WHEN_SAVED.value]


async def test_ack_controller_supports_legacy_zero_argument_callable() -> None:
    calls = 0

    def ack() -> None:
        nonlocal calls
        calls += 1

    controller = AckController(ack)

    await controller.ack(AckStage.WHEN_SAVED)

    assert controller.is_acked
    assert controller.ack_stage == AckStage.WHEN_SAVED.value
    assert calls == 1


async def test_ack_controller_ack_without_stage_records_none() -> None:
    async def ack(stage: str | None) -> None:
        assert stage is None

    controller = AckController(ack)

    await controller.ack()

    assert controller.is_acked
    assert controller.ack_stage is None


async def test_ack_controller_rejects_ack_for_non_ackable_message() -> None:
    controller = AckController(None)

    assert not controller.is_ackable
    with pytest.raises(RuntimeError, match=r"Current message is not ackable\."):
        await controller.ack(AckStage.WHEN_SAVED)
