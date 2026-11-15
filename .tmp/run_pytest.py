import sys
from pathlib import Path

_orig_mkdir = Path.mkdir


def mkdir(self, mode=0o777, parents=False, exist_ok=False):
    return _orig_mkdir(self, mode=0o777, parents=parents, exist_ok=exist_ok)


Path.mkdir = mkdir

from pytest import main

sys.exit(main(sys.argv[1:]))
