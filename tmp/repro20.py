import sys
sys.path.insert(0, '.')
import glom.streaming as st
from glom.core import GlomError

# monkeypatch a fresh _iterate with explicit logging
import glom.streaming as M
src = open(M.__file__).read()
print("'except Exception as e' count:", src.count('except Exception as e'))
