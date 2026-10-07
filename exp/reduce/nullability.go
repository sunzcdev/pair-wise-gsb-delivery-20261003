package reduce

import "github.com/doug-martin/goqu/v9/exp"

func (r *Reducer) nullabilityOf(ident exp.IdentifierExpression) Nullability {
	if r.nullability == nil {
		return UnknownNullability
	}
	if n, ok := r.nullability[identifierKey(ident)]; ok {
		return n
	}
	return UnknownNullability
}

// identifierKey renders the same dotted spelling the caller uses in the
// WithNullability map.
func identifierKey(ident exp.IdentifierExpression) string {
	col, ok := ident.GetCol().(string)
	if !ok {
		return ""
	}
	switch {
	case ident.GetSchema() != "" && ident.GetTable() != "":
		return ident.GetSchema() + "." + ident.GetTable() + "." + col
	case ident.GetTable() != "":
		return ident.GetTable() + "." + col
	default:
		return col
	}
}
