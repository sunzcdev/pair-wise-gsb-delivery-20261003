"""Worker process for the complete-mode concurrency integration test.

It runs one ``virtualenv --complete`` creation in a separate process. A counter file makes the seeder fail for the
first ``FAIL_TIMES`` invocations across every worker, so the test can force a seed failure and a later resume while
two processes compete for the same destination.
"""

from __future__ import annotations

import os
from pathlib import Path


def _install_failing_seeder() -> None:
    counter = Path(os.environ["COMPLETION_FAIL_COUNTER"])
    fail_times = int(os.environ.get("COMPLETION_FAIL_TIMES", "0"))
    if fail_times <= 0:
        return

    from virtualenv.seed.embed.via_app_data.via_app_data import FromAppData

    original_run = FromAppData.run

    def run(self, creator):
        attempt = int(counter.read_text(encoding="utf-8").strip() or "0") + 1 if counter.exists() else 1
        counter.write_text(str(attempt), encoding="utf-8")
        if attempt <= fail_times:
            raise RuntimeError("injected seeder failure from the complete-mode worker")
        return original_run(self, creator)

    FromAppData.run = run


def main() -> int:
    _install_failing_seeder()
    from virtualenv import cli_run  # imported after the patch so the CLI builds the patched seeder

    dest = os.environ["COMPLETION_TEST_DEST"]
    app_data = os.environ["COMPLETION_TEST_APP_DATA"]
    # -v surfaces the reuse/resume info messages the test asserts on
    session = cli_run(["-v", "--complete", "--app-data", app_data, dest])
    # the entry point the parent asserts on: the confirmed destination of the finished environment
    print(f"COMPLETED_DEST={session.creator.dest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
