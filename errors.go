package validator

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	ut "github.com/go-playground/universal-translator"
)

const (
	fieldErrMsg = "Key: '%s' Error:Field validation for '%s' failed on the '%s' tag"
)

// ValidationErrorsTranslations is the translation return type
type ValidationErrorsTranslations map[string]string

// InvalidValidationError describes an invalid argument passed to
// `Struct`, `StructExcept`, StructPartial` or `Field`
type InvalidValidationError struct {
	Type reflect.Type
}

// Error returns InvalidValidationError message
func (e *InvalidValidationError) Error() string {
	if e.Type == nil {
		return "validator: (nil)"
	}

	return "validator: (nil " + e.Type.String() + ")"
}

// ValidationErrors is an array of FieldError's
// for use in custom error messages post validation.
type ValidationErrors []FieldError

// Error is intended for use in development + debugging and not intended to be a production error message.
// It allows ValidationErrors to subscribe to the Error interface.
// All information to create an error message specific to your application is contained within
// the FieldError found within the ValidationErrors array
func (ve ValidationErrors) Error() string {
	buff := bytes.NewBufferString("")

	for i := 0; i < len(ve); i++ {
		buff.WriteString(ve[i].Error())
		buff.WriteString("\n")
	}

	return strings.TrimSpace(buff.String())
}

// Translate translates all of the ValidationErrors
func (ve ValidationErrors) Translate(ut ut.Translator) ValidationErrorsTranslations {
	trans := make(ValidationErrorsTranslations)

	var fe *fieldError

	for i := 0; i < len(ve); i++ {
		fe = ve[i].(*fieldError)

		// // in case an Anonymous struct was used, ensure that the key
		// // would be 'Username' instead of ".Username"
		// if len(fe.ns) > 0 && fe.ns[:1] == "." {
		// 	trans[fe.ns[1:]] = fe.Translate(ut)
		// 	continue
		// }

		trans[fe.ns] = fe.Translate(ut)
	}

	return trans
}

// FieldError contains all functions to get error details
type FieldError interface {

	// Tag returns the validation tag that failed. if the
	// validation was an alias, this will return the
	// alias name and not the underlying tag that failed.
	//
	// eg. alias "iscolor": "hexcolor|rgb|rgba|hsl|hsla"
	// will return "iscolor"
	Tag() string

	// ActualTag returns the validation tag that failed, even if an
	// alias the actual tag within the alias will be returned.
	// If an 'or' validation fails the entire or will be returned.
	//
	// eg. alias "iscolor": "hexcolor|rgb|rgba|hsl|hsla"
	// will return "hexcolor|rgb|rgba|hsl|hsla"
	ActualTag() string

	// Namespace returns the namespace for the field error, with the tag
	// name taking precedence over the field's actual name.
	//
	// eg. JSON name "User.fname"
	//
	// See StructNamespace() for a version that returns actual names.
	//
	// NOTE: this field can be blank when validating a single primitive field
	// using validate.Field(...) as there is no way to extract it's name
	Namespace() string

	// StructNamespace returns the namespace for the field error, with the field's
	// actual name.
	//
	// eg. "User.FirstName" see Namespace for comparison
	//
	// NOTE: this field can be blank when validating a single primitive field
	// using validate.Field(...) as there is no way to extract its name
	StructNamespace() string

	// Field returns the field's name with the tag name taking precedence over the
	// field's actual name.
	//
	// `RegisterTagNameFunc` must be registered to get tag value.
	//
	// eg. JSON name "fname"
	// see StructField for comparison
	Field() string

	// StructField returns the field's actual name from the struct, when able to determine.
	//
	// eg.  "FirstName"
	// see Field for comparison
	StructField() string

	// Value returns the actual field's value in case needed for creating the error
	// message
	Value() interface{}

	// Param returns the param value, in string form for comparison; this will also
	// help with generating an error message
	Param() string

	// Kind returns the Field's reflect Kind
	//
	// eg. time.Time's kind is a struct
	Kind() reflect.Kind

	// Type returns the Field's reflect Type
	//
	// eg. time.Time's type is time.Time
	Type() reflect.Type

	// Translate returns the FieldError's translated error
	// from the provided 'ut.Translator' and registered 'TranslationFunc'
	//
	// NOTE: if no registered translator can be found it returns the same as
	// calling fe.Error()
	Translate(ut ut.Translator) string

	// Error returns the FieldError's message
	Error() string
}

// compile time interface checks
var _ FieldError = new(fieldError)
var _ error = new(fieldError)

// fieldError contains a single field's validation error along
// with other properties that may be needed for error message creation
// it complies with the FieldError interface
type fieldError struct {
	v              *Validate
	tag            string
	actualTag      string
	ns             string
	structNs       string
	fieldLen       uint8
	structfieldLen uint8
	value          interface{}
	param          string
	kind           reflect.Kind
	typ            reflect.Type
	rulePath       string // path of the originating node when produced by an explicit Rule, empty otherwise
	ruleLabel      string // originating leaf rule name (tag expression or custom rule name)
}

// Tag returns the validation tag that failed.
func (fe *fieldError) Tag() string {
	return fe.tag
}

// ActualTag returns the validation tag that failed, even if an
// alias the actual tag within the alias will be returned.
func (fe *fieldError) ActualTag() string {
	return fe.actualTag
}

// Namespace returns the namespace for the field error, with the tag
// name taking precedence over the field's actual name.
func (fe *fieldError) Namespace() string {
	return fe.ns
}

// StructNamespace returns the namespace for the field error, with the field's
// actual name.
func (fe *fieldError) StructNamespace() string {
	return fe.structNs
}

// Field returns the field's name with the tag name taking precedence over the
// field's actual name.
func (fe *fieldError) Field() string {
	return fe.ns[len(fe.ns)-int(fe.fieldLen):]
	// // return fe.field
	// fld := fe.ns[len(fe.ns)-int(fe.fieldLen):]

	// log.Println("FLD:", fld)

	// if len(fld) > 0 && fld[:1] == "." {
	// 	return fld[1:]
	// }

	// return fld
}

// StructField returns the field's actual name from the struct, when able to determine.
func (fe *fieldError) StructField() string {
	// return fe.structField
	return fe.structNs[len(fe.structNs)-int(fe.structfieldLen):]
}

// Value returns the actual field's value in case needed for creating the error
// message
func (fe *fieldError) Value() interface{} {
	return fe.value
}

// Param returns the param value, in string form for comparison; this will
// also help with generating an error message
func (fe *fieldError) Param() string {
	return fe.param
}

// Kind returns the Field's reflect Kind
func (fe *fieldError) Kind() reflect.Kind {
	return fe.kind
}

// Type returns the Field's reflect Type
func (fe *fieldError) Type() reflect.Type {
	return fe.typ
}

// Error returns the fieldError's error message
func (fe *fieldError) Error() string {
	return fmt.Sprintf(fieldErrMsg, fe.ns, fe.Field(), fe.tag)
}

// Translate returns the FieldError's translated error
// from the provided 'ut.Translator' and registered 'TranslationFunc'
//
// NOTE: if no registered translation can be found, it returns the original
// untranslated error message.
func (fe *fieldError) Translate(ut ut.Translator) string {
	var fn TranslationFunc

	m, ok := fe.v.transTagFunc[ut]
	if !ok {
		return fe.Error()
	}

	fn, ok = m[fe.tag]
	if !ok {
		fn, ok = m[fe.actualTag]
		if !ok {
			return fe.Error()
		}
	}

	return fn(ut, fe)
}

// RulePathError is an optional interface implemented by FieldErrors produced by
// explicit nested Rules (see And, Or, Not, Tag and Func). It allows locating
// the originating rule node of a leaf failure.
//
// The path uses zero-based bracket indexes describing the node's position in
// the rule tree, for example "[0][1]" is the second child of the first child
// of the root rule. FieldErrors produced by ordinary tag validation return an
// empty path and do not implement this interface.
type RulePathError interface {
	// RulePath returns the originating node's zero-based path, for example
	// "[1][0]". It is empty for the root rule itself.
	RulePath() string

	// RuleName returns the name of the originating leaf rule: the tag
	// expression for Tag rules or the name supplied to Func/FuncCtxRule.
	RuleName() string
}

// RulePath returns the path of the rule node that produced this error.
func (fe *fieldError) RulePath() string {
	return fe.rulePath
}

// RuleName returns the name of the leaf rule that produced this error.
func (fe *fieldError) RuleName() string {
	return fe.ruleLabel
}

var _ RulePathError = new(fieldError)

// LogicError is returned when an explicit nested Rule (And, Or, Not or a leaf)
// fails. It mirrors the rule tree structure, so failures can be explained in
// logical terms:
//   - A RuleAnd failure carries the single failing child in Children (later
//     children were never evaluated).
//   - A RuleOr failure carries one child per alternative in Children, each
//     describing why that alternative failed. The alternatives are presented as
//     choices ("satisfy one of"), not as conditions that all must be fixed.
//   - A RuleNot failure has no children: it means the negated rule succeeded.
//   - A RuleLeaf failure carries the underlying FieldErrors in Leaves.
//
// Every node exposes Path locating its position in the original rule tree.
type LogicError struct {
	// Op is the boolean operation that failed at this node.
	Op RuleKind
	// Path is this node's zero-based path in the rule tree (empty for root).
	Path string
	// Index is this node's position within its parent's children.
	Index int
	// Children holds the relevant child failure(s):
	// one for And (the first failure) and Not carries none;
	// all evaluated alternatives for Or.
	Children []*LogicError
	// Leaves holds the underlying field errors for a RuleLeaf failure.
	Leaves []FieldError

	ns       string
	structNs string
	field    string
	value    interface{}
	kind     reflect.Kind
	typ      reflect.Type
}

// compile time interface checks
var _ error = new(LogicError)

// Namespace returns the namespace for the failing field, with the tag name
// taking precedence over the field's actual name.
func (le *LogicError) Namespace() string {
	return le.ns
}

// StructNamespace returns the namespace for the failing field using the field's
// actual name.
func (le *LogicError) StructNamespace() string {
	return le.structNs
}

// Field returns the failing field's name with the tag name taking precedence
// over the field's actual name.
func (le *LogicError) Field() string {
	return le.field
}

// Value returns the actual field value that was validated.
func (le *LogicError) Value() interface{} {
	return le.value
}

// ValueKind returns the failing field's reflect Kind.
func (le *LogicError) ValueKind() reflect.Kind {
	return le.kind
}

// Type returns the failing field's reflect Type.
func (le *LogicError) Type() reflect.Type {
	return le.typ
}

// Flatten returns every underlying leaf FieldError contained within the failed
// rule tree. Each FieldError implements RulePathError, locating the rule node
// that produced it. It is useful for applications that render errors
// themselves but still want access to the originating node.
func (le *LogicError) Flatten() []FieldError {
	if le == nil {
		return nil
	}

	var errs []FieldError
	le.collect(&errs)

	return errs
}

func (le *LogicError) collect(errs *[]FieldError) {
	if len(le.Leaves) > 0 {
		*errs = append(*errs, le.Leaves...)
	}

	for _, child := range le.Children {
		child.collect(errs)
	}
}

// Error renders the logical failure tree as a human-readable explanation.
// OR nodes are phrased as alternatives; AND nodes point at the first failing
// branch; leaf nodes quote the underlying validator failure(s).
func (le *LogicError) Error() string {
	var b strings.Builder

	le.write(&b, "")

	return b.String()
}

func (le *LogicError) write(b *strings.Builder, indent string) {
	prefix := le.ns
	if prefix == "" {
		prefix = le.field
	}

	node := le.nodeLabel()

	if len(prefix) > 0 {
		fmt.Fprintf(b, "%s%s: %s", indent, prefix, node)
	} else {
		fmt.Fprintf(b, "%s%s", indent, node)
	}

	switch le.Op {
	case RuleLeaf:
		for _, fe := range le.Leaves {
			fmt.Fprintf(b, "; failed on rule %q (node %s)", leafRuleName(fe), le.pathOrRoot())
		}
	case RuleNot:
		fmt.Fprintf(b, " (node %s); the negated rule succeeded", le.pathOrRoot())
	case RuleAnd:
		if len(le.Children) > 0 {
			fmt.Fprint(b, "; first failing branch:")
		}
	case RuleOr:
		fmt.Fprintf(b, " (node %s); satisfy one of the following alternatives:", le.pathOrRoot())
	}

	b.WriteByte('\n')

	for _, child := range le.Children {
		child.write(b, indent+"  ")
	}
}

func (le *LogicError) nodeLabel() string {
	switch le.Op {
	case RuleAnd:
		return "AND rule failed"
	case RuleOr:
		return "OR rule failed"
	case RuleNot:
		return "NOT rule failed"
	default:
		return "rule failed"
	}
}

func (le *LogicError) pathOrRoot() string {
	if le.Path == "" {
		return "$"
	}
	return le.Path
}

func leafRuleName(fe FieldError) string {
	if rp, ok := fe.(RulePathError); ok && rp.RuleName() != "" {
		return rp.RuleName()
	}
	return fe.Tag()
}
