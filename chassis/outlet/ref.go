package outlet

import (
	"fmt"
	"net/url"
	"strings"
)

// SchemePrefix is the EXEC scheme every driver answers behind.
const SchemePrefix = "outlet://"

// IsOutletExec reports whether an EXEC target names an outlet.
func IsOutletExec(exec string) bool { return strings.HasPrefix(exec, SchemePrefix) }

// ParseRef splits `outlet://<name>/<op>` into its outlet name and operation.
// The URI carries nothing else on purpose: host, port, database and TLS mode
// come only from the declared secret's DSN, so a string with userinfo, a
// port, a query or a fragment is refused — at apply, and again here.
func ParseRef(exec string) (name, op string, err error) {
	if !IsOutletExec(exec) {
		return "", "", fmt.Errorf("not an outlet:// target: %q", exec)
	}
	u, perr := url.Parse(exec)
	if perr != nil {
		return "", "", fmt.Errorf("malformed outlet target %q", exec)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("outlet target %q must not carry credentials; the DSN lives in the declared secret", exec)
	}
	if u.Port() != "" {
		return "", "", fmt.Errorf("outlet target %q must not carry a port; the address lives in the declared secret", exec)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", "", fmt.Errorf("outlet target %q must be exactly outlet://<name>/<op>", exec)
	}
	name = u.Hostname()
	if !ValidName(name) {
		return "", "", fmt.Errorf("outlet target %q: name must match %s", exec, nameRe)
	}
	op = strings.TrimPrefix(u.Path, "/")
	switch op {
	case OpQuery, OpExec:
		return name, op, nil
	}
	return "", "", fmt.Errorf("outlet target %q: operation must be %q or %q", exec, OpQuery, OpExec)
}
