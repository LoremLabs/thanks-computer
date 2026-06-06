package mail

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	"regexp"
	"strings"
	"sync"
	texttemplate "text/template"
)

// defaultShellSrc is the bundled default email template — a responsive,
// CSS-inlined HTML shell (a Go port of the moonbase apps/mailer
// layouts/base.html), with {{.Subject}} as the title and {{.Body}} as the
// content slot. A tenant gets a decent-looking email with zero authoring;
// custom per-stack templates (Phase 2) override it.
//
//go:embed templates/default.html
var defaultShellSrc string

var (
	shellOnce sync.Once
	shellTmpl *htmltemplate.Template
	shellErr  error
)

func defaultShell() (*htmltemplate.Template, error) {
	shellOnce.Do(func() {
		shellTmpl, shellErr = htmltemplate.New("default").Parse(defaultShellSrc)
	})
	return shellTmpl, shellErr
}

type shellData struct {
	Subject   string
	Preheader string
	Body      htmltemplate.HTML // trusted: the tenant's own body, already rendered
}

// renderSubject renders the subject line as a text/template over the
// recipient's vars (so "Welcome {{.name}}" personalizes). Missing keys
// render empty rather than "<no value>".
func renderSubject(subjectSrc string, vars map[string]any) (string, error) {
	t, err := texttemplate.New("subject").Option("missingkey=zero").Parse(subjectSrc)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", err
	}
	// Subjects are single-line; collapse any stray newlines.
	return strings.TrimSpace(strings.ReplaceAll(buf.String(), "\n", " ")), nil
}

// renderBody renders the body as an html/template over the recipient's
// vars: the body's literal HTML is preserved, but interpolated var values
// ({{.name}}) are contextually auto-escaped. (The body itself is trusted
// tenant HTML; only the *vars* are escaped.) Missing keys render empty.
func renderBody(bodySrc string, vars map[string]any) (htmltemplate.HTML, error) {
	t, err := htmltemplate.New("body").Option("missingkey=zero").Parse(bodySrc)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", err
	}
	return htmltemplate.HTML(buf.String()), nil
}

// renderDefault wraps an already-rendered body in the default shell,
// producing the full HTML document.
func renderDefault(subject string, body htmltemplate.HTML, preheader string) (string, error) {
	t, err := defaultShell()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, shellData{Subject: subject, Preheader: preheader, Body: body}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

var (
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
	wsRe    = regexp.MustCompile(`[ \t]*\n[ \t\n]*`)
	spaceRe = regexp.MustCompile(`[ \t]{2,}`)
)

var (
	styleRe  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	scriptRe = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	brRe     = regexp.MustCompile(`(?i)<br\s*/?>`)
)

// htmlToText derives a plaintext alternative from rendered HTML: drop
// <style>/<script>, turn <br> into newlines, strip remaining tags, collapse
// whitespace. Good enough for the text/plain MIME part; a richer converter
// is a later refinement.
func htmlToText(html string) string {
	s := styleRe.ReplaceAllString(html, "")
	s = scriptRe.ReplaceAllString(s, "")
	s = brRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = wsRe.ReplaceAllString(s, "\n")
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
