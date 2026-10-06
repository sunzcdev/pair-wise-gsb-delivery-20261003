package xpath_test

import (
	"fmt"

	"github.com/antchfx/xpath"
)

func variableDoc() xpath.NodeNavigator {
	root := &Node{Data: "root", Type: ElementNode}
	for i := 0; i < 3; i++ {
		row := &Node{Data: "row", Type: ElementNode}
		root.AddChild(row)
	}
	doc := &Node{}
	doc.AddChild(root)
	return &NodeNavigator{curr: doc, root: doc}
}

func ExampleCompile_variables() {
	doc := variableDoc()

	// Declare variables at compile time; bind concrete values per evaluation.
	expr, err := xpath.Compile("$n + 1", xpath.WithVariableDeclarations("n"))
	if err != nil {
		panic(err)
	}
	v, err := expr.EvaluateWithVariables(doc, xpath.Variables{"n": 40.0})
	if err != nil {
		panic(err)
	}
	fmt.Println(v)
	// Output: 41
}

func ExampleExpr_SelectWithVariables_nodeSet() {
	doc := variableDoc()

	rows := xpath.MustCompile("//row").Select(doc)
	binding := xpath.NewNodeSet(rows)

	expr, err := xpath.Compile("$rows[last()]", xpath.WithVariableDeclarations("rows"))
	if err != nil {
		panic(err)
	}

	// The same NodeSet is reusable: each evaluation gets its own iterator.
	for i := 0; i < 2; i++ {
		it, err := expr.SelectWithVariables(doc, xpath.Variables{"rows": binding})
		if err != nil {
			panic(err)
		}
		count := 0
		for it.MoveNext() {
			count++
		}
		fmt.Println(count)
	}
	// Output:
	// 1
	// 1
}
