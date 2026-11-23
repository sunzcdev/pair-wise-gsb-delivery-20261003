from anyio import CheckpointBudgetExceeded, create_memory_object_stream, run
from anyio.streams.buffered import BufferedByteReceiveStream


async def main() -> None:
    send, receive = create_memory_object_stream[bytes](4)
    buffered = BufferedByteReceiveStream(receive)
    # The candidate prefix arrives in two separate reads, followed by a record body
    for part in b"PRE", b"FIX-", b"record-body":
        await send.send(part)

    # Speculatively try to read an optional prefix, bounded by a budget
    async with buffered.checkpoint(budget=8) as checkpoint:
        try:
            prefix = await buffered.receive_exactly(8)
        except CheckpointBudgetExceeded:
            # The budget ran out before a complete prefix arrived; nothing is lost,
            # because the checkpoint is rolled back when the block exits
            prefix = b""

        if prefix == b"PREFIX!!":
            # Keep the speculative read and continue parsing after the prefix
            checkpoint.commit()

    # After a rollback, a regular parser sees every byte the speculative reader saw,
    # straight from the buffer; the forward-only upstream stream is never re-read
    print(await buffered.receive_exactly(15))


run(main)
