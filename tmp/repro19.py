import sys, traceback
sys.path.insert(0, '.')
import glom.streaming as st
orig = st.Iter._iterate
def patched(self, target, scope):
    try:
        yield from orig(self, target, scope)
    except Exception as e:
        print('CAUGHT in layer', repr(self), '->', type(e).__name__)
        raise
st.Iter._iterate = patched
from glom import glom, Iter, GlomError

class Fail:
    def glomit(self, target, scope):
        raise KeyError('boom')

target = [[{'v': 1}], [{'v': 2}, {'oops': 3}]]
gen = glom(target, Iter(Iter(Fail())))
try:
    for chunk in gen:
        list(chunk)
except Exception as e:
    print('FINAL', type(e).__name__)
