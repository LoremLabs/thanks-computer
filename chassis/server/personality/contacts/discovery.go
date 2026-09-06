package contacts

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// discovery.go — the two PROPFINDs a client's account setup performs
// before it knows any address book: the root (`current-user-principal`)
// and the principal (`addressbook-home-set`). The head answers these itself
// because go-webdav v0.7.0 answers a root PROPFIND with a response whose
// href is the PRINCIPAL's path, not the requested root (the same bug the
// calendar head met on prod 2026-09-05). Every href here echoes the
// request's own path as the client encoded it, so `%40` and `@` never
// disagree.

type propfindReq struct {
	XMLName  xml.Name
	AllProp  *struct{} `xml:"allprop"`
	PropName *struct{} `xml:"propname"`
	Prop     *struct {
		Any []propElem `xml:",any"`
	} `xml:"prop"`
}

// readPropfind returns the requested property names (nil ⇒ allprop).
func readPropfind(r *http.Request) (names []xml.Name, allprop bool, err error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil, true, nil
	}
	var pf propfindReq
	if err := xml.Unmarshal(body, &pf); err != nil {
		return nil, false, err
	}
	if pf.Prop == nil || pf.AllProp != nil || pf.PropName != nil {
		return nil, true, nil
	}
	for _, p := range pf.Prop.Any {
		names = append(names, p.XMLName)
	}
	return names, false, nil
}

func hrefEsc(p string) string { return xmlEscape(p) }

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// requestPath is the path exactly as the client sent it (percent-encoding
// preserved), with a trailing slash for a collection.
func requestPath(r *http.Request) string {
	p := r.URL.EscapedPath()
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// serveDiscoveryPropfind answers PROPFIND on the root (depth 0 under the
// prefix) or the principal (depth 1). Every other depth is the library's.
func (c *Controller) serveDiscoveryPropfind(w http.ResponseWriter, r *http.Request, pr principal, parts []string) {
	names, allprop, err := readPropfind(r)
	if err != nil {
		http.Error(w, "malformed propfind", http.StatusBadRequest)
		return
	}
	self := requestPath(r)
	principalPath := c.prefix + "/" + strings.ReplaceAll(url.PathEscape(pr.username), "@", "%40") + "/"
	if len(parts) == 1 {
		principalPath = self // echo the client's own encoding of its principal
	}
	homePath := principalPath + "addressbooks/"

	avail := map[xml.Name]string{
		{Space: nsDAV, Local: "current-user-principal"}: `<D:current-user-principal><D:href>` + hrefEsc(principalPath) + `</D:href></D:current-user-principal>`,
		{Space: nsDAV, Local: "principal-URL"}:          `<D:principal-URL><D:href>` + hrefEsc(principalPath) + `</D:href></D:principal-URL>`,
	}
	if len(parts) == 0 {
		avail[xml.Name{Space: nsDAV, Local: "resourcetype"}] = `<D:resourcetype><D:collection/></D:resourcetype>`
		avail[xml.Name{Space: nsDAV, Local: "displayname"}] = `<D:displayname>Contacts</D:displayname>`
	} else {
		avail[xml.Name{Space: nsDAV, Local: "resourcetype"}] = `<D:resourcetype><D:collection/><D:principal/></D:resourcetype>`
		avail[xml.Name{Space: nsDAV, Local: "displayname"}] = `<D:displayname>` + xmlEscape(pr.username) + `</D:displayname>`
		avail[xml.Name{Space: nsCardDAV, Local: "addressbook-home-set"}] = `<C:addressbook-home-set><D:href>` + hrefEsc(homePath) + `</D:href></C:addressbook-home-set>`
		avail[xml.Name{Space: nsCardDAV, Local: "principal-address"}] = `<C:principal-address><D:href>mailto:` + xmlEscape(pr.username) + `</D:href></C:principal-address>`
	}

	var ok, missing []string
	if allprop {
		for _, frag := range avail {
			ok = append(ok, frag)
		}
	} else {
		for _, n := range names {
			if frag, found := avail[n]; found {
				ok = append(ok, frag)
			} else {
				missing = append(missing, fmt.Sprintf(`<x:%s xmlns:x="%s"/>`, xmlEscape(n.Local), xmlEscape(n.Space)))
			}
		}
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>` + hrefEsc(self) + `</D:href>`)
	if len(ok) > 0 {
		b.WriteString(`<D:propstat><D:prop>` + strings.Join(ok, "") + `</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>`)
	}
	if len(missing) > 0 {
		b.WriteString(`<D:propstat><D:prop>` + strings.Join(missing, "") + `</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>`)
	}
	b.WriteString(`</D:response></D:multistatus>`)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("DAV", "1, 3, addressbook")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write([]byte(b.String()))
}
