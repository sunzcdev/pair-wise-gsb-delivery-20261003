package main

import (
	"fmt"

	"github.com/go-playground/validator/v10"
)

func main() {
	validate := validator.New()

	// A contact is valid when it is either:
	//   - a non-empty, valid email, OR
	//   - a phone number that does NOT start with "000"
	rule := validator.Or(
		validator.And(
			validator.Tag("required"),
			validator.Tag("email"),
		),
		validator.And(
			validator.Tag("e164"),
			validator.Not(validator.Tag("startswith=000")),
		),
	)

	for _, contact := range []string{"", "not-an-email", "+8613800138000"} {
		err := validate.VarRule(contact, rule)
		if err == nil {
			fmt.Printf("%q is valid\n", contact)
			continue
		}

		fmt.Printf("%q is invalid:\n%s\n", contact, err)

		le := err.(*validator.LogicError)
		for _, fe := range le.Flatten() {
			rp := fe.(validator.RulePathError)
			fmt.Printf("  - node %s failed rule %q\n", rp.RulePath(), rp.RuleName())
		}
	}
}
