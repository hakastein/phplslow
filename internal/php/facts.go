// Package php reads one PHP file into the facts the index stores: method declarations and method calls whose
// receiver class is known from the file itself.
package php

import (
	"reflect"
	"strings"

	"github.com/VKCOM/php-parser/pkg/ast"
	"github.com/VKCOM/php-parser/pkg/conf"
	phperrors "github.com/VKCOM/php-parser/pkg/errors"
	"github.com/VKCOM/php-parser/pkg/parser"
	"github.com/VKCOM/php-parser/pkg/version"
	"github.com/VKCOM/php-parser/pkg/visitor/nsresolver"
	"github.com/VKCOM/php-parser/pkg/visitor/traverser"
)

// Method is a method declared in a named class, interface, trait or enum.
type Method struct {
	Key  string // MethodKey of the declaring class and the method
	Name Range  // the method name in the declaration
}

// Call is a method call whose receiver class is known: `(new X)->m()` or `$x->m()` where every write of `$x` in its
// scope is `new X`. It becomes a Reference only when X declares m.
type Call struct {
	Key  string // MethodKey of the receiver class and the called name
	Name Range  // the method name at the call site
}

// Facts is what one file contributes to the index.
type Facts struct {
	Methods []Method
	Calls   []Call
}

// MethodKey names a method: the fully qualified class name without the leading backslash and the method name, in
// lower case, because PHP compares both case-insensitively.
func MethodKey(class, method string) string {
	return strings.ToLower(class) + "::" + strings.ToLower(method)
}

// Parse reads src. A file with syntax errors yields the facts of the tree the parser recovered; the errors are
// returned beside them.
func Parse(src []byte) (Facts, []string) {
	var syntax []string
	root, err := parser.Parse(src, conf.Config{
		Version:          &version.Version{Major: 8, Minor: 1},
		ErrorHandlerFunc: func(e *phperrors.Error) { syntax = append(syntax, e.String()) },
	})
	if err != nil {
		return Facts{}, append(syntax, err.Error())
	}
	if root == nil {
		return Facts{}, syntax
	}

	names := nsresolver.NewNamespaceResolver()
	traverser.NewTraverser(names).Traverse(root)

	w := &walker{lines: newLines(src), names: names.ResolvedNames, scope: &scope{vars: map[string]*binding{}}}
	w.walk(root)
	for _, c := range w.pending {
		if class := c.scope.classOf(c.variable, c.at); class != "" {
			w.facts.Calls = append(w.facts.Calls, Call{Key: MethodKey(class, c.method), Name: c.name})
		}
	}
	return w.facts, syntax
}

// pendingCall is a call on a variable; its receiver is known only after the whole scope is read.
type pendingCall struct {
	scope    *scope
	variable string
	at       int
	method   string
	name     Range
}

type walker struct {
	lines     lines
	names     map[ast.Vertex]string
	namespace string   // the current namespace, "" for the global one
	classes   []string // enclosing class-likes; "" for an anonymous class
	scope     *scope
	facts     Facts
	pending   []pendingCall
}

var vertexType = reflect.TypeOf((*ast.Vertex)(nil)).Elem()

// walk visits n and its subtree in source order.
func (w *walker) walk(n ast.Vertex) {
	if n == nil || reflect.ValueOf(n).IsNil() {
		return
	}
	switch n := n.(type) {
	case *ast.StmtNamespace:
		w.namespace = ""
		if name, ok := n.Name.(*ast.Name); ok {
			w.namespace = joinParts(name.Parts)
		}
	case *ast.StmtClass:
		w.inClass(n, n.Name)
		return
	case *ast.StmtInterface:
		w.inClass(n, n.Name)
		return
	case *ast.StmtTrait:
		w.inClass(n, n.Name)
		return
	case *ast.StmtEnum:
		w.inClass(n, n.Name)
		return
	case *ast.StmtClassMethod:
		if class := w.class(); class != "" {
			if id, ok := n.Name.(*ast.Identifier); ok {
				w.facts.Methods = append(w.facts.Methods, Method{
					Key:  MethodKey(class, string(id.Value)),
					Name: w.lines.span(id.Position),
				})
			}
		}
		w.inScope(n, &scope{vars: map[string]*binding{}})
		return
	case *ast.StmtFunction:
		w.inScope(n, &scope{vars: map[string]*binding{}})
		return
	case *ast.ExprClosure:
		inner := &scope{parent: w.scope, imports: map[string]bool{}, vars: map[string]*binding{}}
		for _, u := range n.Uses {
			use, ok := u.(*ast.ExprClosureUse)
			if !ok {
				continue
			}
			name := variable(use.Var)
			if use.AmpersandTkn != nil {
				w.scope.spoil(name)
				inner.spoil(name)
				continue
			}
			inner.imports[name] = true
		}
		w.inScope(n, inner)
		return
	case *ast.ExprArrowFunction:
		w.inScope(n, &scope{parent: w.scope, all: true, vars: map[string]*binding{}})
		return
	case *ast.ExprAssign:
		if name := variable(n.Var); name != "" {
			if class := w.newClass(n.Expr); class != "" {
				w.scope.assign(name, class, n.Position.EndPos)
			} else {
				w.scope.spoil(name)
			}
		} else {
			w.spoilTarget(n.Var)
		}
	case *ast.ExprAssignReference: // `$a = &$b` binds both names to one value
		w.spoilTarget(n.Var)
		w.spoilTarget(n.Expr)
	case *ast.ExprAssignBitwiseAnd:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignBitwiseOr:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignBitwiseXor:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignCoalesce:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignConcat:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignDiv:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignMinus:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignMod:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignMul:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignPlus:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignPow:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignShiftLeft:
		w.spoilTarget(n.Var)
	case *ast.ExprAssignShiftRight:
		w.spoilTarget(n.Var)
	case *ast.ExprPreInc:
		w.spoilTarget(n.Var)
	case *ast.ExprPreDec:
		w.spoilTarget(n.Var)
	case *ast.ExprPostInc:
		w.spoilTarget(n.Var)
	case *ast.ExprPostDec:
		w.spoilTarget(n.Var)
	case *ast.Argument:
		if n.AmpersandTkn != nil {
			w.spoilTarget(n.Expr)
		}
	case *ast.ExprArrayItem:
		if n.AmpersandTkn != nil {
			w.spoilTarget(n.Val)
		}
	case *ast.Parameter:
		w.spoilTarget(n.Var)
	case *ast.StmtForeach:
		w.spoilTarget(n.Key)
		w.spoilTarget(n.Var)
	case *ast.StmtCatch:
		w.spoilTarget(n.Var)
	case *ast.StmtGlobal:
		for _, v := range n.Vars {
			w.spoilTarget(v)
		}
	case *ast.StmtStaticVar:
		w.spoilTarget(n.Var)
	case *ast.StmtUnset:
		for _, v := range n.Vars {
			w.spoilTarget(v)
		}
	case *ast.ExprMethodCall:
		w.call(n.Var, n.Method)
	case *ast.ExprNullsafeMethodCall:
		w.call(n.Var, n.Method)
	}
	w.children(n)
}

// children walks every Vertex and []Vertex field of n in declaration order, which is source order.
func (w *walker) children(n ast.Vertex) {
	v := reflect.ValueOf(n).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch {
		case f.Type() == vertexType:
			if !f.IsNil() {
				w.walk(f.Interface().(ast.Vertex))
			}
		case f.Kind() == reflect.Slice && f.Type().Elem() == vertexType:
			for j := 0; j < f.Len(); j++ {
				if !f.Index(j).IsNil() {
					w.walk(f.Index(j).Interface().(ast.Vertex))
				}
			}
		}
	}
}

// class is the name of the enclosing named class-like, "" outside one or in an anonymous class.
func (w *walker) class() string {
	if len(w.classes) == 0 {
		return ""
	}
	return w.classes[len(w.classes)-1]
}

func (w *walker) inClass(n ast.Vertex, name ast.Vertex) {
	class := ""
	if id, ok := name.(*ast.Identifier); ok {
		class = string(id.Value)
		if w.namespace != "" {
			class = w.namespace + `\` + class
		}
	}
	w.classes = append(w.classes, class)
	w.children(n)
	w.classes = w.classes[:len(w.classes)-1]
}

func (w *walker) inScope(n ast.Vertex, s *scope) {
	outer := w.scope
	w.scope = s
	w.children(n)
	w.scope = outer
}

// call records a method call whose receiver is `new X` or a variable.
func (w *walker) call(receiver, method ast.Vertex) {
	id, ok := method.(*ast.Identifier)
	if !ok {
		return
	}
	name := w.lines.span(id.Position)
	if class := w.newClass(receiver); class != "" {
		w.facts.Calls = append(w.facts.Calls, Call{Key: MethodKey(class, string(id.Value)), Name: name})
		return
	}
	if v := variable(receiver); v != "" {
		w.pending = append(w.pending, pendingCall{
			scope: w.scope, variable: v, at: id.Position.StartPos, method: string(id.Value), name: name,
		})
	}
}

// newClass is the class X of `new X(...)`, also in parentheses; "" for any other expression and for
// `new self`, `new static`, `new parent`, an anonymous or a dynamic class.
func (w *walker) newClass(e ast.Vertex) string {
	for {
		b, ok := e.(*ast.ExprBrackets)
		if !ok {
			break
		}
		e = b.Expr
	}
	n, ok := e.(*ast.ExprNew)
	if !ok {
		return ""
	}
	switch n.Class.(type) {
	case *ast.Name, *ast.NameFullyQualified, *ast.NameRelative:
	default:
		return ""
	}
	class := w.names[n.Class]
	switch strings.ToLower(class) {
	case "", "self", "static", "parent":
		return ""
	}
	return class
}

// spoilTarget makes every variable written by target unknown: a variable or each variable inside a list or array
// destructuring.
func (w *walker) spoilTarget(target ast.Vertex) {
	if target == nil || reflect.ValueOf(target).IsNil() {
		return
	}
	if name := variable(target); name != "" {
		w.scope.spoil(name)
		return
	}
	switch t := target.(type) {
	case *ast.ExprList:
		w.spoilItems(t.Items)
	case *ast.ExprArray:
		w.spoilItems(t.Items)
	}
}

func (w *walker) spoilItems(items []ast.Vertex) {
	for _, item := range items {
		if it, ok := item.(*ast.ExprArrayItem); ok {
			w.spoilTarget(it.Val)
		}
	}
}

func joinParts(parts []ast.Vertex) string {
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		if part, ok := p.(*ast.NamePart); ok {
			names = append(names, string(part.Value))
		}
	}
	return strings.Join(names, `\`)
}

// variable is the name of a plain variable `$x` without the dollar; "" for `$this`, `$$x` and anything else.
func variable(e ast.Vertex) string {
	v, ok := e.(*ast.ExprVariable)
	if !ok {
		return ""
	}
	id, ok := v.Name.(*ast.Identifier)
	if !ok {
		return ""
	}
	name := strings.TrimPrefix(string(id.Value), "$")
	if name == "this" {
		return ""
	}
	return name
}

// scope is the variables of a function body, a method body, a closure, an arrow function or a file's top level.
type scope struct {
	parent  *scope
	all     bool            // an arrow function sees every variable of its parent it does not write
	imports map[string]bool // a closure sees the variables of its parent it imports by value with `use`
	vars    map[string]*binding
}

// binding is what the scope's writes say about a variable.
type binding struct {
	class string
	after int  // the variable holds `new class` only after this byte offset
	bad   bool // some write is not `new class`
}

func (s *scope) assign(name, class string, after int) {
	b, ok := s.vars[name]
	if !ok {
		s.vars[name] = &binding{class: class, after: after}
		return
	}
	if !strings.EqualFold(b.class, class) {
		b.bad = true
	}
}

func (s *scope) spoil(name string) {
	if name == "" {
		return
	}
	b, ok := s.vars[name]
	if !ok {
		b = &binding{}
		s.vars[name] = b
	}
	b.bad = true
}

// classOf is the class of variable name at byte offset at, or "" when it is not known.
func (s *scope) classOf(name string, at int) string {
	b, ok := s.vars[name]
	if !ok {
		if s.parent != nil && (s.all || s.imports[name]) {
			return s.parent.classOf(name, at)
		}
		return ""
	}
	if b.bad || at < b.after {
		return ""
	}
	return b.class
}
