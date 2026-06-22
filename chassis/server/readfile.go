package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/server/static"
)

// readFile is the handler body for `txco://read-file`: it reads one or
// more of the routed stack's static FILES/ assets and writes their bytes
// into the envelope as data, so a later op can template, hash, or relay
// them. It's the bridge between `txco://static` (which only SERVES a file
// as an HTTP response) and the rest of a rule — e.g. read FILES/_mail/
// welcome.html and hand it to `txco://sendmail`.
//
// Reading goes through the in-memory static Index (and the filecas store
// for tenant CAS entries), never the filesystem — so it stays off the
// request hot path. Unlike HTTP serving, it CAN read `_`-private assets
// (the FILES/_mail/ templating case); cross-tenant reads are impossible
// (the index is keyed by routed tenant+stack).
//
// WITH parameters (op.Meta):
//
//	files = [{"path":"_mail/welcome.html","as":"welcome"}]   (required)
//	into  = "_files"        (optional: destination subtree, default "_files")
//	encode = "auto"         (optional: "auto" | "utf8" | "base64")
//	strict = false          (optional: a miss / over-cap fails the op)
//	max_bytes = 1048576     (optional: per-file cap; overrides config)
//
// `as` is required, unique, and a clean key segment (no "." or "/"): the
// output is keyed strictly by `as` so downstream addressing stays clean
// and decoupled from the on-disk path. Output (default "_files"):
//
//	{"_files":{"welcome":{"found":true,"content":"…","encoding":"utf8",
//	  "ctype":"text/html; charset=utf-8","size":1234,"path":"_mail/welcome.html"}}}
//
// The result is an OBJECT keyed by `as` (never an array): the scope merge
// recursively merges objects but APPENDS arrays, so an object keeps the
// op idempotent across re-execution (a loop or goto). The default `into`
// is "_files", a `_`-prefixed key the default web projection drops, so
// file bytes never leak to a caller unless the author redirects them.
func readFile(ctx context.Context, ix *static.Index, fcas filecas.Store, in []byte, maxBytes int) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))

	files := gjson.GetBytes(meta, "files")
	if !files.IsArray() || len(files.Array()) == 0 {
		return readFileErr("read-file: `files` must be a non-empty array of {path, as}"),
			errors.New("read-file: missing `files`")
	}

	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_files"
	}

	encode := gjson.GetBytes(meta, "encode").String()
	if encode == "" {
		encode = "auto"
	}
	switch encode {
	case "auto", "utf8", "base64":
	default:
		return readFileErr(fmt.Sprintf("read-file: unsupported encode %q (auto|utf8|base64)", encode)),
			fmt.Errorf("read-file: unsupported encode %q", encode)
	}

	strict := gjson.GetBytes(meta, "strict").Bool()
	if mb := gjson.GetBytes(meta, "max_bytes"); mb.Exists() {
		maxBytes = int(mb.Int())
	}

	tenant := gjson.GetBytes(in, "_txc.route.tenant").String()
	stack := gjson.GetBytes(in, "_txc.route.stack").String()

	resp := `{}`
	seen := make(map[string]struct{}, len(files.Array()))

	for i, f := range files.Array() {
		path := f.Get("path").String()
		as := f.Get("as").String()
		if path == "" || as == "" {
			return readFileErr(fmt.Sprintf("read-file: files[%d] needs both `path` and `as`", i)),
				fmt.Errorf("read-file: files[%d] missing path/as", i)
		}
		if strings.ContainsAny(as, "./") {
			return readFileErr(fmt.Sprintf("read-file: files[%d] `as`=%q must not contain '.' or '/'", i, as)),
				fmt.Errorf("read-file: files[%d] bad as", i)
		}
		if _, dup := seen[as]; dup {
			return readFileErr(fmt.Sprintf("read-file: duplicate `as`=%q", as)),
				fmt.Errorf("read-file: duplicate as %q", as)
		}
		seen[as] = struct{}{}

		base := into + "." + as

		r, ok := ix.Asset(tenant, stack, path)
		var body []byte
		switch {
		case ok && r.Found && r.Hash != "":
			if fcas != nil {
				if b, err := fcas.Get(ctx, r.Hash); err == nil {
					body, ok = b, true
				} else {
					ok = false
				}
			} else {
				ok = false
			}
		case ok && r.Found:
			body = r.Body
		default:
			ok = false
		}

		if !ok {
			if strict {
				return readFileErr(fmt.Sprintf("read-file: %q not found", path)),
					fmt.Errorf("read-file: %q not found", path)
			}
			resp, _ = sjson.Set(resp, base+".found", false)
			resp, _ = sjson.Set(resp, base+".path", path)
			continue
		}

		origLen := len(body)
		truncated := false
		if maxBytes > 0 && len(body) > maxBytes {
			if strict {
				return readFileErr(fmt.Sprintf("read-file: %q exceeds max_bytes (%d > %d)", path, origLen, maxBytes)),
					fmt.Errorf("read-file: %q over cap", path)
			}
			body = body[:maxBytes]
			truncated = true
		}

		content, enc := encodeReadFile(body, encode)

		resp, _ = sjson.Set(resp, base+".found", true)
		resp, _ = sjson.Set(resp, base+".path", path)
		resp, _ = sjson.Set(resp, base+".encoding", enc)
		resp, _ = sjson.Set(resp, base+".ctype", r.Ctype)
		resp, _ = sjson.Set(resp, base+".size", origLen)
		if truncated {
			resp, _ = sjson.Set(resp, base+".truncated", true)
		}
		resp, _ = sjson.Set(resp, base+".content", content)
	}

	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// encodeReadFile renders file bytes per the requested encoding, returning
// the content string and the encoding actually used. "auto" emits a UTF-8
// string for valid text and base64 otherwise, so binary assets never
// produce invalid JSON.
func encodeReadFile(body []byte, encode string) (content, enc string) {
	switch encode {
	case "base64":
		return base64.StdEncoding.EncodeToString(body), "base64"
	case "utf8":
		return string(body), "utf8"
	default: // "auto"
		if utf8.Valid(body) {
			return string(body), "utf8"
		}
		return base64.StdEncoding.EncodeToString(body), "base64"
	}
}

// normReadFilePath maps a txcl-style envelope path to the dotted form
// sjson expects: leading "@" → "_txc." (the same sugar txcl applies),
// leading "." stripped. Mirrors ops.normalizePath for the `into` param.
func normReadFilePath(p string) string {
	if strings.HasPrefix(p, "@") {
		return "_txc." + strings.TrimPrefix(p, "@")
	}
	return strings.TrimPrefix(p, ".")
}

// readFileErr builds a structured error event.Payload (never includes
// file bytes — only the human-readable reason).
func readFileErr(msg string) event.Payload {
	em, _ := sjson.Set(`{}`, "error.0", "read-file-err")
	em, _ = sjson.Set(em, "errorMsg", msg)
	return event.Payload{Raw: `{}`, Type: event.Null, Meta: em}
}
