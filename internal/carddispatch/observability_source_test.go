package carddispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObservabilitySourceUsesOnlyReviewedMetricLabels(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "metrics.go", nil, 0)
	require.NoError(t, err)
	allowed := map[string]bool{
		"producer": true,
		"target":   true,
		"result":   true,
		"reason":   true,
	}

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || len(call.Args) < 2 ||
			(selector.Sel.Name != "NewCounterVec" && selector.Sel.Name != "NewHistogramVec" && selector.Sel.Name != "NewGaugeVec") {
			return true
		}
		labels, ok := call.Args[len(call.Args)-1].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, element := range labels.Elts {
			literal, ok := element.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			label, unquoteErr := strconv.Unquote(literal.Value)
			require.NoError(t, unquoteErr)
			assert.True(t, allowed[label], "unreviewed/high-cardinality metric label %q", label)
		}
		return true
	})
}

func TestDispatchLogsCannotIncludeSerializedCardContent(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "dispatch.go", nil, 0)
	require.NoError(t, err)
	allowedFields := map[string]bool{
		"request_id":    true,
		"producer":      true,
		"sender_kind":   true,
		"space_id":      true,
		"target_kind":   true,
		"message_id":    true,
		"message_seq":   true,
		"client_msg_no": true,
	}
	allowedConstructors := map[string]bool{
		"String": true,
		"Int64":  true,
		"Uint32": true,
		"Error":  true,
	}

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (method.Sel.Name != "Info" && method.Sel.Name != "Error") {
			return true
		}
		logger, ok := method.X.(*ast.SelectorExpr)
		if !ok || logger.Sel.Name != "logger" {
			return true
		}
		for _, argument := range call.Args[1:] {
			fieldCall, ok := argument.(*ast.CallExpr)
			require.True(t, ok, "dispatch log fields must use explicit zap constructors")
			constructor, ok := fieldCall.Fun.(*ast.SelectorExpr)
			require.True(t, ok, "dispatch log fields must use explicit zap constructors")
			assert.True(t, allowedConstructors[constructor.Sel.Name], "unsafe zap field constructor %s", constructor.Sel.Name)
			if constructor.Sel.Name == "Error" {
				continue
			}
			require.NotEmpty(t, fieldCall.Args)
			keyLiteral, ok := fieldCall.Args[0].(*ast.BasicLit)
			require.True(t, ok)
			key, unquoteErr := strconv.Unquote(keyLiteral.Value)
			require.NoError(t, unquoteErr)
			assert.True(t, allowedFields[key], "unreviewed dispatch log field %q", key)
		}
		return true
	})
}
