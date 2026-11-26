---
order: 5
---

# Schedule source

Schedule sources are used to get schedule for tasks.
To create new `schedule source` you have to implement the `taskiq.abc.schedule_source.ScheduleSource` abstract class.

Here's a minimal example of a schedule source:

@[code python](../examples/extending/schedule_source.py)

You can implement a schedule source that writes schedules in the database and have delayed tasks in runtime.

## Priority

A source may declare a `priority` to take part in [source resolution](../guide/scheduling-tasks.md#multiple-sources).
Sources with a higher priority win for definitions sharing the same `schedule_id`, and
fields the winning definition leaves out are inherited from the lower priority sources.

::: info Cool tip!
You can also use `LabelScheduleSource` as a base class for your schedule source
if you want to parse schedules from task labels and don't want to implement logic for this from scratch.
:::
