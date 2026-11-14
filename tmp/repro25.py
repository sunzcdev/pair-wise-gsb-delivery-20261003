from xml.etree import ElementTree as ET
el = ET.fromstring('<a/>')
print('has __iter__:', hasattr(el, '__iter__'))
import sys
print(sys.version)
from glom.core import _AbstractIterable
print('isinstance _AbstractIterable:', isinstance(el, _AbstractIterable))
print('subclasshook:', _AbstractIterable.__subclasshook__(type(el)))
