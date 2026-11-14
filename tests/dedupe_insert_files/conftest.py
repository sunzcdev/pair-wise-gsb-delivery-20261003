from pathlib import Path


# The sandbox used to run these tests denies access to directories created
# with the restrictive 0o700 mode that pytest defaults to on Windows.
# Relax the mode for temp directories created in this test package only.
_orig_path_mkdir = Path.mkdir


def _relaxed_path_mkdir(self, mode=0o777, parents=False, exist_ok=False):
    return _orig_path_mkdir(
        self, mode=mode | 0o055, parents=parents, exist_ok=exist_ok
    )


def pytest_configure(config):
    Path.mkdir = _relaxed_path_mkdir
    # Keep pytest's temp tree inside the writable workspace: the default
    # platform temp directory is blocked by the sandbox.
    if config.option.basetemp is None:
        config.option.basetemp = (
            Path(__file__).resolve().parent / ".pytest-basetemp"
        )


def pytest_unconfigure(config):
    Path.mkdir = _orig_path_mkdir
