import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, GlomError

# outer element 1 (the second list) contains a failing inner element
target = [[{'v': 1}], [{'v': 2}, {'oops': 3}]]
spec = Iter(Iter('v'))
gen = glom(target, spec)
try:
    for chunk in gen:
        list(chunk)
except GlomError as e:
    print(str(e))
