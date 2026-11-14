
from sqlite_utils import Migrations

creatures = Migrations("creatures")

@creatures()
def create_table(db):
    db.table("creatures").insert({"name": "Cleo"})

@creatures()
def add_weight(db):
    db.table("creature_weights").insert({"weight": 4.2})

sales = Migrations("sales")

@sales()
def create_table(db):
    db.table("sales").insert({"id": 1})

@sales()
def add_weight(db):
    db.table("sales_weights").insert({"weight": 10})
