from anyio import NamedTemporaryFile, run


async def main() -> None:
    temp_file = NamedTemporaryFile[str](mode="w+", encoding="utf-8")
    async with temp_file as f:
        await f.write("these contents will be published\n")
        await temp_file.publish("final_output.txt")

    # "final_output.txt" now exists; the temporary file has been cleaned up


run(main)