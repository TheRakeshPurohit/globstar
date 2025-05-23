package python

import (
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"globstar.dev/analysis"
)

var SSRFInjection *analysis.Analyzer = &analysis.Analyzer{
	Name:        "ssrf-injection",
	Language:    analysis.LangPy,
	Description: "User-supplied data is used in a server-side request, potentially leading to SSRF. Mitigate by validating schemes and hosts against an allowlist, avoiding direct response forwarding, and enforcing authentication and transport-layer security in the proxied request.",
	Category:    analysis.CategorySecurity,
	Severity:    analysis.SeverityWarning,
	Run:         checkSSRFInjection,
}

func checkSSRFInjection(pass *analysis.Pass) (interface{}, error) {
	requestMethodName := make(map[string]bool)
	userDataVarMap := make(map[string]bool)

	// get the request methods imported
	analysis.Preorder(pass, func(node *sitter.Node) {
		if node.Type() != "import_from_statement" {
			return
		}

		modNameNode := node.ChildByFieldName("module_name")
		if modNameNode.Content(pass.FileContext.Source) != "requests" {
			return
		}

		nameNodes := getNamedChildren(node, 1)
		for _, name := range nameNodes {
			if name.Type() == "dotted_name" {
				methodName := name.NamedChild(0)
				if methodName.Type() == "identifier" {
					requestMethodName[methodName.Content(pass.FileContext.Source)] = true
				}
			}
		}
	})

	// get the variable name from the Flask decorated route function
	analysis.Preorder(pass, func(node *sitter.Node) {
		if node.Type() != "decorated_definition" {
			return
		}
		decNode := node.NamedChild(0)
		if decNode.Type() != "decorator" {
			return
		}
		callNode := decNode.NamedChild(0)
		if callNode.Type() != "call" {
			return
		}
		funcNode := callNode.ChildByFieldName("function")
		if funcNode.Type() != "attribute" {
			return
		}
		if !strings.HasSuffix(funcNode.Content(pass.FileContext.Source), ".route") {
			return
		}
		defNode := node.ChildByFieldName("definition")
		paramsNode := defNode.ChildByFieldName("parameters")
		if paramsNode.Type() != "parameters" {
			return
		}
		allparamNodes := getNamedChildren(paramsNode, 0)
		for _, p := range allparamNodes {
			userDataVarMap[p.Content(pass.FileContext.Source)] = true
		}
	})

	// get the var names for request calls
	analysis.Preorder(pass, func(node *sitter.Node) {
		if node.Type() != "assignment" {
			return
		}

		leftNode := node.ChildByFieldName("left")
		rightNode := node.ChildByFieldName("right")

		if rightNode == nil {
			return
		}

		if isRequestCall(rightNode, pass.FileContext.Source) {
			userDataVarMap[leftNode.Content(pass.FileContext.Source)] = true
		}
	})

	// get the var names for intermediate variables with request data string formatting
	analysis.Preorder(pass, func(node *sitter.Node) {
		if node.Type() != "assignment" {
			return
		}

		leftNode := node.ChildByFieldName("left")
		rightNode := node.ChildByFieldName("right")

		if rightNode == nil {
			return
		}

		if isUserTainted(rightNode, pass.FileContext.Source, userDataVarMap) {
			userDataVarMap[leftNode.Content(pass.FileContext.Source)] = true
		}
	})

	// find insecure method calls
	analysis.Preorder(pass, func(node *sitter.Node) {
		if node.Type() != "call" {
			return
		}

		if !isRequestCall(node, pass.FileContext.Source) && !isImportedRequestMethod(node, pass.FileContext.Source, requestMethodName) {
			return
		}

		argListNode := node.ChildByFieldName("arguments")
		if argListNode.Type() != "argument_list" {
			return
		}
		// fmt.Println(userDataVarMap)
		argsNode := getNamedChildren(argListNode, 0)
		for _, arg := range argsNode {
			if isUserTainted(arg, pass.FileContext.Source, userDataVarMap) {
				pass.Report(pass, node, "Unvalidated user input detected in Server-Side Request - potential SSRF vulnerability")
			}
		}
	})

	return nil, nil
}

func isImportedRequestMethod(node *sitter.Node, source []byte, reqMethodMap map[string]bool) bool {
	if node.Type() != "call" {
		return false
	}

	funcNode := node.ChildByFieldName("function")
	if funcNode.Type() != "identifier" {
		return false
	}

	funcName := funcNode.Content(source)
	isReqMethod := false

	for reqmethname := range reqMethodMap {
		if funcName == reqmethname {
			isReqMethod = true
		}
	}

	return isReqMethod
}

func isUserTainted(node *sitter.Node, source []byte, userDataVarMap map[string]bool) bool {
	switch node.Type() {
	case "call":
		functionNode := node.ChildByFieldName("function")
		if functionNode.Type() != "attribute" {
			return false
		}

		if !strings.HasSuffix(functionNode.Content(source), ".format") {
			return false
		}

		argListNode := node.ChildByFieldName("arguments")
		if argListNode.Type() != "argument_list" {
			return false
		}

		argsNode := getNamedChildren(argListNode, 0)
		for _, arg := range argsNode {
			if arg.Type() == "identifier" && userDataVarMap[arg.Content(source)] {
				return true
			} else if arg.Type() == "call" && isRequestCall(arg, source) {
				return true
			}
		}

	case "string":
		if node.Content(source)[0] != 'f' {
			return false
		}
		stringChildrenNodes := getNamedChildren(node, 0)
		for _, strnode := range stringChildrenNodes {
			if strnode.Type() == "interpolation" {
				exprnode := strnode.ChildByFieldName("expression")
				if exprnode.Type() == "identifier" && userDataVarMap[exprnode.Content(source)] {
					return true
				} else if exprnode.Type() == "call" && isRequestCall(exprnode, source) {
					return true
				}
			}
		}

	case "binary_operator":
		binOpStr := node.Content(source)

		for reqvar := range userDataVarMap {
			pattern := `\b` + reqvar + `\b`
			re := regexp.MustCompile(pattern)

			if re.MatchString(binOpStr) {
				return true
			}
		}

		rightNode := node.ChildByFieldName("right")
		if rightNode.Type() == "call" && isRequestCall(rightNode, source) {
			return true
		} else if rightNode.Type() == "tuple" {
			targsNode := getNamedChildren(rightNode, 0)
			for _, targ := range targsNode {
				if targ.Type() == "identifier" && userDataVarMap[targ.Content(source)] {
					return true
				} else if targ.Type() == "call" && isRequestCall(targ, source) {
					return true
				}
			}
		}

	case "identifier":
		return userDataVarMap[node.Content(source)]

	case "subscript":
		return isRequestCall(node, source)
	}

	return false
}

func isRequestCall(node *sitter.Node, source []byte) bool {
	switch node.Type() {
	case "call":
		funcNode := node.ChildByFieldName("function")
		if funcNode.Type() != "attribute" {
			return false
		}
		if !strings.HasPrefix(funcNode.Content(source), "request") && !strings.HasPrefix(funcNode.Content(source), "flask.request.") {
			return false
		}

		return true

	case "subscript":
		valueNode := node.ChildByFieldName("value")
		if valueNode.Type() != "attribute" {
			return false
		}

		if !strings.HasPrefix(valueNode.Content(source), "request.") && !strings.HasPrefix(valueNode.Content(source), "flask.request.") {
			return false
		}

		return true
	}

	return false
}
