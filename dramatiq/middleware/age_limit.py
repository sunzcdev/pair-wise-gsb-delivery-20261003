# This file is a part of Dramatiq.
#
# Copyright (C) 2017,2018 CLEARTYPE SRL <bogdan@cleartype.io>
#
# Dramatiq is free software; you can redistribute it and/or modify it
# under the terms of the GNU Lesser General Public License as published by
# the Free Software Foundation, either version 3 of the License, or (at
# your option) any later version.
#
# Dramatiq is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
# FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser General Public
# License for more details.
#
# You should have received a copy of the GNU Lesser General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.

from __future__ import annotations

from typing import Optional

from ..common import current_millis
from ..logging import get_logger
from .middleware import Middleware, SkipMessage


class AgeLimit(Middleware):
    """Middleware that drops messages that have been in the queue for
    too long.

    Parameters:
      max_age(int): The default message age limit in milliseconds.
        Defaults to ``None``, meaning that messages can exist
        indefinitely.
      max_age_from_due(bool): Whether to measure message age from the
        first time the message became due (i.e. the moment it left the
        delay queue) rather than from when it was submitted.  This is
        useful for messages that are scheduled far in advance: the
        time they spend waiting in the delay queue doesn't count
        against their age limit.  The original submission time is
        always preserved on ``message_timestamp`` for diagnostics.
        Defaults to ``False``.
    """

    def __init__(self, *, max_age: Optional[int] = None, max_age_from_due: bool = False) -> None:
        self.logger = get_logger(__name__, type(self))
        self.max_age = max_age
        self.max_age_from_due = max_age_from_due

    @property
    def actor_options(self):
        return {"max_age", "max_age_from_due"}

    def _age_from_due(self, actor, message) -> bool:
        return message.options.get(
            "max_age_from_due",
            actor.options.get("max_age_from_due", self.max_age_from_due),
        )

    def before_enqueue(self, broker, message, delay):
        # Record the first due time of opted-in messages so that their
        # age can later be measured from it.  The broker has already
        # stamped "eta" onto the message's options by this point.
        #
        # The baseline is only ever written once: re-enqueues without a
        # delay (e.g. the worker promoting the message off the delay
        # queue) return early, and redeliveries/retries that carry the
        # option forward must not move the start of the window to the
        # redelivery time.
        if delay is None or "due_timestamp" in message.options:
            return

        actor = broker.get_actor(message.actor_name)
        if self._age_from_due(actor, message):
            message.options["due_timestamp"] = message.options.get("eta", current_millis() + delay)

    def before_process_message(self, broker, message):
        actor = broker.get_actor(message.actor_name)
        max_age = message.options.get("max_age") or actor.options.get("max_age", self.max_age)
        if not max_age:
            return

        baseline = message.message_timestamp
        if self._age_from_due(actor, message):
            baseline = message.options.get("due_timestamp") or message.options.get("eta") or baseline

        if current_millis() - baseline >= max_age:
            self.logger.warning(
                "Message %r has exceeded its age limit (submitted at %d, due at %d).",
                message.message_id, message.message_timestamp, baseline,
            )
            message.fail()
            raise SkipMessage("Message age limit exceeded")
