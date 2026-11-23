from __future__ import annotations

import math
from typing import TypeVar
from warnings import warn

from ..streams.memory import (
    MemoryObjectReceiveStream,
    MemoryObjectSendStream,
    MemoryObjectStreamBuffer,
    _MemoryObjectStreamState,
)

T_Item = TypeVar("T_Item")


class create_memory_object_stream(
    tuple[MemoryObjectSendStream[T_Item], MemoryObjectReceiveStream[T_Item]],
):
    """
    Create a memory object stream.

    The stream's item type can be annotated like
    :func:`create_memory_object_stream[T_Item]`.

    :param max_buffer_size: number of items held in the buffer until ``send()`` starts
        blocking, or a :class:`~anyio.streams.memory.MemoryObjectStreamBuffer`
        instance to make the created stream share a common buffer quota with other
        streams created with the same instance
    :param item_type: old way of marking the streams with the right generic type for
        static typing (does nothing on AnyIO 4)

        .. deprecated:: 4.0
          Use ``create_memory_object_stream[YourItemType](...)`` instead.
    :return: a tuple of (send stream, receive stream)

    """

    def __new__(  # type: ignore[misc]
        cls,
        max_buffer_size: float | MemoryObjectStreamBuffer = 0,
        item_type: object = None,
    ) -> tuple[MemoryObjectSendStream[T_Item], MemoryObjectReceiveStream[T_Item]]:
        if isinstance(max_buffer_size, MemoryObjectStreamBuffer):
            buffer = max_buffer_size
        else:
            if max_buffer_size != math.inf and not isinstance(max_buffer_size, int):
                raise ValueError(
                    "max_buffer_size must be either an integer or math.inf"
                )
            if max_buffer_size < 0:
                raise ValueError("max_buffer_size cannot be negative")

            buffer = MemoryObjectStreamBuffer(max_buffer_size)

        if item_type is not None:
            warn(
                "The item_type argument has been deprecated in AnyIO 4.0. "
                "Use create_memory_object_stream[YourItemType](...) instead.",
                DeprecationWarning,
                stacklevel=2,
            )

        state = _MemoryObjectStreamState[T_Item](buffer)
        return (MemoryObjectSendStream(state), MemoryObjectReceiveStream(state))
