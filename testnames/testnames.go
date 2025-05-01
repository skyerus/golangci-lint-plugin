package linters

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"

	"github.com/golangci/plugin-module-register/register" // Import register package
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ast/inspector"
)

// Register the plugin with its name and the New function.
func init() {
	register.Plugin("testnames", New)
}

// TestNamesPlugin struct can hold settings if needed in the future.
type TestNamesPlugin struct{}

// New creates a new instance of the TestNamesPlugin.
// It currently doesn't process any settings.
func New(settings any) (register.LinterPlugin, error) {
	// If settings were needed, decode them here using register.DecodeSettings
	// _, err := register.DecodeSettings[YourSettingsStruct](settings)
	// if err != nil {
	// 	 return nil, err
	// }
	return &TestNamesPlugin{}, nil
}

// BuildAnalyzers returns the list of analyzers provided by this plugin.
func (p *TestNamesPlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{Analyzer}, nil
}

// GetLoadMode specifies how golangci-lint should load packages for this analyzer.
func (p *TestNamesPlugin) GetLoadMode() string {
	// We need type information to resolve variables and their definitions.
	return register.LoadModeTypesInfo
}

// --- Analyzer Definition (remains the same) ---
const Doc = `check for spaces in sub-test names

The testnames linter checks that the names passed to t.Run() do not contain spaces.
It suggests replacing spaces with underscores for consistency and potentially easier filtering.
It also inspects map keys or struct fields used to define test names in range loops.`

var Analyzer = &analysis.Analyzer{
	Name: "testnames",
	Doc:  Doc,
	Run:  run,
	Requires: []*analysis.Analyzer{
		inspect.Analyzer,
	},
}

func run(pass *analysis.Pass) (interface{}, error) {
	inspector := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	nodeFilter := []ast.Node{
		(*ast.CallExpr)(nil),
	}

	// Keep track of reported literals to avoid duplicate reports for the same literal
	reportedLiterals := make(map[token.Pos]bool)

	inspector.Preorder(nodeFilter, func(node ast.Node) {
		callExpr := node.(*ast.CallExpr)

		// --- Check 1: Direct t.Run call ---
		if !isTRunCall(callExpr) {
			return
		}

		nameArg := callExpr.Args[0]

		// --- Check 1a: String Literal in t.Run ---
		if basicLit, ok := nameArg.(*ast.BasicLit); ok && basicLit.Kind == token.STRING {
			if reportedLiterals[basicLit.Pos()] {
				return // Already reported this literal
			}
			testName := strings.Trim(basicLit.Value, `"`)
			if strings.Contains(testName, " ") {
				reportSpaceInLiteral(pass, basicLit, testName)
				reportedLiterals[basicLit.Pos()] = true
			}
			return
		}

		// --- Check 2: Identifier from a range loop ---
		if nameIdent, ok := nameArg.(*ast.Ident); ok {
			// Find the enclosing range statement that defines this identifier as the key
			rangeStmt := findEnclosingRangeStmtForKey(pass, nameIdent)
			if rangeStmt == nil {
				return // Not a range key identifier we can analyze
			}

			// Find the definition of the collection being ranged over
			collectionIdent, ok := rangeStmt.X.(*ast.Ident)
			if !ok {
				return // Range expression is not a simple identifier
			}

			obj := pass.TypesInfo.ObjectOf(collectionIdent)
			if obj == nil {
				return // Cannot find object definition
			}

			// Find the declaration node (AssignStmt or ValueSpec) using the object's position
			declNode := findDeclNodeAt(pass, obj.Pos())
			if declNode == nil {
				return // Could not find declaration node
			}

			// Find the assignment/declaration where the collection was initialized
			switch decl := declNode.(type) {
			case *ast.AssignStmt:
				// Find the literal value in the assignment
				// Assuming short variable declaration `tests := ...` or assignment `tests = ...`
				if len(decl.Lhs) != len(decl.Rhs) {
					return // Mismatched assignment counts
				}
				for i, lhsExpr := range decl.Lhs {
					if lhsIdent, ok := lhsExpr.(*ast.Ident); ok && lhsIdent.Name == collectionIdent.Name {
						checkCompositeLiteral(pass, decl.Rhs[i], reportedLiterals)
						break
					}
				}
			case *ast.ValueSpec:
				// Find the literal value in the var declaration
				if len(decl.Values) == 0 {
					return
				}
				for i, name := range decl.Names {
					if name.Name == collectionIdent.Name && i < len(decl.Values) {
						checkCompositeLiteral(pass, decl.Values[i], reportedLiterals)
						break
					}
				}
			default:
				// Unsupported declaration type
				return
			}

			// --- Removed old assignment/declaration finding logic ---
		}
	})

	return nil, nil
}

// isTRunCall checks if a CallExpr is a call to t.Run (or similar)
func isTRunCall(callExpr *ast.CallExpr) bool {
	selExpr, ok := callExpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if selExpr.Sel.Name != "Run" {
		return false
	}
	ident, ok := selExpr.X.(*ast.Ident)
	// Basic check for common testing variable names (t, tb, etc.)
	if !ok || !strings.HasPrefix(ident.Name, "t") {
		return false
	}
	// Check if there are at least two arguments (name and function)
	return len(callExpr.Args) >= 2
}

// findEnclosingRangeStmtForKey finds the *ast.RangeStmt where ident is defined as the Key
func findEnclosingRangeStmtForKey(pass *analysis.Pass, ident *ast.Ident) *ast.RangeStmt {
	var containingFile *ast.File
	// Find the file containing the identifier
	for _, file := range pass.Files {
		if file.Pos() <= ident.Pos() && ident.Pos() < file.End() {
			containingFile = file
			break
		}
	}
	if containingFile == nil {
		return nil // Identifier not found in any file of this pass
	}

	// Search the AST path within the correct file
	path, _ := astutil.PathEnclosingInterval(containingFile, ident.Pos(), ident.End())
	if path == nil {
		return nil // Should not happen for a valid identifier
	}

	for _, node := range path {
		if rangeStmt, ok := node.(*ast.RangeStmt); ok {
			// Check if the Key identifier's object matches the input identifier's object
			if keyIdent, ok := rangeStmt.Key.(*ast.Ident); ok {
				keyObj := pass.TypesInfo.ObjectOf(keyIdent)
				identObj := pass.TypesInfo.ObjectOf(ident)
				// Compare definition positions instead of object identity as a fallback
				if keyObj != nil && identObj != nil && keyObj.Pos() == identObj.Pos() {
					return rangeStmt
				}
			}
		}
	}
	return nil
}

// findDeclNodeAt finds the declaration AST node (AssignStmt or ValueSpec) at a given position
func findDeclNodeAt(pass *analysis.Pass, pos token.Pos) ast.Node {
	for _, file := range pass.Files {
		path, _ := astutil.PathEnclosingInterval(file, pos, pos)
		if path == nil {
			continue
		}
		// Iterate path backwards (from specific to general) to find the declaration statement
		for i := len(path) - 1; i >= 0; i-- {
			switch node := path[i].(type) {
			case *ast.AssignStmt:
				// Check if this AssignStmt defines the object at pos
				for _, lhs := range node.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok && pass.TypesInfo.ObjectOf(ident).Pos() == pos {
						return node
					}
				}
			case *ast.ValueSpec:
				// Check if this ValueSpec defines the object at pos
				for _, name := range node.Names {
					if pass.TypesInfo.ObjectOf(name).Pos() == pos {
						return node
					}
				}
			}
		}
	}
	return nil
}

// checkCompositeLiteral inspects a map or slice literal for test names with spaces
func checkCompositeLiteral(pass *analysis.Pass, expr ast.Expr, reported map[token.Pos]bool) {
	compLit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return // Not a composite literal
	}

	switch compLit.Type.(type) {
	case *ast.MapType:
		// Check map literal keys
		for _, elt := range compLit.Elts {
			if kvExpr, ok := elt.(*ast.KeyValueExpr); ok {
				if keyLit, ok := kvExpr.Key.(*ast.BasicLit); ok && keyLit.Kind == token.STRING {
					if reported[keyLit.Pos()] {
						continue
					}
					testName := strings.Trim(keyLit.Value, `"`)
					if strings.Contains(testName, " ") {
						reportSpaceInLiteral(pass, keyLit, testName)
						reported[keyLit.Pos()] = true
					}
				}
			}
		}
	case *ast.ArrayType: // Corrected from ast.SliceType
		// Check slice/array literal elements (assuming slice of structs)
		for _, elt := range compLit.Elts {
			if structLit, ok := elt.(*ast.CompositeLit); ok {
				for _, field := range structLit.Elts {
					if kvExpr, ok := field.(*ast.KeyValueExpr); ok {
						if keyIdent, ok := kvExpr.Key.(*ast.Ident); ok {
							// Check common field names for test descriptions
							if keyIdent.Name == "name" || keyIdent.Name == "desc" || keyIdent.Name == "testName" {
								if valLit, ok := kvExpr.Value.(*ast.BasicLit); ok && valLit.Kind == token.STRING {
									if reported[valLit.Pos()] {
										continue
									}
									testName := strings.Trim(valLit.Value, `"`)
									if strings.Contains(testName, " ") {
										reportSpaceInLiteral(pass, valLit, testName)
										reported[valLit.Pos()] = true
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// reportSpaceInLiteral reports a diagnostic for a string literal containing spaces
func reportSpaceInLiteral(pass *analysis.Pass, lit *ast.BasicLit, testName string) {
	newName := strings.ReplaceAll(testName, " ", "_")
	pass.Report(analysis.Diagnostic{
		Pos:     lit.Pos(),
		End:     lit.End(),
		Message: fmt.Sprintf("test name literal %q contains spaces; consider replacing with %q", testName, newName),
		SuggestedFixes: []analysis.SuggestedFix{
			{
				Message: fmt.Sprintf("Replace spaces with underscores: %q", newName),
				TextEdits: []analysis.TextEdit{
					{
						Pos:     lit.Pos(),
						End:     lit.End(),
						NewText: []byte(fmt.Sprintf("%q", newName)),
					},
				},
			},
		},
	})
}
