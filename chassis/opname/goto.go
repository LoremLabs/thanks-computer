package opname

import (
	"fmt"
	"regexp"
	"strings"
)

// GotoScheme is the EXEC scheme of a stage jump written as a scheme:
// `EXEC "goto://<stack>/<scope>"` or `EXEC "goto://<scope>"`.
const GotoScheme = "goto://"

// gotoScope is a scope number: what follows the last "/" of a stage, or the
// whole of a bare target. Nine digits keeps it inside an int everywhere.
var gotoScope = regexp.MustCompile(`^[0-9]{1,9}$`)

// GotoTarget parses an `EXEC "goto://…"` value and returns its target, the
// value the jump writes to `_txc.goto`. The target is either `<stack>/<scope>`
// (a stage, the stack a valid stack name) or a bare `<scope>` in the current
// stack, exactly the two forms `EMIT @goto` takes. The target is literal: a
// jump computed at run time is `EMIT @goto = ._next`.
//
// It is the one check behind the parser (an apply-time error), lint and the
// processor (a runtime error), so all three agree on what is a target.
func GotoTarget(exec string) (string, error) {
	if !strings.HasPrefix(exec, GotoScheme) {
		return "", fmt.Errorf("%q is not a %s target", exec, GotoScheme)
	}
	target := strings.TrimPrefix(exec, GotoScheme)
	if gotoScope.MatchString(target) {
		return target, nil
	}
	i := strings.LastIndex(target, "/")
	if i <= 0 || !gotoScope.MatchString(target[i+1:]) {
		return "", fmt.Errorf("EXEC %q needs a target: %s<stack>/<scope> (e.g. %sbilling/100) or %s<scope> in this stack",
			exec, GotoScheme, GotoScheme, GotoScheme)
	}
	if err := ValidStack(target[:i]); err != nil {
		return "", fmt.Errorf("EXEC %q: %w", exec, err)
	}
	return target, nil
}
