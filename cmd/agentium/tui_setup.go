package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/models"
	"github.com/tegarthegreat/agentium/internal/provider"
)

// turnCost is the price of a turn's usage, 0 when unknown.
func turnCost(res provider.Resolved, fb *provider.Fallback, st agent.Stats) float64 {
	info, known := res.Info, res.Known
	if fb != nil {
		act := fb.Active()
		info, known = act.Info, act.Known
	}
	if !known {
		return 0
	}
	return info.Price(st.Usage.Input, st.Usage.Output, st.Usage.CacheRead, st.Usage.CacheWrite)
}

// turnSummary is the receipt printed after each turn: a rule with the
// time, steps, tokens and cost.
func (u *ui) turnSummary(st agent.Stats, err error, cost float64) string {
	us := st.Usage
	parts := []string{fmt.Sprintf("%.1fs", st.Elapsed.Seconds())}
	if st.ToolCalls > 0 {
		parts = append(parts, fmt.Sprintf("%d step%s", st.ToolCalls, plural(st.ToolCalls)))
	}
	in := fmtK(us.Input+us.CacheRead+us.CacheWrite) + " in"
	if us.CacheRead > 0 {
		in += fmt.Sprintf(" (%s cached)", fmtK(us.CacheRead))
	}
	parts = append(parts, in, fmtK(us.Output)+" out")
	if cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f", cost))
	}
	head := u.paint(cGreen, "●")
	if err != nil {
		head = u.paint(cRed, "●")
	}
	text := " " + strings.Join(parts, " · ") + " "
	rule := max(termWidth(os.Stderr)-strWidth(text)-8, 2)
	return "\n  " + head + u.paint(cGray, " ──"+text+strings.Repeat("─", rule))
}

// banner is shown when an interactive session starts.
func (u *ui) banner(model, mode, box, cwd string) {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	rows := []string{
		u.paint(cCyan, "◆") + " " + u.paint(cBold, "Agentium") + " " + u.paint(cDim, version),
		"",
		u.paint(cDim, "model   ") + model,
		u.paint(cDim, "mode    ") + mode + u.paint(cDim, " · "+box),
		u.paint(cDim, "folder  ") + cwd,
	}
	w := 0
	for _, r := range rows {
		if n := strWidth(r); n > w {
			w = n
		}
	}
	if max := termWidth(os.Stderr) - 4; w > max {
		w = max
	}
	var sb strings.Builder
	sb.WriteString(u.paint(cGray, "╭"+strings.Repeat("─", w+2)+"╮") + "\n")
	for _, r := range rows {
		pad := w - strWidth(r)
		if pad < 0 {
			pad = 0
		}
		sb.WriteString(u.paint(cGray, "│") + " " + r + strings.Repeat(" ", pad) + " " + u.paint(cGray, "│") + "\n")
	}
	sb.WriteString(u.paint(cGray, "╰"+strings.Repeat("─", w+2)+"╯") + "\n")
	hint, max := "", termWidth(os.Stderr)-3
	for _, h := range []string{"/help commands", "/model switch model", "/login add a provider", "Ctrl-C interrupt"} {
		next := h
		if hint != "" {
			next = hint + " · " + h
		}
		if strWidth(next) > max {
			break
		}
		hint = next
	}
	sb.WriteString(u.paint(cDim, "  "+hint) + "\n")
	os.Stderr.WriteString(sb.String())
}

// prompt is the input prompt for the current mode.
func (u *ui) prompt(plan bool) string {
	if plan {
		return u.paint(cMagenta, "plan ❯") + " "
	}
	return u.paint(cCyan, "❯") + " "
}

var helpRows = [][2]string{
	{"/model", "choose a model (or /model provider/model)"},
	{"/login  /logout", "add, change or remove a provider's API key"},
	{"/mode", "approvals: ask · auto · yolo · plan (shift+tab cycles)"},
	{"/effort", "reasoning effort: low · medium · high · xhigh · max"},
	{"/plan  /go", "investigate read-only, then carry out the plan"},
	{"/undo  /rewind", "revert the last turn, or back to an earlier one (esc esc)"},
	{"/copy", "copy the last reply to the clipboard"},
	{"/sessions  /resume", "list and continue saved conversations"},
	{"/clear", "start a new conversation"},
	{"/skills", "list skills; /<skill> [task] runs one"},
	{"/usage  /config", "tokens used · current settings"},
	{"/update", "install the latest release"},
	{"/exit", "quit (also ctrl+d)"},
}

var keyRows = [][2]string{
	{"enter", "send · while a turn runs: queue it for after"},
	{"ctrl+j  shift+enter", "new line (or end a line with \\)"},
	{"/  @", "commands · mention a file (tab or enter picks)"},
	{"esc", "stop the running turn · esc esc: rewind"},
	{"shift+tab", "next approval mode"},
	{"ctrl+r", "search your earlier messages"},
	{"ctrl+g", "write the message in $EDITOR"},
	{"ctrl+o", "full output of recent steps"},
	{"↑ ↓", "history · ctrl+a/e/u/k/w edit the line"},
	{"@file.png", "attach an image"},
}

func (u *ui) help() {
	var sb strings.Builder
	row := func(k, v string) {
		sb.WriteString("  " + u.paint(cAccent, fmt.Sprintf("%-22s", k)) + u.paint(cDim, v) + "\n")
	}
	sb.WriteString("\n" + u.paint(cBold, "Commands") + "\n")
	for _, r := range helpRows {
		row(r[0], r[1])
	}
	sb.WriteString("\n" + u.paint(cBold, "Keys") + "\n")
	for _, r := range keyRows {
		row(r[0], r[1])
	}
	if activeFS() != nil {
		row("pgup pgdn  wheel", "scroll · shift+drag selects text")
		row("ctrl+t", "show or hide the side panel")
		row("agentium --classic", "the inline interface instead")
	}
	os.Stderr.WriteString(sb.String())
}

func (u *ui) note(s string) { fmt.Fprintln(os.Stderr, u.paint(cDim, "  "+s)) }

// success prints a green check line.
func (u *ui) success(s string) { fmt.Fprintln(os.Stderr, u.paint(cGreen, "✓")+" "+s) }

// failure prints a red error line.
func (u *ui) failure(s string) { fmt.Fprintln(os.Stderr, u.paint(cRed, "✗")+" "+s) }

// readKey reads one key press in raw mode.
func readKey() (string, error) {
	restore, err := makeRaw(os.Stdin)
	if err != nil {
		return "", err
	}
	defer restore()
	return (&editor{in: os.Stdin}).key()
}

// menuItem is one choice in a menu.
type menuItem struct {
	value, label, hint string
}

var errCanceled = errors.New("canceled")

// choose shows an arrow-key menu and returns the chosen item's value.
// Typing filters the list; with allowCustom, Enter on a filter that
// matches nothing returns the typed text.
func (u *ui) choose(title string, items []menuItem, current string, allowCustom bool) (string, error) {
	if !isTTY(os.Stdin) || !isTTY(os.Stderr) {
		return "", errors.New("not a terminal")
	}
	restore, err := makeRaw(os.Stdin)
	if err != nil {
		return "", err
	}
	defer restore()
	ed := &editor{in: os.Stdin}
	out := os.Stderr
	out.WriteString("\033[?25l") // hide cursor
	width := termWidth(out) - 2
	fmt.Fprint(out, "\r\n"+u.paint(cBold, truncate(title, width))+"\r\n")
	filter, sel, top, drawn := "", 0, 0, 0
	defer func() {
		// Remove the menu once answered; the caller reports the choice.
		out.WriteString(strings.Repeat("\033[1A\033[2K", drawn+2) + "\r\033[?25h")
	}()
	for i, it := range items {
		if it.value == current {
			sel = i
		}
	}
	const rows = 10
	visible := func() []menuItem {
		if filter == "" {
			return items
		}
		var v []menuItem
		f := strings.ToLower(filter)
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.value+" "+it.label), f) {
				v = append(v, it)
			}
		}
		return v
	}
	for {
		vis := visible()
		if sel >= len(vis) {
			sel = len(vis) - 1
		}
		if sel < 0 {
			sel = 0
		}
		if sel < top {
			top = sel
		}
		if sel >= top+rows {
			top = sel - rows + 1
		}
		var sb strings.Builder
		for i := 0; i < drawn; i++ {
			sb.WriteString("\033[1A\033[2K")
		}
		sb.WriteString("\r\033[2K")
		lines := 0
		hint := "↑/↓ move · Enter select · Esc cancel · type to filter"
		if filter != "" {
			hint = "filter: " + filter
		}
		sb.WriteString(u.paint(cDim, "  "+truncate(hint, width-2)) + "\r\n")
		lines++
		for i := top; i < len(vis) && i < top+rows; i++ {
			it := vis[i]
			label := it.label
			if label == "" {
				label = it.value
			}
			label = truncate(label, width/2)
			row := "  " + label
			if it.hint != "" {
				row += "  " + u.paint(cDim, truncate(it.hint, width-strWidth(label)-6))
			}
			if i == sel {
				row = u.paint(cCyan, "❯ "+label)
				if it.hint != "" {
					row += "  " + u.paint(cDim, truncate(it.hint, width-strWidth(label)-6))
				}
			}
			sb.WriteString("\033[2K" + row + "\r\n")
			lines++
		}
		if len(vis) == 0 {
			msg := "  no match"
			if allowCustom {
				msg = truncate("  Enter to use \""+filter+"\"", width)
			}
			sb.WriteString("\033[2K" + u.paint(cDim, msg) + "\r\n")
			lines++
		} else if len(vis) > rows {
			sb.WriteString("\033[2K" + u.paint(cDim, fmt.Sprintf("  %d/%d", sel+1, len(vis))) + "\r\n")
			lines++
		}
		out.WriteString(sb.String())
		drawn = lines
		k, err := ed.key()
		if err != nil {
			return "", err
		}
		switch k {
		case "\x1b[A", "\x10", "\x1bOA":
			sel--
		case "\x1b[B", "\x0e", "\x1bOB":
			sel++
		case "\x1b[5~":
			sel -= rows
		case "\x1b[6~":
			sel += rows
		case "\r", "\n":
			if len(vis) > 0 {
				return vis[sel].value, nil
			}
			if allowCustom && filter != "" {
				return filter, nil
			}
		case "\x1b", "\x03", "\x04":
			return "", errCanceled
		case "\x7f", "\x08":
			if filter != "" {
				r := []rune(filter)
				filter = string(r[:len(r)-1])
				sel, top = 0, 0
			}
		default:
			if len(k) >= 1 && k[0] >= 0x20 && k[0] != 0x7f && !strings.HasPrefix(k, "\x1b") {
				filter += k
				sel, top = 0, 0
			}
		}
	}
}

// readSecretMasked reads a secret showing • per character.
func readSecretMasked(prompt string) (string, error) {
	restore, err := makeRaw(os.Stdin)
	if err != nil {
		return readSecret(prompt) // no raw mode: stty fallback
	}
	defer restore()
	out := os.Stderr
	out.WriteString("\x1b[?2004h")
	defer out.WriteString("\x1b[?2004l")
	ed := &editor{in: os.Stdin}
	var buf []rune
	// The line is redrawn whole, with at most one row of dots, so a long
	// key never wraps (a wrapped line cannot be erased with backspaces).
	draw := func() {
		n := len(buf)
		if max := termWidth(out) - strWidth(prompt) - 2; n > max {
			n = max
		}
		if n < 0 {
			n = 0
		}
		out.WriteString("\r\x1b[K" + prompt + strings.Repeat("•", n))
	}
	draw()
	for {
		k, err := ed.key()
		if err != nil {
			return "", err
		}
		switch k {
		case "\r", "\n":
			out.WriteString("\r\n")
			return strings.TrimSpace(string(buf)), nil
		case "\x03", "\x1b":
			out.WriteString("\r\n")
			return "", errCanceled
		case "\x7f", "\x08":
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
			}
		case "\x15":
			buf = nil
		case "\x1b[200~", "\x1b[201~":
		default:
			if !strings.HasPrefix(k, "\x1b") {
				for _, r := range k {
					if r > 0x20 && r != 0x7f {
						buf = append(buf, r)
					}
				}
			}
		}
		draw()
	}
}

// providerInfo labels the providers offered by the setup menu.
var providerInfo = []struct{ id, name, keyURL string }{
	{"anthropic", "Anthropic (Claude)", "https://console.anthropic.com/settings/keys"},
	{"openai", "OpenAI", "https://platform.openai.com/api-keys"},
	{"gemini", "Google Gemini", "https://aistudio.google.com/apikey"},
	{"deepseek", "DeepSeek", "https://platform.deepseek.com/api_keys"},
	{"openrouter", "OpenRouter (hundreds of models)", "https://openrouter.ai/keys"},
	{"xai", "xAI (Grok)", "https://console.x.ai"},
	{"groq", "Groq", "https://console.groq.com/keys"},
	{"cerebras", "Cerebras", "https://cloud.cerebras.ai"},
	{"mistral", "Mistral", "https://console.mistral.ai/api-keys"},
	{"moonshot", "Moonshot (Kimi)", "https://platform.moonshot.ai"},
	{"zai", "Z.ai (GLM)", "https://z.ai/manage-apikey/apikey-list"},
	{"together", "Together AI", "https://api.together.ai/settings/api-keys"},
	{"fireworks", "Fireworks", "https://fireworks.ai/account/api-keys"},
	{"github", "GitHub Models", "https://github.com/settings/tokens"},
	{"ollama", "Ollama (local)", ""},
	{"lmstudio", "LM Studio (local)", ""},
}

// saveKey stores a provider key in the OS keychain, else auth.json.
func saveKey(id, key string) (string, error) {
	where := "~/.agentium/auth.json"
	cred := config.Credential{APIKey: key}
	if config.KeychainAvailable() && config.KeychainSet(id, key) == nil {
		cred, where = config.Credential{Keychain: true}, "the OS keychain"
	}
	return where, config.UpdateAuth(func(a config.Auth) { a[id] = cred })
}

// setup walks through choosing a provider, entering its key and picking
// a model, saves them, and returns the "provider/model" reference. With
// pid set the provider step is skipped.
func (u *ui) setup(pid string) (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return "", err
	}
	specs := provider.Specs(cfg)
	if pid == "" {
		var items []menuItem
		for _, p := range providerInfo {
			s, ok := specs[p.id]
			if !ok {
				continue
			}
			hint := "needs an API key"
			switch {
			case s.NoKey:
				hint = "runs locally, no key"
			case provider.Key(s, auth) != "":
				hint = "✓ connected"
			}
			items = append(items, menuItem{value: p.id, label: p.name, hint: hint})
		}
		if pid, err = u.choose("Choose a provider", items, "", false); err != nil {
			return "", err
		}
	}
	s, ok := specs[pid]
	if !ok {
		return "", fmt.Errorf("unknown provider %q", pid)
	}
	name, keyURL := pid, ""
	for _, p := range providerInfo {
		if p.id == pid {
			name, keyURL = p.name, p.keyURL
		}
	}
	if !s.NoKey && s.Protocol != "bedrock" && s.Protocol != "vertex" {
		has := provider.Key(s, auth) != ""
		for attempt := 0; ; attempt++ {
			if has && attempt == 0 {
				replace, err := u.choose(name+" is already connected", []menuItem{
					{value: "keep", label: "Keep the current key"},
					{value: "new", label: "Enter a new key"},
				}, "keep", false)
				if err != nil {
					return "", err
				}
				if replace == "keep" {
					break
				}
			}
			fmt.Fprintln(os.Stderr)
			if keyURL != "" {
				u.note("Get a key at " + keyURL)
			}
			key, err := readSecretMasked(u.paint(cBold, "  "+name+" API key: "))
			if err != nil {
				return "", err
			}
			if key == "" {
				return "", errCanceled
			}
			// Check the key before storing it, so a rejected key never
			// replaces one that works. Listing models doubles as the check.
			u.note("checking the key…")
			trial := config.Auth{}
			for k, v := range auth {
				trial[k] = v
			}
			trial[pid] = config.Credential{APIKey: key}
			if _, err := provider.ListModels(context.Background(), pid, cfg, trial); err != nil &&
				(strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403")) {
				u.failure("the key was rejected (" + firstLine(err.Error()) + "); try again")
				has = false
				continue
			}
			where, err := saveKey(pid, key)
			if err != nil {
				return "", err
			}
			auth, _ = config.LoadAuth()
			u.success("Saved the " + name + " key to " + where)
			break
		}
	}
	model, err := u.pickModel(pid, cfg, auth, "")
	if err != nil {
		return "", err
	}
	ref := pid + "/" + model
	if err := config.Set("model", ref); err != nil {
		return "", err
	}
	return ref, nil
}

// pickModel lists a provider's models (live from its API, with details
// from models.dev) and returns the chosen model id.
func (u *ui) pickModel(pid string, cfg config.Config, auth config.Auth, current string) (string, error) {
	u.note("loading models…")
	live, _ := provider.ListModels(context.Background(), pid, cfg, auth)
	known := map[string]models.Model{}
	registryID := pid
	if a, ok := models.Aliases[pid]; ok {
		registryID = a
	}
	for _, m := range models.List(config.Home(), registryID) {
		known[m.ID] = m
	}
	ids := live
	if len(ids) == 0 {
		for id, m := range known {
			if m.Tools {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
	}
	def := provider.Specs(cfg)[pid].Default
	if current == "" {
		current = def
	}
	var items []menuItem
	for _, id := range ids {
		hint := ""
		if m, ok := known[id]; ok {
			if !m.Tools {
				continue // cannot call tools: useless for an agent
			}
			hint = fmt.Sprintf("%s context", fmtK(m.Context))
			if m.Cost.Input > 0 || m.Cost.Output > 0 {
				hint += fmt.Sprintf(" · $%g / $%g per M tokens", m.Cost.Input, m.Cost.Output)
			}
		}
		if id == def {
			hint = strings.TrimPrefix(hint+" · recommended", " · ")
		}
		items = append(items, menuItem{value: id, hint: hint})
	}
	// Recommended first, then newest registry entries, then the rest.
	sort.SliceStable(items, func(i, j int) bool {
		if (items[i].value == def) != (items[j].value == def) {
			return items[i].value == def
		}
		return known[items[i].value].Released > known[items[j].value].Released
	})
	if len(items) == 0 && def != "" {
		items = append(items, menuItem{value: def, hint: "recommended"})
	}
	return u.choose("Choose a model", items, current, true)
}

// showConfig prints the current settings.
func (u *ui) showConfig(model, mode, effort, box string) {
	if effort == "" {
		effort = "model default"
	}
	rows := [][2]string{
		{"model", model}, {"mode", mode}, {"effort", effort}, {"sandbox", box},
		{"config", filepath.Join(config.Home(), "config.json")},
	}
	fmt.Fprintln(os.Stderr)
	for _, r := range rows {
		fmt.Fprintln(os.Stderr, "  "+u.paint(cDim, fmt.Sprintf("%-8s", r[0]))+r[1])
	}
}

// approvalTitle turns a gate action ("bash: cmd") into a question.
func approvalTitle(action, reason string) (title, what string) {
	kind, what, ok := strings.Cut(action, ": ")
	if !ok {
		return "Allow this?", action
	}
	switch kind {
	case "bash":
		title = "Run this command?"
	case "write":
		title = "Change this file?"
	case "network":
		title = "Allow network access for this command?"
	case "read":
		title = "Read this file?"
	case "fetch":
		title = "Fetch this URL?"
	case "mcp":
		title = "Use this MCP tool?"
	default:
		title = "Allow " + kind + "?"
	}
	if reason != "" && reason != "ask mode" && !strings.HasPrefix(title, "Allow network") {
		title += "  " + reason
	}
	return title, what
}

// approve asks for permission with a single key press.
func (u *ui) approve(action, reason, scope string) (string, error) {
	title, what := approvalTitle(action, reason)
	what = u.relative(what)
	u.mu.Lock()
	u.paused = true
	u.clearLive()
	u.endLine()
	u.afterTool = false
	// The whole command is shown (wrapped), never cut: what is approved
	// must be what is seen. Very long ones show their first rows and say so.
	var body strings.Builder
	for i, row := range wrapRows(sanitize(what), termWidth(os.Stderr)-4) {
		if i == 12 {
			body.WriteString("  " + u.paint(cYellow, "… (command continues; press n and ask the agent to split it)") + "\n")
			break
		}
		body.WriteString("  " + u.paint(cCyan, row) + "\n")
	}
	fmt.Fprintf(os.Stderr, "\n%s %s\n%s  %s ",
		u.paint(cYellow, "▲"), u.paint(cBold, title), body.String(),
		u.paint(cDim, "[y] yes  [a] always "+scope+"  [n] no ›"))
	u.mu.Unlock()
	u.inOffice(func(o *office) { o.setLead(actWait, "") })
	if f := activeFS(); f != nil {
		f.setBusy(true, u.paint(cYellow, "▲")+" Waiting for your answer · y yes · a always · n no", "", nil)
	}
	asked := time.Now()
	defer func() {
		u.inOffice(func(o *office) { o.setLead(actThink, "") })
		u.mu.Lock()
		u.paused = false
		u.approvalWait += time.Since(asked)
		held := u.held
		u.held = nil
		for _, l := range held {
			fmt.Fprintln(os.Stderr, l)
		}
		u.mu.Unlock()
	}()
	last := time.Now()
	u.drainKeys()
	hinted := false
	for {
		k, err := u.nextKey()
		if err != nil {
			return "", err
		}
		if k == "\x1b" || k == "\x03" { // Esc and Ctrl-C always mean no
			fmt.Fprintln(os.Stderr, u.paint(cRed, "no"))
			return "n", nil
		}
		// An answer is a key on its own: a pause before it, and nothing
		// right after it. Keys inside a burst of typing (a message typed
		// ahead: "add tests" must not answer "always") never count.
		gap := time.Since(last)
		last = time.Now()
		ans := strings.ToLower(k)
		isAnswer := ans == "y" || ans == "a" || ans == "n" || ans == "\r" || ans == "\n"
		if gap >= 400*time.Millisecond && isAnswer {
			if next, ok := u.keyWithin(400 * time.Millisecond); !ok {
				switch ans {
				case "y":
					fmt.Fprintln(os.Stderr, u.paint(cGreen, "yes"))
					return "y", nil
				case "a":
					fmt.Fprintln(os.Stderr, u.paint(cGreen, "always"))
					return "a", nil
				default:
					fmt.Fprintln(os.Stderr, u.paint(cRed, "no"))
					return "n", nil
				}
			} else if next == "\x1b" || next == "\x03" {
				fmt.Fprintln(os.Stderr, u.paint(cRed, "no"))
				return "n", nil
			}
			last = time.Now()
		}
		if !hinted {
			hinted = true
			fmt.Fprint(os.Stderr, u.paint(cDim, "(pause, then press y, a or n) "))
		}
	}
}

// wrapRows splits s into rows at most width columns wide.
func wrapRows(s string, width int) []string {
	if width < 10 {
		width = 10
	}
	var rows []string
	var cur strings.Builder
	n := 0
	for _, r := range s {
		if r == '\n' {
			rows = append(rows, cur.String())
			cur.Reset()
			n = 0
			continue
		}
		if w := runeWidth(r); n+w > width {
			rows = append(rows, cur.String())
			cur.Reset()
			n = 0
		}
		cur.WriteRune(r)
		n += runeWidth(r)
	}
	return append(rows, cur.String())
}

// sanitize makes text safe to measure and print: tabs become spaces and
// other control characters (C0, DEL, C1) are dropped, so a command
// cannot move the cursor or hide part of itself.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, s)
}
