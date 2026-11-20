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

import dataclasses
import enum
from typing import Any, Optional

from .errors import ResultFailure


class ResultStatus(str, enum.Enum):
    """The state of an individual result yielded by
    :meth:`dramatiq.group.iter_results`.
    """

    #: The result was available and has been retrieved successfully.
    #: A stored ``None`` result is reported with this status rather
    #: than being confused with a result that is still missing.
    COMPLETE = "complete"

    #: The actor failed and its result is a stored exception.  The
    #: original exception is available on :attr:`GroupResult.error`.
    FAILED = "failed"

    #: No result was available before the wait deadline expired.
    MISSING = "missing"


@dataclasses.dataclass(frozen=True)
class GroupResult:
    """A single result retrieved from a :class:`~dramatiq.group` by
    :meth:`~dramatiq.group.iter_results`.

    Results are yielded in the order they become available rather than
    in submission order.  The :attr:`index_path` records where the
    result came from within the (possibly nested) group so the caller
    can correlate it with the original messages.

    Parameters:
      index_path(tuple[int, ...]): The position of the result within
        the group.  Each element is an index into the corresponding
        level of nested groups, so a result from the second child of
        the first nested group has the path ``(0, 1)``.
      status(ResultStatus): Whether the result is complete, the
        product of a failed actor or still missing.
      value(Any): The result value when ``status`` is
        :attr:`ResultStatus.COMPLETE`.  This may legitimately be
        ``None``.  It is ``None`` for every other status.
      error(Optional[ResultFailure]): The failure retrieved from the
        result backend when ``status`` is :attr:`ResultStatus.FAILED`.
    """

    index_path: tuple[int, ...]
    status: ResultStatus
    value: Any = None
    error: Optional[ResultFailure] = None

    @property
    def complete(self) -> bool:
        """True when the result was retrieved successfully."""
        return self.status is ResultStatus.COMPLETE

    @property
    def failed(self) -> bool:
        """True when the actor failed and a stored exception was
        retrieved instead of a result.
        """
        return self.status is ResultStatus.FAILED

    @property
    def missing(self) -> bool:
        """True when no result was available before the deadline."""
        return self.status is ResultStatus.MISSING
