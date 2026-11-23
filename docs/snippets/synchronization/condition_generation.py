from anyio import Condition, create_task_group, run


async def watch_config(condition: Condition, config: dict[str, str]) -> None:
    async with condition:
        last_generation = condition.generation
        while "endpoint" not in config:
            # Returns immediately if the config already changed since we last looked
            last_generation = await condition.wait_generation(last_generation)

        print("Observed configuration:", config)


async def main() -> None:
    condition = Condition()
    config: dict[str, str] = {}
    async with create_task_group() as tg:
        tg.start_soon(watch_config, condition, config)

        async with condition:
            config["endpoint"] = "https://example.org"
            condition.notify_all()


run(main)

# Output:
# Observed configuration: {'endpoint': 'https://example.org'}
