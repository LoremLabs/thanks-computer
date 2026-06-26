package cli

import (
	"strings"
	"testing"

	"github.com/rivo/tview"
)

// TestBuildJSONTreeView_TopLevelOnly verifies the initial tree state:
// root's direct children are visible (collapsed), and nested
// containers below them are collapsed too. Toggling expands all;
// toggling again returns to the initial state.
func TestBuildJSONTreeView_TopLevelOnly(t *testing.T) {
	v := map[string]any{
		"name": "hello",
		"meta": map[string]any{
			"deep": map[string]any{"k": "v"},
		},
		"tags": []any{"a", "b"},
	}
	tv := buildJSONTreeView("input", v, "full")
	root := tv.GetRoot()

	children := root.GetChildren()
	if len(children) != 3 {
		t.Fatalf("expected 3 top-level children, got %d", len(children))
	}
	// Find the "meta" container child (sorted: meta, name, tags).
	var meta *tview.TreeNode
	for _, c := range children {
		if strings.Contains(c.GetText(), `"meta"`) {
			meta = c
			break
		}
	}
	if meta == nil {
		t.Fatal("did not find meta child")
	}
	if meta.IsExpanded() {
		t.Errorf("meta should be collapsed in top-level-only initial state")
	}

	// Toggle global → fully expanded.
	toggleJSONExpansion(tv, scopeWhole, true)
	if !meta.IsExpanded() {
		t.Errorf("meta should be expanded after toggle")
	}
	// Walk down: meta.deep should also be expanded.
	deep := meta.GetChildren()[0]
	if !deep.IsExpanded() {
		t.Errorf("meta.deep should be expanded after toggle")
	}

	// Toggle again → back to top-level only.
	toggleJSONExpansion(tv, scopeWhole, true)
	if meta.IsExpanded() {
		t.Errorf("meta should be collapsed after second toggle")
	}
}

// TestJSONNodeText_Coloring confirms the per-node label puts blue on
// object keys, gray on array indices, green on string values, and so
// on. Format only — actual rendering is tview's job.
func TestJSONNodeText_Coloring(t *testing.T) {
	cases := []struct {
		key      string
		val      any
		arrayIdx bool
		want     string
	}{
		{"name", "hello", false, `[blue::b]"name"[-:-:-]: [green]"hello"[-]`},
		{"count", float64(42), false, `[blue::b]"count"[-:-:-]: 42`},
		{"ok", true, false, `[blue::b]"ok"[-:-:-]: [yellow]true[-]`},
		{"n", nil, false, `[blue::b]"n"[-:-:-]: [gray]null[-]`},
		{"meta", map[string]any{"a": 1, "b": 2}, false, `[blue::b]"meta"[-:-:-]: {`},
		{"tags", []any{"a", "b", "c"}, false, `[blue::b]"tags"[-:-:-]: [`},
		{"0", "first", true, `[gray]\[0][-] [green]"first"[-]`},
		// Compactions:
		{"empty_obj", map[string]any{}, false, `[blue::b]"empty_obj"[-:-:-]: {}`},
		{"empty_arr", []any{}, false, `[blue::b]"empty_arr"[-:-:-]: []`},
		{"solo_str", []any{"hello"}, false, `[blue::b]"solo_str"[-:-:-]: [ [green]"hello"[-] ]`},
		{"solo_num", []any{float64(42)}, false, `[blue::b]"solo_num"[-:-:-]: [ 42 ]`},
		{"solo_obj", []any{map[string]any{"a": 1}}, false, `[blue::b]"solo_obj"[-:-:-]: [`},
	}
	for _, c := range cases {
		got := jsonNodeText(c.key, c.val, c.arrayIdx)
		if got != c.want {
			t.Errorf("jsonNodeText(%q, %v, arrayIdx=%v):\n  got:  %s\n  want: %s",
				c.key, c.val, c.arrayIdx, got, c.want)
		}
	}
}

// TestBuildJSONTreeView_Markers verifies that container nodes pick up
// the "+ " prefix while collapsed and "- " when expanded, and that
// leaves get the two-space alignment prefix. Asserts the marker
// updates in lockstep with expansion-state changes.
func TestBuildJSONTreeView_Markers(t *testing.T) {
	v := map[string]any{
		"name": "hello",
		"meta": map[string]any{"k": "v"},
	}
	tv := buildJSONTreeView("input", v, "full")
	root := tv.GetRoot()

	var name, meta *tview.TreeNode
	for _, c := range root.GetChildren() {
		switch {
		case strings.Contains(c.GetText(), `"name"`):
			name = c
		case strings.Contains(c.GetText(), `"meta"`):
			meta = c
		}
	}
	if name == nil || meta == nil {
		t.Fatal("expected name + meta children")
	}

	// Leaf: 2-space prefix, no +/-.
	if !strings.HasPrefix(name.GetText(), "  [blue::b]") {
		t.Errorf("leaf should start with 2-space prefix, got %q", name.GetText())
	}
	// Container collapsed: "+ " prefix (with gray tag).
	if !strings.HasPrefix(meta.GetText(), "[gray]+[-] ") {
		t.Errorf("collapsed container should start with '+' marker, got %q", meta.GetText())
	}
	// After expand: "- " prefix.
	meta.Expand()
	refreshAllNodeTexts(root, nil, scopeWhole)
	if !strings.HasPrefix(meta.GetText(), "[gray]-[-] ") {
		t.Errorf("expanded container should start with '-' marker, got %q", meta.GetText())
	}
	// After collapse: back to "+".
	meta.Collapse()
	refreshAllNodeTexts(root, nil, scopeWhole)
	if !strings.HasPrefix(meta.GetText(), "[gray]+[-] ") {
		t.Errorf("after collapse, '+' marker should return, got %q", meta.GetText())
	}
}

// TestBuildJSONTreeView_SingleScalarArrayInlined makes sure a single-
// scalar array doesn't get a useless [0] child — the value is on the
// parent's label and the parent has no children to expand into.
func TestBuildJSONTreeView_SingleScalarArrayInlined(t *testing.T) {
	v := map[string]any{
		"__stripe_mid": []any{"3be92203-f6cd-4a5a-b06f"},
		"items":        []any{"a", "b"},
	}
	tv := buildJSONTreeView("input", v, "full")
	root := tv.GetRoot()

	var stripe, items *tview.TreeNode
	for _, c := range root.GetChildren() {
		switch {
		case strings.Contains(c.GetText(), "__stripe_mid"):
			stripe = c
		case strings.Contains(c.GetText(), `"items"`):
			items = c
		}
	}
	if stripe == nil || items == nil {
		t.Fatal("expected to find __stripe_mid and items nodes")
	}

	// __stripe_mid: single-scalar array → inlined, no children.
	if len(stripe.GetChildren()) != 0 {
		t.Errorf("single-scalar array should have 0 children, got %d",
			len(stripe.GetChildren()))
	}
	if !strings.Contains(stripe.GetText(), `3be92203`) {
		t.Errorf("expected single value inlined in node text, got %q",
			stripe.GetText())
	}

	// items: 2-element array → keeps its [0]/[1] children.
	if len(items.GetChildren()) != 2 {
		t.Errorf("2-item array should have 2 children, got %d",
			len(items.GetChildren()))
	}
}

// TestComputeCopyText verifies what `c` puts on the clipboard for
// each scope. The user's intent (from the request that introduced
// this): single-scalar arrays unwrap so the copied value matches the
// inlined display.
func TestComputeCopyText(t *testing.T) {
	mkLeaf := func(key string, value any, arrayIdx bool) *tview.TreeNode {
		n := tview.NewTreeNode("")
		n.SetReference(&jsonNodeData{key: key, value: value, arrayIdx: arrayIdx})
		return n
	}

	cases := []struct {
		name  string
		node  *tview.TreeNode
		scope selectScope
		want  string
	}{
		// Single-scalar array (the case from the user's example).
		{
			"whole_single_scalar_array",
			mkLeaf("_cyan_cohort", []any{"202619"}, false),
			scopeWhole,
			`"_cyan_cohort": "202619"`,
		},
		{
			"key_single_scalar_array",
			mkLeaf("_cyan_cohort", []any{"202619"}, false),
			scopeKey,
			"_cyan_cohort",
		},
		{
			"value_single_scalar_array",
			mkLeaf("_cyan_cohort", []any{"202619"}, false),
			scopeValue,
			`"202619"`,
		},
		// Regular scalar leaf.
		{
			"whole_string_leaf",
			mkLeaf("name", "hello", false),
			scopeWhole,
			`"name": "hello"`,
		},
		{
			"key_string_leaf",
			mkLeaf("name", "hello", false),
			scopeKey,
			"name",
		},
		{
			"value_string_leaf",
			mkLeaf("name", "hello", false),
			scopeValue,
			`"hello"`,
		},
		// Number / bool / null values.
		{
			"value_number",
			mkLeaf("count", float64(42), false),
			scopeValue,
			"42",
		},
		{
			"value_bool",
			mkLeaf("ok", true, false),
			scopeValue,
			"true",
		},
		{
			"value_null",
			mkLeaf("none", nil, false),
			scopeValue,
			"null",
		},
		// Container — value copies the whole JSON subtree.
		{
			"value_container_object",
			mkLeaf("meta", map[string]any{"a": float64(1)}, false),
			scopeValue,
			`{"a":1}`,
		},
		// Array element (arrayIdx=true) — whole copies bare value.
		{
			"whole_array_element_drops_index",
			mkLeaf("0", "hello", true),
			scopeWhole,
			`"hello"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := computeCopyText(c.node, c.scope)
			if !ok {
				t.Fatalf("computeCopyText returned !ok")
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRenderJSONNodeLabel_ScopeDimming verifies the scope-aware
// re-render: whole keeps all colors, key dims the value side, value
// dims the key side. The dim wrap (`[::d]…[::-]`) is what makes the
// non-scoped half visually fade so the user sees what `c` will copy.
func TestRenderJSONNodeLabel_ScopeDimming(t *testing.T) {
	nd := &jsonNodeData{key: "name", value: "hello"}

	whole := renderJSONNodeLabel(nd, false, false, scopeWhole)
	if strings.Contains(whole, "[::d]") {
		t.Errorf("whole scope shouldn't dim anything, got %q", whole)
	}
	if !strings.Contains(whole, `[blue::b]"name"`) {
		t.Errorf("whole scope should style the key, got %q", whole)
	}
	if !strings.Contains(whole, `[green]"hello"`) {
		t.Errorf("whole scope should style the value, got %q", whole)
	}

	keyScope := renderJSONNodeLabel(nd, false, false, scopeKey)
	if !strings.Contains(keyScope, `[blue::b]"name"`) {
		t.Errorf("key scope must keep the key styled, got %q", keyScope)
	}
	if !strings.Contains(keyScope, "[::d]") || !strings.Contains(keyScope, "[::-]") {
		t.Errorf("key scope must wrap value side in [::d]…[::-], got %q", keyScope)
	}
	// The dimmed half should have no color tags (only the plain text).
	if strings.Contains(keyScope, `[green]"hello"`) {
		t.Errorf("key scope must NOT keep green on the value, got %q", keyScope)
	}

	valScope := renderJSONNodeLabel(nd, false, false, scopeValue)
	if !strings.Contains(valScope, `[green]"hello"`) {
		t.Errorf("value scope must keep the value styled, got %q", valScope)
	}
	if !strings.Contains(valScope, "[::d]") {
		t.Errorf("value scope must dim the key side, got %q", valScope)
	}
	if strings.Contains(valScope, `[blue::b]"name"`) {
		t.Errorf("value scope must NOT keep blue on the key, got %q", valScope)
	}
}

// TestJSONEncodeAll confirms whole-document copy doesn't apply the
// single-scalar-array unwrap (which would turn a top-level ["x"] into
// "x" — the unwrap is meaningful for a nested *value*, not the whole
// document).
func TestJSONEncodeAll(t *testing.T) {
	got := jsonEncodeAll([]any{"x"})
	if !strings.Contains(got, "[") || !strings.Contains(got, "]") {
		t.Errorf("jsonEncodeAll should preserve top-level array, got %q", got)
	}
	got = jsonEncodeAll(map[string]any{"k": "v"})
	if !strings.Contains(got, `"k"`) || !strings.Contains(got, `"v"`) {
		t.Errorf("jsonEncodeAll should emit object with key+value, got %q", got)
	}
}

// TestBuildJSONTreeView_NilShowsPlaceholder covers the "no payload"
// case — when step.In or step.Out is nil (summary trace mode), the
// view shows a single dim placeholder child instead of an empty tree.
func TestBuildJSONTreeView_NilShowsPlaceholder(t *testing.T) {
	tv := buildJSONTreeView("input", nil, "summary")
	root := tv.GetRoot()
	children := root.GetChildren()
	if len(children) != 1 {
		t.Fatalf("expected 1 placeholder child, got %d", len(children))
	}
	text := children[0].GetText()
	if !strings.Contains(text, "no input payload") || !strings.Contains(text, "summary") {
		t.Errorf("placeholder text missing expected substrings: %q", text)
	}
}
