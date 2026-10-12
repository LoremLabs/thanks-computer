package opname

import (
	"fmt"
	"strconv"
	"strings"
)

// CallScheme is the EXEC scheme of a stack call: `EXEC "call://<stack>"`
// runs another stack in process, inside the same request, and answers with
// one result object; `EXEC "call://<stack>/<scope>"` enters it at a floor.
const CallScheme = "call://"

// CallTarget parses an `EXEC "call://…"` value into the stack to call and
// the scope to enter it at. A target is `<stack>` (entered at scope 0) or
// `<stack>/<scope>`: the last segment is the scope when it is all digits,
// exactly as a goto target is read, so a stack whose last segment is a
// number cannot be called at its floor (name it with a scope). The target is
// literal; a stack chosen at run time is `WITH stack = ._next`.
//
// It is the one check behind the parser (an apply-time error), lint and the
// processor (a result error), so all three agree on what is a target.
func CallTarget(exec string) (stack string, scope int, err error) {
	if !strings.HasPrefix(exec, CallScheme) {
		return "", 0, fmt.Errorf("%q is not a %s target", exec, CallScheme)
	}
	target := strings.TrimPrefix(exec, CallScheme)
	if target == "" {
		return "", 0, fmt.Errorf("EXEC %q needs a target: %s<stack> (e.g. %sbilling) or %s<stack>/<scope>",
			exec, CallScheme, CallScheme, CallScheme)
	}
	stack = target
	if i := strings.LastIndex(target, "/"); i > 0 && gotoScope.MatchString(target[i+1:]) {
		stack = target[:i]
		scope, _ = strconv.Atoi(target[i+1:])
	}
	if err := ValidStack(stack); err != nil {
		return "", 0, fmt.Errorf("EXEC %q: %w", exec, err)
	}
	return stack, scope, nil
}
