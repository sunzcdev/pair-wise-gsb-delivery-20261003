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

from ..logging import get_logger
from .middleware import Middleware


class Pipelines(Middleware):
    """Middleware that lets you pipe actors together so that the
    output of one actor feeds into the input of another.

    Parameters:
      pipe_ignore(bool): When True, ignores the result of the previous
        actor in the pipeline.
      pipe_target(dict): A message representing the actor the current
        result should be fed into.
      max_delegate_depth(int): The maximum number of times a pipeline
        may be delegated via |Delegate| values before further
        delegations are dropped.  Defaults to
        ``dramatiq.composition.MAX_DELEGATE_DEPTH``.
    """

    def __init__(self, *, max_delegate_depth=None):
        self.logger = get_logger(__name__, type(self))
        self.max_delegate_depth = max_delegate_depth

    @property
    def actor_options(self):
        return {
            "pipe_ignore",
            "pipe_target",
        }

    def after_process_message(self, broker, message, *, result=None, exception=None):
        # Since Pipelines is a default middleware, these imports have
        # to happen at runtime in order to avoid cyclic dependencies
        # from broker -> pipelines -> messages -> broker.
        from ..composition import MAX_DELEGATE_DEPTH, Delegate
        from ..message import Message

        if exception is not None or message.failed:
            return

        actor = broker.get_actor(message.actor_name)
        message_data = message.options.get("pipe_target")

        if isinstance(result, Delegate):
            max_delegate_depth = self.max_delegate_depth
            if max_delegate_depth is None:
                max_delegate_depth = MAX_DELEGATE_DEPTH

            delegate_depth = message.options.get("delegate_depth", 0)
            if delegate_depth >= max_delegate_depth:
                self.logger.error(
                    "Message %s exceeded the maximum delegate depth (%d); dropping the delegation.",
                    message,
                    max_delegate_depth,
                )
                return

            next_message = self._chain_delegate(message_data, result, delegate_depth)
            broker.enqueue(next_message, delay=next_message.options.get("delay"))
            return

        if message_data is not None:
            next_message = Message(**message_data)
            pipe_ignore = next_message.options.get("pipe_ignore") or actor.options.get("pipe_ignore")
            if not pipe_ignore:
                next_message = next_message.copy(args=next_message.args + (result,))

            broker.enqueue(next_message, delay=next_message.options.get("delay"))

    @staticmethod
    def _chain_delegate(message_data, delegate, delegate_depth):
        """Splice a delegated sub-chain in front of the original pipe
        target and return the first message to enqueue.

        The last message of the sub-chain inherits the original
        ``pipe_target`` so that, once the sub-chain has run to
        completion, the pipeline resumes with its original successor
        exactly once.  Every message in the sub-chain carries an
        incremented ``delegate_depth`` so that nested delegations
        remain bounded.
        """
        messages = [sub_message.copy() for sub_message in delegate.messages]

        for sub_message in messages:
            sub_message.options["delegate_depth"] = delegate_depth + 1

        if message_data is None:
            messages[-1].options.pop("pipe_target", None)
        else:
            messages[-1].options["pipe_target"] = message_data

        for sub_message, next_sub_message in zip(messages, messages[1:]):
            sub_message.options["pipe_target"] = next_sub_message.asdict()

        return messages[0]
