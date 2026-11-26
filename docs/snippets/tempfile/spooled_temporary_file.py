from anyio import SpooledTemporaryFile, run


async def main() -> None:
    async with SpooledTemporaryFile(max_size=1024, mode="w+") as f:
        await f.write("Spooled temp file content")
        # At this point the data is still held in the in-memory buffer.

        # Force the handoff from the in-memory buffer to an on-disk file. Concurrent
        # reads and writes wait until this has completed, so nobody ever sees a
        # partially copied disk stream; the cursor position is preserved.
        await f.rollover()
        assert f.rollover_phase is SpooledTemporaryFile.RolloverPhase.ROLLED_OVER

        await f.seek(0)
        print(await f.read())


run(main)
