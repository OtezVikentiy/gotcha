package guards

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Типы, где рождается ПОЛЕ вида (не тело вебхука); trace.PerfIssue доверена —
// источник trace.Finding проверен. Встраивание потребует идти по промотированным полям.
var kindCarrierTypes = map[string]bool{
	"escalation.DispatchInput": true,
	"alert.Event":              true,
	"uptime.Event":             true,
	"trace.RegressionEvent":    true,
	"trace.PerfIssue":          true,
	"trace.Finding":            true,
}

const maxKindResolveDepth = 8

type kindFuncInfo struct {
	pkg      string
	filePath string
	decl     *ast.FuncDecl
	// имя параметра/ресивера/локальной переменной -> "pkg.Type" (без
	// указателя) — источники равноправны для проверки поля вида.
	varType     map[string]string
	paramOrder  []string
	localAssign map[string][]ast.Expr
}

type kindFileInfo struct {
	pkg  string
	file *ast.File
}

// typeKeyOf — тип параметра/ресивера/var, указатель или нет: несущей
// структуре всё равно, обращаются к её полю через значение или через *T.
func typeKeyOf(t ast.Expr, pkg string) string {
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch v := t.(type) {
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
	case *ast.Ident:
		return pkg + "." + v.Name
	}
	return ""
}

// funcReturnTypeKeyInPackage ищет функцию БЕЗ ресивера по всему пакету (не
// файлу) и берёт тип её первого результата — конвенция «значение, error».
func funcReturnTypeKeyInPackage(ctx *kindResolveCtx, pkg, name string) string {
	for _, f := range ctx.pkgFiles[pkg] {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != name {
				continue
			}
			if fd.Type.Results == nil || len(fd.Type.Results.List) == 0 {
				return ""
			}
			return typeKeyOf(fd.Type.Results.List[0].Type, pkg)
		}
	}
	return ""
}

// Тип значения справа от := — что даёт признать переменную несущей: прямой
// литерал (в т.ч. &T{}) или вызов функции пакета (любой файл, любая арность).
func rhsTypeKey(rhs ast.Expr, ctx *kindResolveCtx, pkg string) string {
	if u, ok := rhs.(*ast.UnaryExpr); ok && u.Op == token.AND {
		rhs = u.X
	}
	switch v := rhs.(type) {
	case *ast.CompositeLit:
		return compositeLitTypeKey(v, pkg)
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		if !ok {
			return ""
		}
		return funcReturnTypeKeyInPackage(ctx, pkg, id.Name)
	}
	return ""
}

func collectFuncInfo(pkg, filePath string, decl *ast.FuncDecl, ctx *kindResolveCtx) *kindFuncInfo {
	fi := &kindFuncInfo{
		pkg: pkg, filePath: filePath, decl: decl,
		varType: map[string]string{}, localAssign: map[string][]ast.Expr{},
	}
	if decl.Recv != nil && len(decl.Recv.List) == 1 && len(decl.Recv.List[0].Names) == 1 {
		fi.varType[decl.Recv.List[0].Names[0].Name] = typeKeyOf(decl.Recv.List[0].Type, pkg)
	}
	if decl.Type.Params != nil {
		for _, field := range decl.Type.Params.List {
			tk := typeKeyOf(field.Type, pkg)
			if len(field.Names) == 0 {
				fi.paramOrder = append(fi.paramOrder, "")
				continue
			}
			for _, name := range field.Names {
				fi.varType[name.Name] = tk
				fi.paramOrder = append(fi.paramOrder, name.Name)
			}
		}
	}
	if decl.Body == nil {
		return fi
	}
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || i >= len(node.Rhs) {
					continue
				}
				fi.localAssign[id.Name] = append(fi.localAssign[id.Name], node.Rhs[i])
				if node.Tok == token.DEFINE && ctx != nil {
					if tk := rhsTypeKey(node.Rhs[i], ctx, pkg); tk != "" {
						fi.varType[id.Name] = tk
					}
				}
			}
		case *ast.DeclStmt:
			gd, ok := node.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				tk := typeKeyOf(vs.Type, pkg)
				if tk == "" {
					continue
				}
				for _, name := range vs.Names {
					fi.varType[name.Name] = tk
				}
			}
		}
		return true
	})
	return fi
}

func compositeLitTypeKey(cl *ast.CompositeLit, pkg string) string {
	if cl.Type == nil {
		return ""
	}
	return typeKeyOf(cl.Type, pkg)
}

// map[string]any{...} — тип написан буквально так: FormState и подобные
// именованные типы (SelectorExpr/Ident, не MapType) сюда не попадают.
func isAnonymousStringAnyMap(cl *ast.CompositeLit) bool {
	mt, ok := cl.Type.(*ast.MapType)
	if !ok {
		return false
	}
	key, ok := mt.Key.(*ast.Ident)
	if !ok || key.Name != "string" {
		return false
	}
	val, ok := mt.Value.(*ast.Ident)
	return ok && val.Name == "any"
}

type kindSink struct {
	pos    token.Position
	value  ast.Expr // nil, если reason уже окончательный — резолвить нечего
	reason string
}

// Две формы синка: поле/ключ вида в композитном литерале при создании, и
// присвоение x.Kind = ... после — для значения и указателя одинаково.
func findKindSinks(fset *token.FileSet, pkg string, fn *kindFuncInfo, body ast.Node) []kindSink {
	var sinks []kindSink
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			carrier := kindCarrierTypes[compositeLitTypeKey(node, pkg)]
			anonMap := isAnonymousStringAnyMap(node)
			if !carrier && !anonMap {
				return true
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				isField := carrier && func() bool { id, ok := kv.Key.(*ast.Ident); return ok && id.Name == "Kind" }()
				isMapKey := anonMap && func() bool {
					lit, ok := kv.Key.(*ast.BasicLit)
					return ok && lit.Kind == token.STRING && unquoteGoString(lit.Value) == "kind"
				}()
				if isField || isMapKey {
					sinks = append(sinks, kindSink{pos: fset.Position(kv.Value.Pos()), value: kv.Value})
				}
			}
		case *ast.AssignStmt:
			if fn == nil || node.Tok != token.ASSIGN {
				return true
			}
			for i, lhs := range node.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Kind" || i >= len(node.Rhs) {
					continue
				}
				base, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				typ, known := fn.varType[base.Name]
				switch {
				case known && kindCarrierTypes[typ]:
					sinks = append(sinks, kindSink{pos: fset.Position(node.Rhs[i].Pos()), value: node.Rhs[i]})
				case !known:
					// Тип не выведен вообще — не «точно не несущая», а «не смогли
					// проверить»; неизвестность здесь считается нарушением, не пропуском.
					pos := fset.Position(sel.Pos())
					sinks = append(sinks, kindSink{pos: pos, reason: fmt.Sprintf(
						"cannot determine the type of %q to know whether it carries a webhook kind — extend type inference (var decl, or a package function with a resolvable return type)", base.Name)})
				}
			}
		}
		return true
	})
	return sinks
}

type kindResolveCtx struct {
	fset     *token.FileSet
	consts   map[string]map[string]ast.Expr // package -> const name -> value expr
	files    map[string]*kindFileInfo       // file path -> package/AST, для трассировки вызовов
	pkgFiles map[string][]*ast.File         // package -> все его файлы, для поиска функций-строителей
}

// resolveKindExpr допускает только ссылку notify.Kind* или цепочку к ней;
// возвращаемый путь называет весь маршрут резолвинга, а не только причину.
func resolveKindExpr(e ast.Expr, ctx *kindResolveCtx, pkg string, fn *kindFuncInfo, depth int) (bool, []string) {
	if depth > maxKindResolveDepth {
		return false, []string{"resolution chain too deep (possible cycle)"}
	}
	switch v := e.(type) {
	case *ast.SelectorExpr:
		xid, ok := v.X.(*ast.Ident)
		if !ok {
			return false, []string{"selector base is not a simple identifier"}
		}
		if xid.Name == "notify" && strings.HasPrefix(v.Sel.Name, "Kind") {
			return true, nil
		}
		if rhs, ok := ctx.consts[xid.Name][v.Sel.Name]; ok {
			ok2, trace := resolveKindExpr(rhs, ctx, xid.Name, nil, depth+1)
			step := fmt.Sprintf("via constant %s.%s", xid.Name, v.Sel.Name)
			return ok2, append([]string{step}, trace...)
		}
		if v.Sel.Name == "Kind" && fn != nil {
			if typ, ok := fn.varType[xid.Name]; ok && kindCarrierTypes[typ] {
				return true, nil
			}
		}
		return false, []string{fmt.Sprintf("%s.%s is neither notify.Kind*, a known constant, nor a known carrier's field", xid.Name, v.Sel.Name)}
	case *ast.Ident:
		if rhs, ok := ctx.consts[pkg][v.Name]; ok {
			ok2, trace := resolveKindExpr(rhs, ctx, pkg, nil, depth+1)
			step := fmt.Sprintf("via constant %s.%s", pkg, v.Name)
			return ok2, append([]string{step}, trace...)
		}
		if fn != nil {
			if rhss, ok := fn.localAssign[v.Name]; ok {
				for _, rhs := range rhss {
					if ok2, trace := resolveKindExpr(rhs, ctx, pkg, fn, depth+1); !ok2 {
						step := fmt.Sprintf("via local variable %q in %s()", v.Name, fn.decl.Name.Name)
						return false, append([]string{step}, trace...)
					}
				}
				return true, nil
			}
			if _, isParam := fn.varType[v.Name]; isParam {
				return resolveParamAtCallSites(v.Name, fn, ctx, depth)
			}
		}
		return false, []string{fmt.Sprintf("%q is not a local assignment, a parameter, or a known constant", v.Name)}
	default:
		return false, []string{"not a reference — literal, call, or computed expression"}
	}
}

// Параметр разрешается через аргумент на КАЖДОМ вызове функции в её файле;
// ноль найденных вызовов — тоже провал (слепая проверка), не согласие.
func resolveParamAtCallSites(param string, fn *kindFuncInfo, ctx *kindResolveCtx, depth int) (bool, []string) {
	idx := -1
	for i, name := range fn.paramOrder {
		if name == param {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, []string{"parameter index not found (scanner bug)"}
	}
	fi, ok := ctx.files[fn.filePath]
	if !ok {
		return false, []string{"file info missing for parameter resolution (scanner bug)"}
	}
	calls := findCallSitesInFile(fi.file, fi.pkg, fn.filePath, fn.decl.Name.Name, ctx)
	if len(calls) == 0 {
		return false, []string{fmt.Sprintf("parameter %q of %s() has no call site in its file (blind guard)", param, fn.decl.Name.Name)}
	}
	for _, c := range calls {
		callPos := ctx.fset.Position(c.call.Pos())
		if idx >= len(c.call.Args) {
			return false, []string{fmt.Sprintf("call to %s() at %s:%d has fewer arguments than parameter %q",
				fn.decl.Name.Name, callPos.Filename, callPos.Line, param)}
		}
		if ok2, trace := resolveKindExpr(c.call.Args[idx], ctx, fi.pkg, c.enclosing, depth+1); !ok2 {
			step := fmt.Sprintf("via parameter %q of %s(), called at %s:%d", param, fn.decl.Name.Name, callPos.Filename, callPos.Line)
			return false, append([]string{step}, trace...)
		}
	}
	return true, nil
}

type kindCallSite struct {
	call      *ast.CallExpr
	enclosing *kindFuncInfo
}

// Вызовы вида receiver.Method(...) ИЛИ func(...) с совпадающим именем —
// этого достаточно для приватных функций/методов, вызываемых внутри своего файла.
func findCallSitesInFile(f *ast.File, pkg, filePath, name string, ctx *kindResolveCtx) []kindCallSite {
	var sites []kindCallSite
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		enclosing := collectFuncInfo(pkg, filePath, fd, ctx)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel.Name != name {
					return true
				}
			case *ast.Ident:
				if fn.Name != name {
					return true
				}
			default:
				return true
			}
			sites = append(sites, kindCallSite{call, enclosing})
			return true
		})
	}
	return sites
}

func buildKindConstTable(tree *Tree) (map[string]map[string]ast.Expr, *token.FileSet, map[string]*ast.File) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	consts := map[string]map[string]ast.Expr{}
	for _, gf := range tree.GoFiles {
		if gf.Generated || strings.HasSuffix(gf.Path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, gf.Path, gf.Body, 0)
		if err != nil {
			continue
		}
		parsed[gf.Path] = f
		pkg := f.Name.Name
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if consts[pkg] == nil {
						consts[pkg] = map[string]ast.Expr{}
					}
					consts[pkg][name.Name] = vs.Values[i]
				}
			}
		}
	}
	return consts, fset, parsed
}

// Проверяет ПОЛЕ несущей структуры (notify.Kind* либо падение при неизвестном
// типе; отражение/сериализация/кодоген — нет), не тело вебхука: от подмены поля через Extra бережёт порядок сборки в Dispatch, не этот тест.
func TestWebhookKindFieldsAreRegistryReferences(t *testing.T) {
	tree := Load(t)
	consts, fset, parsed := buildKindConstTable(tree)
	if len(parsed) < 100 {
		t.Fatalf("blind guard: only parsed %d files — the scanner is broken", len(parsed))
	}

	files := map[string]*kindFileInfo{}
	pkgFiles := map[string][]*ast.File{}
	for path, f := range parsed {
		files[path] = &kindFileInfo{pkg: f.Name.Name, file: f}
		pkgFiles[f.Name.Name] = append(pkgFiles[f.Name.Name], f)
	}
	ctx := &kindResolveCtx{fset: fset, consts: consts, files: files, pkgFiles: pkgFiles}

	sinkCount := 0
	for path, f := range parsed {
		pkg := f.Name.Name
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := collectFuncInfo(pkg, path, fd, ctx)
			for _, sink := range findKindSinks(fset, pkg, fn, fd.Body) {
				sinkCount++
				if sink.reason != "" {
					t.Errorf("%s:%d: %s", sink.pos.Filename, sink.pos.Line, sink.reason)
					continue
				}
				if ok2, trace := resolveKindExpr(sink.value, ctx, pkg, fn, 0); !ok2 {
					t.Errorf("%s:%d: kind value is not a notify.Kind* reference — %s",
						sink.pos.Filename, sink.pos.Line, strings.Join(trace, " -> "))
				}
			}
		}
	}
	if sinkCount < 15 {
		t.Fatalf("blind guard: found only %d webhook kind sinks — the scanner is broken", sinkCount)
	}
}
