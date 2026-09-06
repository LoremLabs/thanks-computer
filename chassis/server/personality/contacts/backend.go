package contacts

import (
	"context"
	"fmt"
	"net/http"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"

	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
)

// backend adapts the store to go-webdav's carddav.Backend for what the
// library still serves: the home set and address book PROPFINDs, OPTIONS,
// MKCOL, DELETE routing and addressbook-query. PUT, GET and multiget never
// reach it (objects.go). The principal comes from the request context
// (handler.go authenticated it); every path is checked against it, so one
// account never sees another's tree.
type backend struct{ c *Controller }

func httpErr(code int, format string, a ...any) error {
	return webdav.NewHTTPError(code, fmt.Errorf(format, a...))
}

var supportedAddressData = []carddav.AddressDataType{
	{ContentType: vcard.MIMEType, Version: "3.0"},
	{ContentType: vcard.MIMEType, Version: "4.0"},
}

func (b *backend) principal(ctx context.Context) (principal, error) {
	pr, ok := principalFrom(ctx)
	if !ok {
		return principal{}, httpErr(http.StatusUnauthorized, "no principal")
	}
	return pr, nil
}

func (b *backend) principalPath(pr principal) string { return b.c.prefix + "/" + pr.username + "/" }
func (b *backend) homePath(pr principal) string      { return b.principalPath(pr) + "addressbooks/" }
func (b *backend) bookPath(pr principal, name string) string {
	return b.homePath(pr) + name + "/"
}

// parse resolves a request path to (address book name, resource name).
// depth is 3 for a book, 4 for an object; anything else is refused.
func (b *backend) parse(ctx context.Context, path string) (pr principal, bookName, resource string, depth int, err error) {
	pr, err = b.principal(ctx)
	if err != nil {
		return
	}
	parts := b.c.pathParts(path)
	depth = len(parts)
	if depth < 1 || parts[0] != pr.username {
		return pr, "", "", depth, httpErr(http.StatusForbidden, "not your principal")
	}
	if depth >= 2 && parts[1] != "addressbooks" {
		return pr, "", "", depth, httpErr(http.StatusNotFound, "no such collection")
	}
	if depth >= 3 {
		bookName = parts[2]
	}
	if depth >= 4 {
		resource = parts[3]
	}
	if depth > 4 {
		return pr, "", "", depth, httpErr(http.StatusNotFound, "no such resource")
	}
	return
}

func (b *backend) bookAt(ctx context.Context, path string, wantDepth int) (principal, chcon.Addressbook, string, error) {
	pr, bookName, resource, depth, err := b.parse(ctx, path)
	if err != nil {
		return pr, chcon.Addressbook{}, "", err
	}
	if depth != wantDepth {
		return pr, chcon.Addressbook{}, "", httpErr(http.StatusNotFound, "no such resource")
	}
	ab, found, err := b.c.store.GetAddressbook(ctx, pr.tenant, pr.username, bookName)
	if err != nil {
		return pr, chcon.Addressbook{}, "", httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	if !found {
		return pr, chcon.Addressbook{}, "", httpErr(http.StatusNotFound, "no such address book")
	}
	return pr, ab, resource, nil
}

func (b *backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	pr, err := b.principal(ctx)
	if err != nil {
		return "", err
	}
	return b.principalPath(pr), nil
}

func (b *backend) AddressBookHomeSetPath(ctx context.Context) (string, error) {
	pr, err := b.principal(ctx)
	if err != nil {
		return "", err
	}
	return b.homePath(pr), nil
}

func (b *backend) toBook(pr principal, ab chcon.Addressbook) carddav.AddressBook {
	name := ab.DisplayName
	if name == "" {
		name = ab.Name
	}
	return carddav.AddressBook{Path: b.bookPath(pr, ab.Name), Name: name, Description: ab.Description,
		MaxResourceSize: b.c.maxBytes, SupportedAddressData: supportedAddressData}
}

func (b *backend) ListAddressBooks(ctx context.Context) ([]carddav.AddressBook, error) {
	pr, err := b.principal(ctx)
	if err != nil {
		return nil, err
	}
	books, err := b.c.store.ListAddressbooks(ctx, pr.tenant, pr.username)
	if err != nil {
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	out := make([]carddav.AddressBook, 0, len(books))
	for _, ab := range books {
		out = append(out, b.toBook(pr, ab))
	}
	return out, nil
}

func (b *backend) GetAddressBook(ctx context.Context, path string) (*carddav.AddressBook, error) {
	pr, ab, _, err := b.bookAt(ctx, path, 3)
	if err != nil {
		return nil, err
	}
	out := b.toBook(pr, ab)
	return &out, nil
}

// CreateAddressBook is the library's MKCOL (extended or plain) at depth 3:
// the `mkaddressbook` policy decides, default deny.
func (b *backend) CreateAddressBook(ctx context.Context, ab *carddav.AddressBook) error {
	pr, bookName, _, depth, err := b.parse(ctx, ab.Path)
	if err != nil {
		return err
	}
	if depth != 3 {
		return httpErr(http.StatusForbidden, "address books live under %s", b.homePath(pr))
	}
	_, status, msg := b.c.createAddressbook(ctx, pr, bookName, map[string]string{"displayname": ab.Name, "description": ab.Description})
	if status != 0 {
		return httpErr(status, "%s", msg)
	}
	return nil
}

// DeleteAddressBook is the library's DELETE at depth 3: the `remove`
// policy decides, default deny.
func (b *backend) DeleteAddressBook(ctx context.Context, path string) error {
	pr, ab, _, err := b.bookAt(ctx, path, 3)
	if err != nil {
		return err
	}
	m := mutation{tenant: pr.tenant, account: pr.username, op: opRemove, addressbook: refOf(ab), clientIP: pr.clientIP}
	if status, msg := b.c.gate(&ab, &pr.acct, chcon.VerbRemove, &m); status != 0 {
		return httpErr(status, "%s", msg)
	}
	if _, err := b.c.store.RemoveAddressbook(ctx, ab.ID); err != nil {
		return httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	b.c.after(&ab, &pr.acct, chcon.VerbRemove, m)
	return nil
}

func (b *backend) toObject(pr principal, ab chcon.Addressbook, o chcon.Object) (carddav.AddressObject, error) {
	card, err := chcon.ToLibrary(o.VCard)
	if err != nil {
		return carddav.AddressObject{}, httpErr(http.StatusInternalServerError, "stored object unreadable: %v", err)
	}
	return carddav.AddressObject{Path: b.bookPath(pr, ab.Name) + o.Name, ModTime: o.UpdatedAt, ContentLength: o.Size, ETag: o.ETag, Card: card}, nil
}

func (b *backend) GetAddressObject(ctx context.Context, path string, req *carddav.AddressDataRequest) (*carddav.AddressObject, error) {
	pr, ab, resource, err := b.bookAt(ctx, path, 4)
	if err != nil {
		return nil, err
	}
	o, found, err := b.c.store.GetObject(ctx, ab.ID, resource)
	if err != nil {
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	if !found {
		return nil, httpErr(http.StatusNotFound, "no such object")
	}
	ao, err := b.toObject(pr, ab, o)
	if err != nil {
		return nil, err
	}
	return &ao, nil
}

func (b *backend) listObjects(ctx context.Context, path string) ([]carddav.AddressObject, error) {
	pr, ab, _, err := b.bookAt(ctx, path, 3)
	if err != nil {
		return nil, err
	}
	objs, err := b.c.store.ListObjects(ctx, ab.ID, chcon.ListOpts{})
	if err != nil {
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	out := make([]carddav.AddressObject, 0, len(objs))
	for _, o := range objs {
		ao, err := b.toObject(pr, ab, o)
		if err != nil {
			continue
		}
		out = append(out, ao)
	}
	return out, nil
}

func (b *backend) ListAddressObjects(ctx context.Context, path string, req *carddav.AddressDataRequest) ([]carddav.AddressObject, error) {
	return b.listObjects(ctx, path)
}

// QueryAddressObjects is addressbook-query: every live object through the
// library's Filter (first field of a property only, case-sensitive text
// match — its limits, documented). A Filter error keeps the objects rather
// than failing the report.
func (b *backend) QueryAddressObjects(ctx context.Context, path string, query *carddav.AddressBookQuery) ([]carddav.AddressObject, error) {
	aos, err := b.listObjects(ctx, path)
	if err != nil {
		return nil, err
	}
	if query == nil {
		return aos, nil
	}
	filtered, ferr := carddav.Filter(query, aos)
	if ferr != nil {
		return aos, nil
	}
	return filtered, nil
}

// PutAddressObject is unreachable: handler.go answers every PUT itself so
// a client's bytes are stored as written.
func (b *backend) PutAddressObject(ctx context.Context, path string, card vcard.Card, opts *carddav.PutAddressObjectOptions) (*carddav.AddressObject, error) {
	return nil, httpErr(http.StatusMethodNotAllowed, "PUT is served by the head")
}

func (b *backend) DeleteAddressObject(ctx context.Context, path string) error {
	pr, ab, resource, err := b.bookAt(ctx, path, 4)
	if err != nil {
		return err
	}
	existing, found, err := b.c.store.GetObject(ctx, ab.ID, resource)
	if err != nil {
		return httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	if !found {
		return httpErr(http.StatusNotFound, "no such object")
	}
	m := mutation{tenant: pr.tenant, account: pr.username, op: opDelete, addressbook: refOf(ab),
		object:   &objRef{Name: resource, UID: existing.UID, ETag: existing.ETag, PriorETag: existing.ETag, Size: existing.Size, Exists: true},
		clientIP: pr.clientIP}
	if pc, err := chcon.Parse(existing.VCard); err == nil {
		m.prior = &pc
	}
	if status, msg := b.c.gate(&ab, &pr.acct, chcon.VerbDelete, &m); status != 0 {
		return httpErr(status, "%s", msg)
	}
	if _, _, err := b.c.store.DeleteObject(ctx, ab.ID, resource); err != nil {
		return httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	b.c.after(&ab, &pr.acct, chcon.VerbDelete, m)
	return nil
}
