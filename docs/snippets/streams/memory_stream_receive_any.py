from anyio import EndOfStream, create_memory_object_stream, create_task_group, run
from anyio.streams.memory import MemoryObjectReceiveStream, MemoryObjectSendStream


async def produce(name: str, send_stream: MemoryObjectSendStream[str]) -> None:
    async with send_stream:
        for number in range(3):
            await send_stream.send(f"{number} from {name}")


async def main() -> None:
    send_a, receive_a = create_memory_object_stream[str]()
    send_b, receive_b = create_memory_object_stream[str]()
    async with create_task_group() as tg, send_a, send_b, receive_a, receive_b:
        tg.start_soon(produce, "A", send_a)
        tg.start_soon(produce, "B", send_b)

        # Wait for whichever stream becomes readable first. A stream that has been
        # closed and drained is skipped as long as another candidate can still deliver.
        while True:
            try:
                item = await receive_a.receive_any(receive_b)
            except EndOfStream:
                break

            print("received", item)


run(main)
