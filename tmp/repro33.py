import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S

# both iters bind the SAME scope key 'v' via S(v=T); interleave
target = [1, 2, 3]
spec = {
    'x': Iter().map((S(v=T), S.v)),
    'y': Iter().map((S(v=T * 100), S.v)),
}
res = glom(target, spec)
ix, iy = res['x'], res['y']
log = []
log.append(next(ix))   # x binds v=1
log.append(next(iy))   # y binds v=100, must not see x's v nor alter it
log.append(next(ix))   # x next element binds v=2
log.append(next(iy))   # y binds v=200
log.append(list(ix))
log.append(list(iy))
print(log)
# x stream: 1,2,3 ; y stream: 100,200,300
flat_x = [1, 2] + log[4]
flat_y = [100, 200] + log[5]
print('x:', flat_x, 'y:', flat_y)
assert flat_x == [1, 2, 3]
assert flat_y == [100, 200, 300]
print('BINDING ISOLATION OK')
