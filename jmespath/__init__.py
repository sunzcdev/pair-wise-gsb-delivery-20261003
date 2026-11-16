from jmespath import parser
from jmespath import exceptions
from jmespath.visitor import Options
from jmespath.stream import stream_search

__version__ = '1.1.0'


def compile(expression):
    return parser.Parser().parse(expression)


def search(expression, data, options=None):
    return parser.Parser().parse(expression).search(data, options=options)
