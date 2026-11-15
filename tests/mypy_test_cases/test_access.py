from marshmallow import Schema, fields
from marshmallow.access import (
    ALLOWLIST_MODE,
    INSTANCE_MODE,
    AccessPolicy,
    AccessPolicyError,
    ReadPlan,
    forbidden_access,
)
from marshmallow.experimental.access import AccessPolicy as ExperimentalPolicy


class MySchema(Schema):
    name = fields.String()


schema = MySchema()

# Mode constants are strings accepted by the constructor.
policy: AccessPolicy = AccessPolicy(INSTANCE_MODE)
with policy as entered:
    entered.dump(schema, object())

other: AccessPolicy = AccessPolicy(ALLOWLIST_MODE, allowed_fields=["name"])
allowed: frozenset[str] | None = other.allowed_fields
assert allowed == frozenset({"name"})

# Invalid modes raise the dedicated error.
try:
    AccessPolicy("not-a-mode")  # type: ignore[arg-type]
except AccessPolicyError:
    pass

# The read plan is optional until a dump runs.
plan: ReadPlan | None = other.read_plan
if plan is not None:
    evaluated: frozenset[str] = plan.evaluated_fields
    forbidden: frozenset[str] = plan.forbidden_fields
    missing: frozenset[str] = plan.missing_fields
    unevaluated: frozenset[str] = plan.unevaluated
    assert unevaluated == forbidden | missing

# The sentinel is a distinct, falsy object.
assert not forbidden_access

# The experimental re-export is the same class.
experimental: ExperimentalPolicy = AccessPolicy("instance")
assert isinstance(experimental, AccessPolicy)
