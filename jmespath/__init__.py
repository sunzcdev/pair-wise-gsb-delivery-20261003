from jmespath import parser
from jmespath.visitor import Options
from jmespath.capabilities import CapabilityPolicy
from jmespath.exceptions import FunctionNotAllowedError

__version__ = '1.1.0'


def compile(expression):
    return parser.Parser().parse(expression)


def search(expression, data, options=None):
    return parser.Parser().parse(expression).search(data, options=options)


__all__ = [
    'compile',
    'search',
    'Options',
    'CapabilityPolicy',
    'FunctionNotAllowedError',
]
