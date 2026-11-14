import sys, traceback
sys.path.insert(0, '.')
from glom.core import _DEFAULT_SCOPE, TargetRegistry
from glom import glom, Fill, Ref, T, Iter
from xml.etree import ElementTree as ET
el = ET.fromstring('<html><body/></html>')

reg = _DEFAULT_SCOPE[TargetRegistry]
orig = reg.get_handler
def spy(op, obj, path=None, raise_exc=True):
    if op == 'iterate' and type(obj).__name__ == 'Element':
        print('get_handler iterate Element, raise_exc=', raise_exc)
        traceback.print_stack()
    return orig(op, obj, path, raise_exc)
reg.get_handler = spy

spec = Fill(Ref('ET', (T.tag, (Iter(Ref('ET')), list))))
try:
    print('RESULT:', glom(el, spec))
except Exception as e:
    print('ERR', type(e).__name__)
