package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// runCallOrder returns, in source order, the selector calls made directly in
// run()'s body (not inside the goroutine or closure literals it starts), as
// "receiver.Method" strings, plus whether run() calls os.Exit anywhere.
func runCallOrder(t *testing.T) (calls []string, exits bool) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "run" && fn.Recv == nil {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("run() not found in main.go")
	}
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			// What a goroutine or closure does happens when it runs, not where it
			// is written; only run()'s own straight-line sequence is the order.
			ast.Inspect(v.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if name := selectorName(call); name == "os.Exit" {
						exits = true
					}
				}
				return true
			})
			return false
		case *ast.CallExpr:
			if name := selectorName(v); name != "" {
				calls = append(calls, name)
				if name == "os.Exit" {
					exits = true
				}
			}
		}
		return true
	})
	return calls, exits
}

func selectorName(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name + "." + sel.Sel.Name
}

// TestStartupOrderFailsBeforeAnythingIsAccepted pins the construction order in
// run(): everything that can fail on misconfiguration or on a stale instance
// (the update state's directory, file and instance lock) is bound before any
// listener accepts a connection or a pipeline goroutine starts, and signals are
// disposed of before the first blocking startup wait, so a stop or a mis-aimed
// SIGHUP during startup is held rather than terminating the process. Every
// failure path returns from run() so the deferred cleanups run; os.Exit is
// reserved for main().
func TestStartupOrderFailsBeforeAnythingIsAccepted(t *testing.T) {
	calls, exits := runCallOrder(t)
	if exits {
		t.Error("run() calls os.Exit, bypassing the ordered shutdown and every deferred cleanup")
	}
	index := func(name string) int {
		for i, c := range calls {
			if c == name {
				return i
			}
		}
		t.Fatalf("run() has no direct call %s; the order test must be updated with the code", name)
		return -1
	}
	before := func(earlier, later string) {
		t.Helper()
		if e, l := index(earlier), index(later); e >= l {
			t.Errorf("%s (call #%d) must precede %s (call #%d) in run()", earlier, e, later, l)
		}
	}
	// Signal disposition precedes the first startup wait that can take seconds.
	before("signal.Notify", "authorization.NewDatabase")
	before("signal.Ignore", "authorization.NewDatabase")
	// The update state (directory, file, instance lock) is opened before any
	// listener binds, and before the historian or any producer starts.
	before("updates.Open", "obs.Listen")
	before("updates.Open", "net.Listen")
	before("updates.Open", "pushSrv.Listen")
	before("updates.Open", "store.New")
	// ingestWG.Add is the first producer start (mgr.Run runs in the goroutine
	// it accounts for, so the Add is the direct call that marks it).
	before("updates.Open", "ingestWG.Add")
	// Listeners bind before the historian and the producers start, so a
	// startup address conflict needs no drain.
	before("obs.Listen", "store.New")
	before("pushSrv.Listen", "store.New")
	before("store.New", "ingestWG.Add")
}
