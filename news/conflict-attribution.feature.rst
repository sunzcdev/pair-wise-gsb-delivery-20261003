Explain dependency conflicts by tracing each conflicting requirement
through its parent dependency chain back to the root request, and list
releases rejected solely because of a ``Requires-Python`` mismatch in a
dedicated section. Dependencies from extras that were not requested, and
markers that do not match the environment, no longer participate in the
conflict attribution.
