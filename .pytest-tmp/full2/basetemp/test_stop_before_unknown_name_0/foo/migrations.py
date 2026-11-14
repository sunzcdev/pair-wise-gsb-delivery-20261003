
from sqlite_utils import Migrations

m = Migrations("hello")

@m()
def foo(db):
    db.table("foo").insert({"hello": "world"})

@m()
def bar(db):
    db.table("bar").insert({"hello": "world"})
