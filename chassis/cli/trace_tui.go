package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atotto/clipboard"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// selectScope tracks what `c` will copy on the focused node:
//
//	scopeWhole — `"key": <value-as-json>` (the entire row content)
//	scopeKey   — just the key name (no quotes — handy for greps)
//	scopeValue — the value as JSON (a single-scalar array unwraps to
//	             its inner scalar so what's copied matches what's shown)
//
// Reset to scopeWhole on cursor move, tab switch, or after copy.
type selectScope int

const (
	scopeWhole selectScope = iota
	scopeKey
	scopeValue
)

// runTraceTUI renders a single trace as an interactive curses-style
// view: a step list you can arrow through, Enter to drill into a
// tabbed detail view (In / Out / Meta), Esc to go back, q to quit.
//
// The caller decides when to open the TUI (see runTrace's TTY check);
// this function assumes stdout is a terminal and tcell can drive it.
// Returns nil on clean exit, or an error if the screen failed to init.
func runTraceTUI(resp *client.TraceResponse, rid string) error {
	app := tview.NewApplication()
	pages := tview.NewPages()

	openStepListPage(app, pages, resp, rid, func() {
		// Esc with no parent list to return to → quit.
		app.Stop()
	})

	// Mouse capture intentionally OFF so the terminal's native click-
	// drag text selection keeps working in the JSON view. The keyboard
	// nav (arrows, tab, enter) is the primary interface anyway.
	if err := app.SetRoot(pages, true).Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// openStepListPage installs the step-list page for `resp` into `pages`
// and switches to it. The step list's Enter handler lazily builds the
// step-detail page on demand. onBack runs when the user presses Esc on
// the step list (or 'b' / left-arrow with no cursor) — pass app.Stop
// for a single-trace session, or a "return to trace list" closure for
// the list-mode flow.
func openStepListPage(app *tview.Application, pages *tview.Pages, resp *client.TraceResponse, rid string, onBack func()) {
	const stepListPage = "step_list"
	const stepDetailPage = "step_detail"

	table := tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	fillStepsTable(table, resp.Steps)

	header := tview.NewTextView().
		SetDynamicColors(true).
		SetText(formatTraceHeader(resp, rid))

	listFooter := tview.NewTextView().
		SetDynamicColors(true).
		SetText("[::d]↑/↓: select  enter: detail  esc: back  q: quit[::-]")

	listView := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, headerHeight(resp), 0, false).
		AddItem(table, 0, 1, true).
		AddItem(listFooter, 1, 0, false)

	table.SetSelectedFunc(func(row, _ int) {
		idx := row - 1
		if idx < 0 || idx >= len(resp.Steps) {
			return
		}
		detail := buildStepDetail(resp, &resp.Steps[idx], func() {
			pages.SwitchToPage(stepListPage)
			app.SetFocus(table)
		}, app)
		pages.RemovePage(stepDetailPage)
		pages.AddPage(stepDetailPage, detail, true, true)
	})

	table.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEscape:
			onBack()
			return nil
		}
		if ev.Rune() == 'q' {
			app.Stop()
			return nil
		}
		return ev
	})

	pages.RemovePage(stepListPage)
	pages.AddPage(stepListPage, listView, true, true)
	app.SetFocus(table)
}

// runTraceListTUI shows the list of recent traces; Enter on a row
// fetches that trace and opens the standard step-list view. Esc on
// the trace list quits (or clears an active filter first). `/` opens
// a grep input, Enter applies it, Esc cancels.
//
// watchInterval > 0 starts a background ticker that polls
// ListTracesETag every interval; the server uses ETags to make the
// "unchanged" case a stat-only check, so frequent polling is cheap.
func runTraceListTUI(c *client.Client, initial *client.TraceListResponse, initialGrep string, watchInterval time.Duration) error {
	app := tview.NewApplication()
	pages := tview.NewPages()

	// State that the watch goroutine touches; everything mutated from
	// background must go through QueueUpdateDraw, but the snapshot
	// reads (currentGrep, currentETag) cross goroutines so they live
	// behind a mutex.
	var mu sync.Mutex
	current := initial
	currentGrep := initialGrep
	currentETag := ""

	const tracesPage = "traces"

	table := tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)

	header := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	footer := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	searchInput := tview.NewInputField().
		SetLabel("/").
		SetFieldWidth(0)

	// The bottom row is a tview.Pages so we can swap between the
	// status footer and the search input without rebuilding the
	// outer Flex layout.
	bottom := tview.NewPages().
		AddPage("footer", footer, true, true).
		AddPage("search", searchInput, true, false)

	refreshChrome := func() {
		watchTag := ""
		if watchInterval > 0 {
			watchTag = fmt.Sprintf("  [::d]watch:[-] [yellow]%s[-]", watchInterval)
		}
		if currentGrep == "" {
			header.SetText(fmt.Sprintf("[::b]traces[::-]  [yellow]%d[-] shown of [yellow]%d[-] total  [::d]@ %s[::-]%s",
				len(current.Traces), current.Total, tview.Escape(c.Addr()), watchTag))
			footer.SetText("[::d]↑/↓: select  enter: inspect  /: filter  r: refresh  q: quit[::-]")
		} else {
			header.SetText(fmt.Sprintf("[::b]traces[::-]  [yellow]%d[-] match (of %d shown)  [::d]filter:[-] [yellow]%s[-]  [::d]@ %s[::-]%s",
				current.Total, len(current.Traces), tview.Escape(currentGrep), tview.Escape(c.Addr()), watchTag))
			footer.SetText("[::d]↑/↓: select  enter: inspect  /: refilter  r: refresh  esc: clear filter  q: quit[::-]")
		}
	}

	fillTracesTable(table, current.Traces)
	refreshChrome()

	listView := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 1, 0, false).
		AddItem(table, 0, 1, true).
		AddItem(bottom, 1, 0, false)

	pages.AddPage(tracesPage, listView, true, true)

	openTrace := func(rid string) {
		resp, _, err := c.GetTrace(context.Background(), rid, true)
		if err != nil {
			footer.SetText("[red]fetch failed: " + tview.Escape(err.Error()) + "[-]")
			return
		}
		openStepListPage(app, pages, resp, rid, func() {
			pages.SwitchToPage(tracesPage)
			app.SetFocus(table)
		})
	}

	// rememberSelected captures the currently-highlighted rid so we
	// can restore the cursor after a refill, surviving polls that
	// reorder/insert rows. Returns "" when no row is selected.
	rememberSelected := func() string {
		row, _ := table.GetSelection()
		idx := row - 1
		if idx < 0 || idx >= len(current.Traces) {
			return ""
		}
		return current.Traces[idx].RID
	}
	// restoreSelected re-seats the cursor on a row whose rid matches
	// `rid`, or leaves the default selection if it isn't present.
	restoreSelected := func(rid string) {
		if rid == "" {
			return
		}
		for i, tr := range current.Traces {
			if tr.RID == rid {
				table.Select(i+1, 0)
				return
			}
		}
	}

	// applyFilter refetches with the given grep and updates the table.
	// Used by `/` submit, `r` refresh, and Esc-to-clear-filter. Always
	// runs synchronously on the UI goroutine.
	applyFilter := func(grep string) {
		prev := rememberSelected()
		list, etag, _, err := c.ListTracesETag(context.Background(), 0, grep, "")
		if err != nil {
			footer.SetText("[red]list failed: " + tview.Escape(err.Error()) + "[-]")
			return
		}
		mu.Lock()
		current = list
		currentGrep = grep
		currentETag = etag
		mu.Unlock()
		fillTracesTable(table, current.Traces)
		restoreSelected(prev)
		refreshChrome()
	}

	// openSearch swaps the footer for the search input, pre-filled
	// with the active filter (if any) so the user can edit rather
	// than retype.
	openSearch := func() {
		searchInput.SetText(currentGrep)
		bottom.SwitchToPage("search")
		app.SetFocus(searchInput)
	}
	closeSearch := func() {
		bottom.SwitchToPage("footer")
		app.SetFocus(table)
	}

	searchInput.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			applyFilter(strings.TrimSpace(searchInput.GetText()))
			closeSearch()
		case tcell.KeyEscape:
			closeSearch()
		}
	})

	table.SetSelectedFunc(func(row, _ int) {
		idx := row - 1
		if idx < 0 || idx >= len(current.Traces) {
			return
		}
		openTrace(current.Traces[idx].RID)
	})

	table.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			// Esc clears an active filter first; quits when none.
			if currentGrep != "" {
				applyFilter("")
				return nil
			}
			app.Stop()
			return nil
		}
		switch ev.Rune() {
		case 'q':
			app.Stop()
			return nil
		case 'r':
			applyFilter(currentGrep)
			return nil
		case '/':
			openSearch()
			return nil
		}
		return ev
	})

	// Background watch loop. Polls every watchInterval with the last
	// ETag; the server uses on-disk stats to return 304 cheaply when
	// nothing changed. On 200 we refresh the table via QueueUpdateDraw
	// (UI mutations must run on tview's goroutine). The loop exits
	// when ctx is cancelled — tied to app.Stop via the deferred cancel.
	if watchInterval > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			t := time.NewTicker(watchInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					mu.Lock()
					etag, grep := currentETag, currentGrep
					mu.Unlock()

					list, newEtag, notModified, err := c.ListTracesETag(ctx, 0, grep, etag)
					if err != nil || notModified {
						// Silently skip errors during watch — a hiccup
						// shouldn't take over the screen. The user will
						// notice if it's persistent.
						continue
					}
					app.QueueUpdateDraw(func() {
						mu.Lock()
						// Drop the update if the user changed grep while
						// our request was in flight — they'll get the
						// right data via applyFilter.
						if currentGrep != grep {
							mu.Unlock()
							return
						}
						current = list
						currentETag = newEtag
						mu.Unlock()
						prev := rememberSelected()
						fillTracesTable(table, current.Traces)
						restoreSelected(prev)
						refreshChrome()
					})
				}
			}
		}()
	}

	if err := app.SetRoot(pages, true).Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// fillTracesTable populates the trace-list table: rid, src, started,
// duration, status. Newest is row 1 (server returns newest-first).
func fillTracesTable(table *tview.Table, traces []client.TraceSummary) {
	table.Clear()
	headers := []string{"rid", "src", "stack", "started", "dur", "status"}
	for i, h := range headers {
		table.SetCell(0, i,
			tview.NewTableCell(h).
				SetTextColor(tcell.ColorYellow).
				SetSelectable(false).
				SetAttributes(tcell.AttrBold))
	}
	for i, t := range traces {
		row := i + 1
		table.SetCell(row, 0, tview.NewTableCell(t.RID))
		table.SetCell(row, 1, tview.NewTableCell(truncate(t.Src, 30)))
		table.SetCell(row, 2, tview.NewTableCell(truncate(routeOrStack(t.Route, t.Stack), 30)))
		table.SetCell(row, 3, tview.NewTableCell(t.StartedAt))
		dur := "--"
		if t.DurationMs != nil {
			dur = fmt.Sprintf("%dms", *t.DurationMs)
		}
		table.SetCell(row, 4, tview.NewTableCell(dur).SetAlign(tview.AlignRight))
		table.SetCell(row, 5, tview.NewTableCell(t.Status).
			SetTextColor(statusColor(t.Status)))
	}
	if len(traces) > 0 {
		table.Select(1, 0)
	}
}

// fillStepsTable lays out the step list with the same column shape as
// the plain renderer: step, name, operation, status, dur, in→out.
func fillStepsTable(table *tview.Table, steps []client.TraceStep) {
	headers := []string{"step", "name", "operation", "status", "dur", "in→out"}
	for i, h := range headers {
		table.SetCell(0, i,
			tview.NewTableCell(h).
				SetTextColor(tcell.ColorYellow).
				SetSelectable(false).
				SetAttributes(tcell.AttrBold))
	}
	for i, s := range steps {
		row := i + 1
		table.SetCell(row, 0, tview.NewTableCell(stepLabel(s)))
		table.SetCell(row, 1, tview.NewTableCell(s.Name))
		table.SetCell(row, 2, tview.NewTableCell(truncate(s.Operation, 50)))
		table.SetCell(row, 3, tview.NewTableCell(s.Status).
			SetTextColor(statusColor(s.Status)))
		table.SetCell(row, 4, tview.NewTableCell(fmt.Sprintf("%dms", s.DurationMs)).
			SetAlign(tview.AlignRight))
		table.SetCell(row, 5, tview.NewTableCell(fmt.Sprintf("%s→%s",
			humanBytes(s.InputBytes), humanBytes(s.OutputBytes))))
	}
	if len(steps) > 0 {
		table.Select(1, 0)
	}
}

// buildStepDetail returns a Flex layout with a top tab bar + content
// area for the chosen step. Pass back a `back` callback the detail
// view calls on Esc; the app reference is needed for SetFocus during
// tab switching.
//
// Tab order: Meta first (the default landing tab — that's the dense
// per-step info), then In, then Out. In/Out are TreeViews that start
// collapsed at the top level — Enter toggles the global expand
// state; ←/→ collapses/expands one node at a time; Space pages
// forward. Meta is a plain TextView.
func buildStepDetail(resp *client.TraceResponse, step *client.TraceStep, back func(), app *tview.Application) tview.Primitive {
	type tabEntry struct {
		label     string
		primitive tview.Primitive
		treeView  *tview.TreeView // non-nil for JSON tabs
	}

	metaView := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	metaView.SetText(formatStepMeta(step))

	inTree := buildJSONTreeView("input", step.In, resp.TraceMode)
	outTree := buildJSONTreeView("output", step.Out, resp.TraceMode)

	// Per-JSON-tab state. cursorVisible is per-tree (each tab independently
	// remembers whether the user has "engaged" with it), but the rawValue
	// holds the original parsed JSON so `c` in the no-cursor state can
	// copy the whole document.
	type jsonTabState struct {
		tv            *tview.TreeView
		rawValue      any
		cursorVisible bool
	}
	jsonTabs := []*jsonTabState{
		{tv: inTree, rawValue: step.In},
		{tv: outTree, rawValue: step.Out},
	}
	stateFor := func(tv *tview.TreeView) *jsonTabState {
		for _, s := range jsonTabs {
			if s.tv == tv {
				return s
			}
		}
		return nil
	}

	tabs := []tabEntry{
		{label: "Meta", primitive: metaView},
		{label: "In", primitive: inTree, treeView: inTree},
		{label: "Out", primitive: outTree, treeView: outTree},
	}

	pages := tview.NewPages()
	for i, t := range tabs {
		pages.AddPage(t.label, t.primitive, true, i == 0)
	}

	active := 0
	scope := scopeWhole
	// flashMsg, when non-empty, is shown in place of the normal footer
	// hint on the next renderChrome() call. Reset to "" after the next
	// user action so the hint comes back. Used for "✓ copied" feedback.
	flashMsg := ""

	tabBar := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	footer := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)

	renderChrome := func() {
		var b strings.Builder
		for i, t := range tabs {
			if i == active {
				fmt.Fprintf(&b, " [black:white] %s [white:-] ", t.label)
			} else {
				fmt.Fprintf(&b, "  [::d]%s[::-]  ", t.label)
			}
		}
		tabBar.SetText(b.String())

		if flashMsg != "" {
			footer.SetText(flashMsg)
			return
		}
		if curTV := tabs[active].treeView; curTV != nil {
			st := stateFor(curTV)
			switch {
			case !st.cursorVisible:
				footer.SetText("[::d]↓: select row  ←→: switch tab  enter: toggle all  space: page  c: copy entire JSON  esc: back  q: quit[::-]")
			case scope == scopeKey:
				footer.SetText("[::d]selection: [yellow]KEY[-:-:-][::d]  c: copy  ←/→: line  tab: switch  esc: back  q: quit[::-]")
			case scope == scopeValue:
				footer.SetText("[::d]selection: [yellow]VALUE[-:-:-][::d]  c: copy  ←/→: line  tab: switch  esc: back  q: quit[::-]")
			default:
				footer.SetText("[::d]↑↓: move  ←→: collapse/expand or refine  enter: toggle all  space: page  c: copy  tab: switch  esc: back  q: quit[::-]")
			}
		} else {
			footer.SetText("[::d]↑↓: scroll  ←→/tab: switch  c: copy meta  esc: back  q: quit[::-]")
		}
	}
	renderChrome()

	title := tview.NewTextView().
		SetDynamicColors(true).
		SetText(fmt.Sprintf("[::b]trace %s[::-]  step [yellow]%s[-]",
			resp.RID, stepLabel(*step)))

	switchTab := func(delta int) {
		active = (active + delta + len(tabs)) % len(tabs)
		scope = scopeWhole
		flashMsg = ""
		// Reset every JSON tab to the no-cursor state so re-entering
		// any of them is a fresh experience.
		for _, s := range jsonTabs {
			s.cursorVisible = false
			s.tv.SetCurrentNode(nil)
			refreshAllNodeTexts(s.tv.GetRoot(), nil, scopeWhole)
		}
		pages.SwitchToPage(tabs[active].label)
		renderChrome()
		app.SetFocus(tabs[active].primitive)
	}

	// Per-widget input capture: Tab/Shift+Tab cycle tabs, Esc returns
	// to the list, q quits. We deliberately do NOT bind ←/→ here —
	// the JSON TreeView needs those for collapse/expand. The capture
	// is per-widget rather than at the Flex root so a focused widget
	// gets first crack at the key (tview routes captures top-down).
	commonCapture := func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyTab:
			switchTab(+1)
			return nil
		case tcell.KeyBacktab:
			switchTab(-1)
			return nil
		case tcell.KeyEscape:
			back()
			return nil
		}
		if ev.Rune() == 'q' {
			app.Stop()
			return nil
		}
		return ev
	}

	// JSON tabs: every key the user expects to "do tree things" has to
	// be bound explicitly because this version of tview's TreeView
	// treats KeyLeft/KeyRight as movement (same as KeyUp/KeyDown),
	// not as collapse/expand. So we intercept:
	//   Left  → collapse current node
	//   Right → expand current node
	//   Enter → toggle global (all expanded ↔ top-level only)
	//   Space → page down
	// Up/Down/Home/End/PgDn/PgUp/j/k/g/G/J/K stay on tview's defaults.
	// resetScopeOnMove: any cursor move (Up/Down/Home/End/PgDn/PgUp/j/k/g/G
	// in tview's default TreeView bindings) lands here via SetChangedFunc.
	// We snap scope back to Whole so the user doesn't accidentally copy
	// just the key from the wrong row, and refresh the tree so the old
	// row's dim-styling clears.
	resetScopeOnMove := func(node *tview.TreeNode) {
		changed := false
		if scope != scopeWhole {
			scope = scopeWhole
			changed = true
		}
		if flashMsg != "" {
			flashMsg = ""
			changed = true
		}
		if changed {
			// Find which tree this is by checking which TreeView's
			// current matches; refresh that one.
			for _, s := range jsonTabs {
				if s.tv.GetCurrentNode() == node {
					refreshAllNodeTexts(s.tv.GetRoot(), node, scopeWhole)
					break
				}
			}
			renderChrome()
		}
	}

	// seatCursor flips a no-cursor tab into "cursor visible" mode and
	// puts the cursor on the first selectable row.
	seatCursor := func(st *jsonTabState) {
		if first := firstSelectable(st.tv.GetRoot()); first != nil {
			st.tv.SetCurrentNode(first)
		}
		st.cursorVisible = true
	}

	// copyText is the common "write to clipboard + flash footer" path.
	copyText := func(text, label string) {
		if err := clipboard.WriteAll(text); err != nil {
			flashMsg = "[red]copy failed: " + tview.Escape(err.Error()) + "[-]"
		} else {
			flashMsg = "[green]✓ copied " + label + "[-]  [::d](" + tview.Escape(truncate(text, 60)) + ")[::-]"
		}
	}

	// Meta tab capture: ←/→ cycle tabs (Meta has no tree to expand
	// and no horizontal text to scroll, so arrows are free), c copies
	// the meta block. Up/Down still scroll the TextView (commonCapture
	// passes those through).
	metaView.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyLeft:
			switchTab(-1)
			return nil
		case tcell.KeyRight:
			switchTab(+1)
			return nil
		}
		if ev.Rune() == 'c' {
			copyText(formatStepMetaPlain(step), "meta")
			renderChrome()
			return nil
		}
		return commonCapture(ev)
	})

	for _, t := range []*tview.TreeView{inTree, outTree} {
		tv := t
		tv.SetChangedFunc(resetScopeOnMove)
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			st := stateFor(tv)

			// No-cursor state: vertical movement seats the cursor, ←/→
			// cycle tabs (since they have no collapse/expand or
			// scope-refine meaning when nothing is selected), `c`
			// copies the whole JSON document.
			if !st.cursorVisible {
				switch ev.Key() {
				case tcell.KeyDown, tcell.KeyUp, tcell.KeyPgDn, tcell.KeyPgUp,
					tcell.KeyHome, tcell.KeyEnd:
					seatCursor(st)
					refreshAllNodeTexts(tv.GetRoot(), tv.GetCurrentNode(), scope)
					renderChrome()
					return nil
				case tcell.KeyLeft:
					switchTab(-1)
					return nil
				case tcell.KeyRight:
					switchTab(+1)
					return nil
				case tcell.KeyEnter:
					toggleJSONExpansion(tv, scope, st.cursorVisible)
					return nil
				}
				if ev.Key() == tcell.KeyRune {
					switch ev.Rune() {
					case 'j', 'k', 'g', 'G':
						seatCursor(st)
						refreshAllNodeTexts(tv.GetRoot(), tv.GetCurrentNode(), scope)
						renderChrome()
						return nil
					case ' ':
						return tcell.NewEventKey(tcell.KeyPgDn, 0, tcell.ModNone)
					case 'c':
						copyText(jsonEncodeAll(st.rawValue), "entire JSON")
						renderChrome()
						return nil
					}
				}
				return commonCapture(ev)
			}

			// Cursor-visible state: existing behavior.
			switch ev.Key() {
			case tcell.KeyLeft:
				cur := tv.GetCurrentNode()
				if cur == nil {
					return nil
				}
				if len(cur.GetChildren()) > 0 {
					cur.Collapse()
					refreshAllNodeTexts(tv.GetRoot(), tv.GetCurrentNode(), scope)
				} else {
					switch scope {
					case scopeWhole:
						scope = scopeKey
					default:
						scope = scopeWhole
					}
					flashMsg = ""
					refreshAllNodeTexts(tv.GetRoot(), tv.GetCurrentNode(), scope)
					renderChrome()
				}
				return nil
			case tcell.KeyRight:
				cur := tv.GetCurrentNode()
				if cur == nil {
					return nil
				}
				if len(cur.GetChildren()) > 0 {
					cur.Expand()
					refreshAllNodeTexts(tv.GetRoot(), tv.GetCurrentNode(), scope)
				} else {
					switch scope {
					case scopeWhole:
						scope = scopeValue
					default:
						scope = scopeWhole
					}
					flashMsg = ""
					refreshAllNodeTexts(tv.GetRoot(), tv.GetCurrentNode(), scope)
					renderChrome()
				}
				return nil
			case tcell.KeyEnter:
				toggleJSONExpansion(tv, scope, st.cursorVisible)
				return nil
			}
			if ev.Key() == tcell.KeyRune {
				switch ev.Rune() {
				case ' ':
					return tcell.NewEventKey(tcell.KeyPgDn, 0, tcell.ModNone)
				case 'c':
					if cur := tv.GetCurrentNode(); cur != nil {
						if text, ok := computeCopyText(cur, scope); ok {
							copyText(text, scopeLabel(scope))
							scope = scopeWhole
							refreshAllNodeTexts(tv.GetRoot(), tv.GetCurrentNode(), scope)
							renderChrome()
						}
					}
					return nil
				}
			}
			return commonCapture(ev)
		})
	}

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(title, 1, 0, false).
		AddItem(tabBar, 1, 0, false).
		AddItem(pages, 0, 1, true).
		AddItem(footer, 1, 0, false)

	app.SetFocus(metaView)
	return root
}

// jsonNodeData lives on each TreeNode's reference. We re-render the
// node's visible text from this whenever expansion state changes —
// tview doesn't fire a callback on collapse/expand, so we just refresh
// every node after our key handlers act.
type jsonNodeData struct {
	key      string
	value    any
	arrayIdx bool
}

// buildJSONTreeView returns a tview.TreeView whose hidden root holds
// children built from v (which is the parsed JSON returned by the
// trace endpoint — a map[string]any, []any, scalar, or nil).
//
// Initial state is "top-level only": the root's direct children are
// visible, but any nested container they own is collapsed.
func buildJSONTreeView(label string, v any, traceMode string) *tview.TreeView {
	root := tview.NewTreeNode("")

	if v == nil {
		msg := fmt.Sprintf("(no %s payload — trace_mode=%s)", label, traceModeOrUnset(traceMode))
		placeholder := tview.NewTreeNode("[::d]" + tview.Escape(msg) + "[::-]").
			SetSelectable(false)
		root.AddChild(placeholder)
	} else {
		addJSONChildren(root, v)
		// Top-level-only: collapse every direct child (and their
		// descendants); the children themselves stay visible because
		// the root is expanded (and hidden via SetTopLevel below).
		for _, c := range root.GetChildren() {
			c.CollapseAll()
		}
		refreshAllNodeTexts(root, nil, scopeWhole)
	}

	// Start with NO cursor. The caller flips on cursorVisible the first
	// time the user presses a movement key (Down/Up/PgDn/etc.) and seats
	// the cursor at firstSelectable(root) at that moment. Before then,
	// `c` copies the whole JSON document rather than a single row.
	return tview.NewTreeView().
		SetRoot(root).
		SetCurrentNode(nil).
		SetTopLevel(1)
}

// addJSONChildren walks v and adds one TreeNode per direct child.
// Objects → one node per key (sorted, deterministic). Arrays → one
// node per element with a "[i]" index marker, EXCEPT for single-
// scalar arrays which jsonNodeText already inlines on the parent
// (no point in a [0] indirection when there's exactly one value).
// Scalars at this level have no children to add.
//
// Text is set AFTER children are added so the +/- marker can read the
// final has-children state.
func addJSONChildren(parent *tview.TreeNode, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := tview.NewTreeNode("")
			child.SetReference(&jsonNodeData{key: k, value: x[k]})
			addJSONChildren(child, x[k])
			applyNodeText(child, nil, scopeWhole)
			parent.AddChild(child)
		}
	case []any:
		if len(x) == 1 && isScalarJSON(x[0]) {
			// Inlined by jsonNodeText — skip the lone [0] child.
			return
		}
		for i, elem := range x {
			child := tview.NewTreeNode("")
			child.SetReference(&jsonNodeData{key: strconv.Itoa(i), value: elem, arrayIdx: true})
			addJSONChildren(child, elem)
			applyNodeText(child, nil, scopeWhole)
			parent.AddChild(child)
		}
	default:
		// Scalar at the root — render as a single leaf so the user
		// sees something rather than an empty tree.
		if parent.GetText() == "" && parent.GetReference() == nil && len(parent.GetChildren()) == 0 {
			leaf := tview.NewTreeNode(renderJSONValue(v))
			parent.AddChild(leaf)
		}
	}
}

// applyNodeText rebuilds a node's visible text from its reference,
// reflecting current expansion state and (for the cursor's node) the
// selection scope. Pass current=nil + scope=scopeWhole for a plain
// re-render (no scoped emphasis anywhere — used during build).
func applyNodeText(n, current *tview.TreeNode, scope selectScope) {
	nd, ok := n.GetReference().(*jsonNodeData)
	if !ok {
		return
	}
	s := scopeWhole
	if n == current {
		s = scope
	}
	n.SetText(renderJSONNodeLabel(nd, len(n.GetChildren()) > 0, n.IsExpanded(), s))
}

// refreshAllNodeTexts walks parent's subtree and reapplies node texts.
// Called after any action that mutates expansion state or scope.
// The current/scope pair is forwarded so the cursor's row picks up
// scoped dimming while the rest stays at whole-row colors.
func refreshAllNodeTexts(parent, current *tview.TreeNode, scope selectScope) {
	for _, c := range parent.GetChildren() {
		applyNodeText(c, current, scope)
		refreshAllNodeTexts(c, current, scope)
	}
}

// refreshTreeForCursor is the convenience wrapper input handlers use:
// derive `current` from the TreeView's current selection.
func refreshTreeForCursor(tv *tview.TreeView, scope selectScope) {
	refreshAllNodeTexts(tv.GetRoot(), tv.GetCurrentNode(), scope)
}

// renderJSONNodeLabel produces "<marker> <key>: <value>" for one node.
// For non-whole scopes, the un-selected side is wrapped in [::d]…[::-]
// so the eye is drawn to the part `c` will copy. The marker is "+" for
// an expandable-but-collapsed container, "-" for an expanded one, or
// two spaces for a leaf.
func renderJSONNodeLabel(nd *jsonNodeData, hasChildren, expanded bool, scope selectScope) string {
	marker := nodeMarker(hasChildren, expanded)
	kStyled, kPlain, sep, vStyled, vPlain := jsonNodeParts(nd)
	switch scope {
	case scopeKey:
		return marker + kStyled + "[::d]" + sep + tview.Escape(vPlain) + "[::-]"
	case scopeValue:
		return marker + "[::d]" + tview.Escape(kPlain+sep) + "[::-]" + vStyled
	default:
		return marker + kStyled + sep + vStyled
	}
}

// nodeMarker returns the leading "+ " / "- " / "  " indicator that
// tells the user whether a row can be expanded.
func nodeMarker(hasChildren, expanded bool) string {
	switch {
	case hasChildren && expanded:
		return "[gray]-[-] "
	case hasChildren:
		return "[gray]+[-] "
	default:
		return "  "
	}
}

// jsonNodeText renders the "<key>: <value>" portion of a node — no
// marker. Equivalent to renderJSONNodeLabel with scopeWhole and no
// marker. Retained as a thin wrapper because tests (and any future
// non-TUI render path) want the styled key+value as one string.
func jsonNodeText(key string, v any, arrayIdx bool) string {
	kStyled, _, sep, vStyled, _ := jsonNodeParts(&jsonNodeData{
		key: key, value: v, arrayIdx: arrayIdx,
	})
	return kStyled + sep + vStyled
}

// jsonNodeParts decomposes a node into:
//
//	keyStyled — key with tview color tags (used in whole/key scope)
//	keyPlain  — key without tags (used inside [::d]…[::-] when dimmed)
//	sep       — separator: ": " for object props, " " for array items
//	valStyled — value (or container-marker) with color tags
//	valPlain  — same value/marker, no tags
//
// Splitting the styled and plain forms lets the scope-aware renderer
// swap one for the other without trying to strip color tags from text.
func jsonNodeParts(nd *jsonNodeData) (keyStyled, keyPlain, sep, valStyled, valPlain string) {
	if nd.arrayIdx {
		keyStyled = fmt.Sprintf("[gray]\\[%s][-]", nd.key)
		keyPlain = "[" + nd.key + "]"
		sep = " "
	} else {
		keyStyled = fmt.Sprintf(`[blue::b]"%s"[-:-:-]`, nd.key)
		keyPlain = `"` + nd.key + `"`
		sep = ": "
	}
	switch x := nd.value.(type) {
	case map[string]any:
		if len(x) == 0 {
			valStyled, valPlain = "{}", "{}"
		} else {
			valStyled, valPlain = "{", "{"
		}
	case []any:
		switch {
		case len(x) == 0:
			valStyled, valPlain = "[]", "[]"
		case len(x) == 1 && isScalarJSON(x[0]):
			valStyled = "[ " + renderJSONValue(x[0]) + " ]"
			valPlain = "[ " + renderJSONValuePlain(x[0]) + " ]"
		default:
			valStyled, valPlain = "[", "["
		}
	default:
		valStyled = renderJSONValue(nd.value)
		valPlain = renderJSONValuePlain(nd.value)
	}
	return
}

// renderJSONValuePlain is renderJSONValue without color tags. Used
// when emitting the dimmed half of a scoped row — we can't have color
// tags inside a [::d]…[::-] block because the inner tags would reset
// the dim attribute partway through.
func renderJSONValuePlain(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return `"` + x + `"`
	}
	return fmt.Sprintf("%v", v)
}

// isScalarJSON reports whether v is a non-container JSON value
// (string, number, bool, null). Used to decide when an array can be
// inlined onto its parent without losing structure information.
func isScalarJSON(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	}
	return true
}

// renderJSONValue formats a scalar JSON value with jq-style colors.
// Container values shouldn't reach here — they're handled at
// jsonNodeText's switch.
func renderJSONValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "[gray]null[-]"
	case bool:
		if x {
			return "[yellow]true[-]"
		}
		return "[yellow]false[-]"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return `[green]"` + tview.Escape(x) + `"[-]`
	default:
		return tview.Escape(fmt.Sprintf("%v", v))
	}
}

// toggleJSONExpansion flips a TreeView between "fully expanded" and
// "top-level only". Bound to Enter in the input handler. When called
// while everything is already open, collapses every child recursively
// (the root's direct children remain visible because the root itself
// stays expanded and hidden via SetTopLevel). Refreshes node texts
// so +/- markers reflect the new state.
//
// If the cursor was on a node that got hidden by the collapse,
// re-seats it on the first visible row. Pass cursorVisible=false to
// keep the no-cursor state — Enter doesn't reveal a cursor.
func toggleJSONExpansion(tv *tview.TreeView, scope selectScope, cursorVisible bool) {
	root := tv.GetRoot()
	if root == nil {
		return
	}
	if allDescendantsExpanded(root) {
		for _, c := range root.GetChildren() {
			c.CollapseAll()
		}
		if cursorVisible {
			tv.SetCurrentNode(firstSelectable(root))
		}
	} else {
		root.ExpandAll()
	}
	refreshAllNodeTexts(root, tv.GetCurrentNode(), scope)
}

// allDescendantsExpanded returns true if every node under parent that
// has children is currently expanded. The parent itself is not
// inspected — only its subtree.
func allDescendantsExpanded(parent *tview.TreeNode) bool {
	for _, c := range parent.GetChildren() {
		if len(c.GetChildren()) > 0 {
			if !c.IsExpanded() {
				return false
			}
			if !allDescendantsExpanded(c) {
				return false
			}
		}
	}
	return true
}

// firstSelectable returns the first child of root (or root itself if
// it has none). Used to seat the current-node cursor when the tree is
// built or after a collapse-all repositions focus.
func firstSelectable(root *tview.TreeNode) *tview.TreeNode {
	for _, c := range root.GetChildren() {
		return c
	}
	return root
}

// computeCopyText decides what to put on the clipboard given the node
// and selection scope. Returns "" + false when the node has no
// reference data (root, placeholder, or scalar-root leaf — nothing
// useful to copy).
//
//	scopeKey    → just the key name (no quotes). For array elements,
//	              the bracketed index like "[0]" (least useful — most
//	              users won't end up here on array items).
//	scopeValue  → the value as JSON. Single-scalar arrays unwrap to
//	              the inner scalar so what's copied matches what was
//	              displayed inline.
//	scopeWhole  → `"<key>": <value-json>` — drop the key into another
//	              JSON document verbatim. For array items, just the
//	              JSON value (the index isn't meaningful as a key).
func computeCopyText(n *tview.TreeNode, scope selectScope) (string, bool) {
	nd, ok := n.GetReference().(*jsonNodeData)
	if !ok {
		return "", false
	}
	switch scope {
	case scopeKey:
		if nd.arrayIdx {
			return "[" + nd.key + "]", true
		}
		return nd.key, true
	case scopeValue:
		return jsonValueString(nd.value), true
	default:
		val := jsonValueString(nd.value)
		if nd.arrayIdx {
			// "0": "foo"  is just confusing — copy the bare value
			// for array elements in whole-line scope.
			return val, true
		}
		keyJSON, _ := json.Marshal(nd.key)
		return string(keyJSON) + ": " + val, true
	}
}

// jsonValueString returns v as compact JSON, with one ergonomic
// special-case: a single-scalar array unwraps to its inner scalar so
// the copied form matches the inlined display (`"_cyan_cohort": ["x"]`
// → copying the value yields `"x"`, not `["x"]`).
func jsonValueString(v any) string {
	if arr, ok := v.([]any); ok && len(arr) == 1 && isScalarJSON(arr[0]) {
		v = arr[0]
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// jsonEncodeAll returns v as indented JSON, no inlining hacks. Used by
// the "copy whole document" path so a top-level array like ["x"]
// doesn't accidentally unwrap to "x".
func jsonEncodeAll(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func scopeLabel(s selectScope) string {
	switch s {
	case scopeKey:
		return "key"
	case scopeValue:
		return "value"
	}
	return "line"
}

// formatTraceHeader returns the same summary block the plain renderer
// prints at the top of the trace, with tview color tags.
func formatTraceHeader(r *client.TraceResponse, rid string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[::b]trace %s[::-]\n", rid)
	if r.Src != "" {
		fmt.Fprintf(&b, "  src      %s\n", r.Src)
	}
	if r.Tenant != "" {
		fmt.Fprintf(&b, "  tenant   %s\n", r.Tenant)
	}
	if s := routeOrStack(r.Route, r.Stack); s != "" {
		fmt.Fprintf(&b, "  stack    %s\n", s)
	}
	if r.StartedAt != "" {
		fmt.Fprintf(&b, "  started  %s\n", r.StartedAt)
	}
	dur := "--"
	if r.DurationMs != nil {
		dur = fmt.Sprintf("%dms", *r.DurationMs)
	}
	fmt.Fprintf(&b, "  status   [%s]%-12s[-] duration %s\n",
		statusColorName(r.Status), r.Status, dur)
	if r.Error != "" {
		fmt.Fprintf(&b, "  reason   %s\n", tview.Escape(r.Error))
	}
	if r.BytesIn > 0 || r.BytesOut > 0 {
		fmt.Fprintf(&b, "  bytes    %s → %s\n", humanBytes(r.BytesIn), humanBytes(r.BytesOut))
	}
	if r.Fuel > 0 {
		fmt.Fprintf(&b, "  fuel     %d\n", r.Fuel)
	}
	return b.String()
}

// headerHeight returns the row count of the header block — needed for
// the Flex sizing so the table gets the rest. Count newlines + 1.
func headerHeight(r *client.TraceResponse) int {
	return strings.Count(formatTraceHeader(r, r.RID), "\n")
}

// formatStepMetaPlain returns the same fields as formatStepMeta but
// with no tview color tags — used by the `c` copy path so what lands
// on the clipboard is plain text ready to paste anywhere.
func formatStepMetaPlain(s *client.TraceStep) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  name        %s\n", s.Name)
	if s.Operation != "" {
		fmt.Fprintf(&b, "  operation   %s\n", s.Operation)
	}
	if s.Transport != "" {
		fmt.Fprintf(&b, "  transport   %s\n", s.Transport)
	}
	if s.Stack != "" {
		fmt.Fprintf(&b, "  stack/scope %s/%d\n", s.Stack, s.Scope)
	}
	if s.StartedAt != "" {
		fmt.Fprintf(&b, "  started     %s\n", s.StartedAt)
	}
	if s.FinishedAt != "" {
		fmt.Fprintf(&b, "  finished    %s\n", s.FinishedAt)
	}
	fmt.Fprintf(&b, "  status      %-12s duration %dms\n", s.Status, s.DurationMs)
	fmt.Fprintf(&b, "  in→out      %s → %s\n",
		humanBytes(s.InputBytes), humanBytes(s.OutputBytes))
	if s.Error != "" {
		fmt.Fprintf(&b, "  error       %s\n", s.Error)
	}
	return b.String()
}

// formatStepMeta returns a human-readable rendering of the structured
// fields on a step. Mirrors what the plain --step renderer prints.
func formatStepMeta(s *client.TraceStep) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  name        %s\n", s.Name)
	if s.Operation != "" {
		fmt.Fprintf(&b, "  operation   %s\n", s.Operation)
	}
	if s.Transport != "" {
		fmt.Fprintf(&b, "  transport   %s\n", s.Transport)
	}
	if s.Stack != "" {
		fmt.Fprintf(&b, "  stack/scope %s/%d\n", s.Stack, s.Scope)
	}
	if s.StartedAt != "" {
		fmt.Fprintf(&b, "  started     %s\n", s.StartedAt)
	}
	if s.FinishedAt != "" {
		fmt.Fprintf(&b, "  finished    %s\n", s.FinishedAt)
	}
	fmt.Fprintf(&b, "  status      [%s]%-12s[-] duration %dms\n",
		statusColorName(s.Status), s.Status, s.DurationMs)
	fmt.Fprintf(&b, "  in→out      %s → %s\n",
		humanBytes(s.InputBytes), humanBytes(s.OutputBytes))
	if s.Error != "" {
		fmt.Fprintf(&b, "  [red]error       %s[-]\n", s.Error)
	}
	return b.String()
}

func statusColor(s string) tcell.Color {
	switch s {
	case "ok":
		return tcell.ColorGreen
	case "error", "timeout":
		return tcell.ColorRed
	case "pending", "in-flight":
		return tcell.ColorYellow
	}
	return tcell.ColorDefault
}

func statusColorName(s string) string {
	switch s {
	case "ok":
		return "green"
	case "error", "timeout":
		return "red"
	case "pending", "in-flight":
		return "yellow"
	}
	return "white"
}
