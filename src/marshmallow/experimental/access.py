"""Experimental attribute access policies.

Re-exports the public API of :mod:`marshmallow.access`. The feature lives in
the top-level package because the serialization core needs to consult the
active policy; importing it from this module marks the usage as experimental.

.. seealso:: :mod:`marshmallow.access`
"""

from __future__ import annotations

from marshmallow.access import (
    ACCESS_MODES,
    AccessMode,
    AccessPolicy,
    AccessPolicyError,
    ReadPlan,
    forbidden_access,
    get_active_policy,
)

__all__ = [
    "ACCESS_MODES",
    "AccessMode",
    "AccessPolicy",
    "AccessPolicyError",
    "ReadPlan",
    "forbidden_access",
    "get_active_policy",
]
