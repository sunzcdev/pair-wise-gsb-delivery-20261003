---
order: 5
---

# Schedule source

Schedule sources are used to get schedule for tasks.
To create new `schedule source` you have to implement the `taskiq.abc.schedule_source.ScheduleSource` abstract class.

Here's a minimal example of a schedule source:

@[code python](../examples/extending/schedule_source.py)

You can implement a schedule source that writes schedules in the database and have delayed tasks in runtime.

::: warning Verification before sending
`pre_send` may wait on an external confirmation, and a schedule definition can be
replaced in the source while that check is in flight. Before handing a task to the
broker, the scheduler re-queries `get_schedules()` and compares the current
definition (by `schedule_id`) with the one that just passed `pre_send`:

- if the definition changed, the payload is refreshed and `pre_send` runs again
  for the new definition;
- if the schedule is gone (or the source fails to list schedules), the dispatch
  is skipped;
- only a definition that is still current is sent, and `post_send` receives
  exactly the version that was committed to the broker.

Make sure your `get_schedules()` returns stable `schedule_id` values for
unchanged schedules, otherwise their dispatches will be skipped.
:::

::: info Cool tip!
You can also use `LabelScheduleSource` as a base class for your schedule source
if you want to parse schedules from task labels and don't want to implement logic for this from scratch.
:::
