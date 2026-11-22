Add an optional ``--probe-env`` policy that controls which parent environment variables the interpreter probe subprocess
inherits, so a launcher that leaves a stale ``PYTHONHOME`` or ``PYTHONPATH`` behind no longer hides a healthy
interpreter. The executable named by ``--python`` and the options the creator receives are unchanged. A probe that fails
is retried under every narrower policy, and the resulting error distinguishes a disturbed probe (``external_env`` is
``True``) from an interpreter that cannot report its information.
