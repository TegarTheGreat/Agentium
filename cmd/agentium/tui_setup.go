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
		u.paint(cAccent, "◆") + " " + u.paint(cBold, "Agentium") + " " + u.paint(cDim, version),
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
	for _, h := range []string{"/ commands", "@ files", "esc stop", "shift+tab mode", "? keys"} {
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

// helpRows are the commands by group; a row with an empty description
// starts a group.
var helpRows = [][2]string{
	{"Work", ""},
	{"/plan  /go", "investigate read-only, then carry out the plan"},
	{"/undo  /rewind", "revert the last turn · go back before a message: conversation, files or both (esc esc)"},
	{"/diff  /copy", "what changed · copy the last reply"},
	{"/review  /commit  /pr", "review the changes · commit them · open a pull request"},
	{"/security-review", "look for exploitable vulnerabilities in the changes"},
	{"/init", "write AGENTS.md for this repository"},
	{"/btw <question>", "a side question, not added to the conversation"},
	{"/watch", "act on comments you end with AI! (a change) or AI? (a question)"},
	{"!command", "run a shell command yourself (the agent sees it)"},
	{"Conversation", ""},
	{"/context  /compact", "what fills the context · summarize to free it"},
	{"/sessions  /resume", "list and continue saved conversations (number or name)"},
	{"/rename  /fork", "name this conversation · continue in a copy"},
	{"/handoff [goal]", "a new conversation that starts with a brief for the goal"},
	{"/clear  /export", "start a new one · save this one (.md, or .html for a page to share)"},
	{"/memory", "what I remember and where · /memory edit"},
	{"Setup", ""},
	{"/model  /effort  /mode", "model · reasoning effort · approvals (ask auto yolo plan)"},
	{"/login  /logout", "add, change or remove a provider's API key"},
	{"/permissions  /add-dir", "what runs without asking · another working folder"},
	{"/theme  /vim", "auto · dark · light · vim keys in the message box"},
	{"/style", "how I talk: default, explanatory, learning, terse"},
	{"/skills  /<name> [args]", "skills, and your commands in .agentium/commands"},
	{"/agents", "your specialist sub-agents (.agentium/agents, .claude/agents)"},
	{"/bug [what]", "open a prefilled GitHub issue for Agentium"},
	{"/release-notes [version]", "what changed in this version, or another · latest"},
	{"/doctor  /mcp  /tools", "health check · MCP servers · available tools"},
	{"/usage  /config  /update", "tokens used · settings · install the latest release"},
	{"/exit", "quit (also ctrl+d)"},
}

var keyRows = [][2]string{
	{"Typing", ""},
	{"enter  ctrl+j", "send · new line (also shift+enter, or end a line with \\)"},
	{"/  @", "commands · mention a file (@file:10-40 a range; tab or enter picks)"},
	{"tab", "on an empty message: take the suggested next message"},
	{"↑ ↓  ctrl+r", "lines of the message, then history · search history"},
	{"ctrl+k/u/w  ctrl+y", "cut to line end / start, a word · paste it back"},
	{"ctrl+_  ctrl+s", "undo · put the draft aside (again: bring it back)"},
	{"ctrl+g  ctrl+v", "write the message in $EDITOR · paste an image from the clipboard"},
	{"ctrl+z", "suspend to the shell · fg brings agentium back"},
	{"While it works", ""},
	{"enter  tab  ↑", "steer at the next step · queue for after · take back"},
	{"esc", "stop the turn · esc esc: rewind"},
	{"ctrl+o", "step outputs and thinking · t the conversation · / search · [ ] your messages"},
	{"Anytime", ""},
	{"shift+tab", "next approval mode"},
	{"alt+p  alt+t", "switch model · reasoning effort"},
	{"?", "this help"},
}

func (u *ui) help() {
	var sb strings.Builder
	width := termWidth(os.Stderr) - 1
	keyW := 0
	for _, r := range append(append([][2]string(nil), helpRows...), keyRows...) {
		if r[1] != "" {
			keyW = max(keyW, strWidth(r[0])+2)
		}
	}
	narrow := keyW > width/3 // then each description goes under its key
	row := func(k, v string) {
		if v == "" { // a group
			sb.WriteString("\n  " + u.paint(cBold, k) + "\n")
			return
		}
		if narrow {
			sb.WriteString("    " + u.paint(cAccent, k) + "\n")
			for _, l := range wordWrap(v, max(width-8, 20)) {
				sb.WriteString("      " + u.paint(cDim, l) + "\n")
			}
			return
		}
		lines := wordWrap(v, max(width-keyW-4, 20))
		for i, l := range lines {
			key := ""
			if i == 0 {
				key = k
			}
			sb.WriteString("    " + u.paint(cAccent, padTo(key, keyW)) + u.paint(cDim, l) + "\n")
		}
	}
	sb.WriteString("\n" + u.paint(cBold, "Commands"))
	for _, r := range helpRows {
		row(r[0], r[1])
	}
	sb.WriteString("\n" + u.paint(cBold, "Keys"))
	for _, r := range keyRows {
		row(r[0], r[1])
	}
	if activeFS() != nil {
		row("pgup pgdn  wheel", "scroll · shift+drag selects text")
		row("ctrl+t", "show or hide the side panel")
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
// readReply reads one line typed after an approval prompt (Enter ends
// it; Esc gives up, reported as not ok).
func (u *ui) readReply() (string, bool) {
	var r []rune
	for {
		k, err := u.nextKey()
		if err != nil {
			return string(r), false
		}
		switch {
		case k == "\r" || k == "\n":
			fmt.Fprintln(os.Stderr)
			return string(r), true
		case k == "\x1b" || k == "\x03":
			fmt.Fprintln(os.Stderr)
			return "", false
		case k == "\x7f" || k == "\x08":
			if len(r) > 0 {
				r = r[:len(r)-1]
				fmt.Fprint(os.Stderr, "\b \b")
			}
		case !strings.HasPrefix(k, "\x1b") && k[0] >= 0x20:
			r = append(r, []rune(k)...)
			fmt.Fprint(os.Stderr, k)
		}
	}
}

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
func (u *ui) approve(action, reason, scope string, keep bool) (string, error) {
	title, what := approvalTitle(action, reason)
	what = u.relative(what)
	u.mu.Lock()
	u.paused = true
	u.lastPerm = "" // the prompt and answer come between: no rewriting above them
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
	// A file change shows what would change.
	if kind, path, _ := strings.Cut(action, ": "); kind == "write" {
		// Only when exactly one pending edit targets the file: with two,
		// the preview could show the other one.
		var match []*liveTool
		for _, t := range u.tools {
			if t.call.Name != "edit" {
				continue
			}
			var m struct{ Path string }
			_ = jsonUnmarshal(t.call.Args, &m)
			p := m.Path
			if !filepath.IsAbs(p) {
				p = filepath.Join(u.cwd, p)
			}
			if filepath.Clean(p) == filepath.Clean(path) || u.relative(p) == u.relative(path) {
				match = append(match, t)
			}
		}
		switch len(match) {
		case 1:
			for _, l := range u.diffCardAt(match[0].call.Args, termWidth(os.Stderr)-1, true) {
				body.WriteString(l + "\n")
			}
		case 0:
		default:
			body.WriteString("  " + u.paint(cYellow, fmt.Sprintf("%d changes to this file are pending; each is asked for separately", len(match))) + "\n")
		}
	}
	keepOpt := " · p always here"
	if !keep {
		keepOpt = ""
	}
	fmt.Fprintf(os.Stderr, "\n%s %s\n%s  %s ",
		u.paint(cYellow, "▲"), u.paint(cBold, title), body.String(),
		u.paint(cDim, "y yes · a always "+scope+keepOpt+" · c yes + note · n no · t no + why ›"))
	u.mu.Unlock()
	u.inOffice(func(o *office) { o.setLead(actWait, "") })
	if f := activeFS(); f != nil {
		f.setBusy(true, u.paint(cYellow, "▲")+" Waiting for your answer · y yes · n no"+u.paint(cDim, " · more choices above"), "", nil)
	}
	u.setTitle("needs you")
	u.notify("Agentium needs your answer: " + title)
	asked := time.Now()
	defer func() {
		u.setTitle("working")
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
		isAnswer := ans == "y" || ans == "a" || ans == "p" || ans == "n" || ans == "t" || ans == "c" || ans == "\r" || ans == "\n"
		if gap >= 400*time.Millisecond && isAnswer {
			if next, ok := u.keyWithin(400 * time.Millisecond); !ok {
				switch ans {
				case "y":
					fmt.Fprintln(os.Stderr, u.paint(cGreen, "yes"))
					return "y", nil
				case "a":
					fmt.Fprintln(os.Stderr, u.paint(cGreen, "always"))
					return "a", nil
				case "p":
					if !keep {
						fmt.Fprintln(os.Stderr, u.paint(cGreen, "always")+u.paint(cDim, " (this session; this one is not kept)"))
						return "a", nil
					}
					fmt.Fprintln(os.Stderr, u.paint(cGreen, "always, in this project")+u.paint(cDim, " · /permissions to review"))
					return "p", nil
				case "t":
					fmt.Fprint(os.Stderr, u.paint(cRed, "no")+"\n  "+u.paint(cInk, "tell Agentium:")+" ")
					reply, _ := u.readReply()
					return "t:" + reply, nil
				case "c":
					fmt.Fprint(os.Stderr, u.paint(cGreen, "yes")+"\n  "+u.paint(cInk, "and tell Agentium:")+" ")
					reply, ok := u.readReply()
					if !ok { // Esc or Ctrl-C: no after all
						fmt.Fprintln(os.Stderr, u.paint(cRed, "  cancelled: no"))
						return "n", nil
					}
					return "c:" + reply, nil
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
