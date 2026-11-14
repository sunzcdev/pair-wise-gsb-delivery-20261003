
from sqlite_utils import Migrations

m = Migrations("hello")

@m()
def foo(db):
    db.table("dogs").insert({"id": 1, "name": "Cleo"})
    
@m()
def bar(db):
    db.table("dogs").add_column("age", int)
    db.table("dogs").add_column("weight", float)
    db.table("dogs").transform()
