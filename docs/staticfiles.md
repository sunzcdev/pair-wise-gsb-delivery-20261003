
Starlette also includes a `StaticFiles` class for serving files in a given directory:

### StaticFiles

Signature: `StaticFiles(directory=None, packages=None, html=False, check_dir=True, follow_symlink=False, precompressed=None)`

* `directory` - A string or [os.PathLike][pathlike] denoting a directory path.
* `packages` - A list of strings or list of tuples of strings of python packages.
* `html` - Run in HTML mode. Automatically loads `index.html` for directories if such file exist.
* `check_dir` - Ensure that the directory exists upon instantiation. Defaults to `True`.
* `follow_symlink` - A boolean indicating if symbolic links for files and directories should be followed. Defaults to `False`.
* `precompressed` - A mapping of content codings to the suffix of the precompressed files that sit next to the originals, for example `{"gzip": ".gz", "br": ".br"}`. Defaults to `None`, which disables precompressed serving.

You can combine this ASGI application with Starlette's routing to provide
comprehensive static file serving.

```python
from starlette.applications import Starlette
from starlette.routing import Mount
from starlette.staticfiles import StaticFiles


routes = [
    ...
    Mount('/static', app=StaticFiles(directory='static'), name="static"),
]

app = Starlette(routes=routes)
```

Static files will respond with "404 Not found" or "405 Method not allowed"
responses for requests which do not match. In HTML mode if `404.html` file
exists it will be shown as 404 response.

The `packages` option can be used to include "static" directories contained within
a python package. The Python "bootstrap4" package is an example of this.

```python
from starlette.applications import Starlette
from starlette.routing import Mount
from starlette.staticfiles import StaticFiles


routes=[
    ...
    Mount('/static', app=StaticFiles(directory='static', packages=['bootstrap4']), name="static"),
]

app = Starlette(routes=routes)
```

By default `StaticFiles` will look for a `statics` directory in each package,
you can change the default directory by specifying a tuple of strings.

```python
routes=[
    ...
    Mount('/static', app=StaticFiles(packages=[('bootstrap4', 'static')]), name="static"),
]
```

You may prefer to include static files directly inside the "static" directory
rather than using Python packaging to include static files, but it can be useful
for bundling up reusable components.

### Precompressed files

Static site builders can write a compressed version of each asset next to the
original file, such as `app.js` together with `app.js.gz` and `app.js.br`.
Passing the `precompressed` option tells `StaticFiles` which suffixes map to
which content coding. The representation is chosen from the request's
`Accept-Encoding` header, honoring the quality weights, and missing compressed
files transparently fall back to the next accepted coding or to the original
file. When the option is omitted or `None`, requests are handled exactly as
before.

When a compressed representation is sent, its `Content-Type` still describes
the original resource, while `Content-Length`, `ETag`, `Last-Modified` and
byte ranges all describe the bytes that are actually transferred. A
`Vary: Accept-Encoding` header is added so caches keep the representations
apart, and a compressed `ETag` never validates the unencoded representation.

```python
from starlette.staticfiles import StaticFiles


app = StaticFiles(directory="static", precompressed={"gzip": ".gz", "br": ".br"})
```

With the configuration above, a request for `/app.js` carrying
`Accept-Encoding: gzip, br;q=0.9` is answered with the contents of
`app.js.gz` and a `Content-Encoding: gzip` header when that file exists.

[pathlike]: https://docs.python.org/3/library/os.html#os.PathLike
