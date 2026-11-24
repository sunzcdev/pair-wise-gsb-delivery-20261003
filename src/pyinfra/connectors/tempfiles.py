"""Compatibility import for the per-host temporary file ownership registry."""

from pyinfra.api.tempfiles import TempFileLease, TempFileRegistry, TempFileRemover

__all__ = ["TempFileLease", "TempFileRegistry", "TempFileRemover"]
