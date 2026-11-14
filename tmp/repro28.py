import sys
sys.path.insert(0, '.')
from glom.core import _DEFAULT_SCOPE, TargetRegistry
from xml.etree import ElementTree as ET

el = ET.fromstring('<html><body/></html>')
reg = _DEFAULT_SCOPE[TargetRegistry]
print('initial cache:', reg._type_cache)
# what does iterating do directly?
print('iter(el):', list(iter(el)))
print('hasattr __iter__ on type:', hasattr(type(el), '__iter__'))
# check the abstract iterable check
from glom.core import _AbstractIterable
print('isinstance:', isinstance(el, _AbstractIterable))
import abc
print('ABC caches:', _AbstractIterable._abc_registry if hasattr(_AbstractIterable, "_abc_registry") else "n/a")
print('abc cache:', getattr(_AbstractIterable, "_abc_caches", None))
