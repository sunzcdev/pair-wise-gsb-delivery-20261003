
from sqlite_utils import Migrations

m = Migrations("hello2")

@m()
def foo(db):
    db.table("foo").insert({"hello": "world"})
    