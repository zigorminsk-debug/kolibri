package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/workflow"
)

// systemPrompt is deliberately short: on a local model every prompt token is
// paid for in prefill time.
const systemPrompt = `You are a senior software engineer working inside an automated pipeline. A program parses your reply, commits the files to a GitHub repository and GitHub Actions builds them. Nobody edits your reply before the build, so the code must compile and work as written.

Reply in exactly this format and nothing else:

SUMMARY: <one-line commit message>

=== FILE: relative/path/name.ext ===
<complete content of the file>
=== END FILE ===

Rules:
- Send every file you create or change in full. No diffs, no placeholders, no "...".
- Do not wrap file content in Markdown code fences.
- To remove a file write a single line: === DELETE: relative/path ===
- Files you do not mention stay as they are.
- Never create files under .github/ or .kolibri/ — the build workflow and the work log are kept by the pipeline.
- Text files only. No explanations before, between or after the blocks.
- Write user-facing text, comments and the README in the language of the task.

Large tasks: you have no memory between requests. When a work log is shown, it is the history of this project — continue from it and do not redo finished work. If the whole task does not fit into one reply, do a coherent part that builds on its own and, after the last file, list what is left:

=== NEXT ===
- <a remaining step>
=== END NEXT ===

Leave the NEXT block out when the task is finished.`

var targetTitles = map[string]string{"windows": "Windows", "linux": "Linux", "macos": "macOS", workflow.Android: "Android"}

// androidRules is added when the program is also packaged for a phone.
const androidRules = "Android: CI packs the same Go + Fyne code into an APK with `fyne package`, so the code must compile with GOOS=android. " +
	"Use only pure-Go dependencies besides Fyne. Do not start other programs (os/exec), do not use the system tray, extra windows, drag and drop of files or desktop-only packages; " +
	"code that exists only for desktops goes into files with `//go:build !android`. " +
	"Keep data in fyne.CurrentApp().Storage() and Preferences() and open files through the Fyne dialogs — a phone has no home directory to write to. " +
	"Lay the window out for a narrow touch screen: one column, long content inside container.NewVScroll, every action reachable by a tap (no hover, right click or keyboard shortcut only). " +
	"Do not rely on Window.Resize or SetFixedSize — on a phone the window always fills the screen. " +
	"Do not add AndroidManifest.xml, Gradle files or an icon: CI supplies the package identifier, the signature and the icon."

func buildRules(kind workflow.Kind, targets []string) string {
	var b strings.Builder
	b.WriteString("Build rules:\n")
	android := wantsAndroid(targets) && workflow.Supports(kind, workflow.Android)
	if kind == workflow.Auto && android {
		// Only one stack can be packaged for a phone, so there is no choice.
		if info, ok := workflow.Lookup(workflow.GoFyne); ok {
			b.WriteString("The program must also run on Android, and CI can build an Android package only from this stack, so use it:\n")
			b.WriteString(info.Contract)
		}
	} else if kind == workflow.Auto {
		b.WriteString(workflow.AutoContract())
	} else if info, ok := workflow.Lookup(kind); ok {
		b.WriteString(info.Contract)
	}
	if info, ok := workflow.Lookup(kind); ok && info.PerOS && len(targets) > 0 {
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			if title, ok := targetTitles[t]; ok && workflow.Supports(kind, t) {
				names = append(names, title)
			}
		}
		if len(names) > 0 {
			b.WriteString("\nThe program is built for: " + strings.Join(names, ", ") + ".")
		}
	}
	if android {
		b.WriteString("\n" + androidRules)
	}
	return b.String()
}

func wantsAndroid(targets []string) bool {
	for _, t := range targets {
		if t == workflow.Android {
			return true
		}
	}
	return false
}

// lockFile reports generated dependency locks: the model should know they
// exist, but their content would only waste context.
func lockFile(path string) bool {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	switch base {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.lock", "poetry.lock", "uv.lock", "composer.lock":
		return true
	}
	return false
}

const maxPromptFile = 60_000

// renderFiles formats project files for a prompt within a character budget.
// Files named in first go ahead of the rest; what does not fit is returned
// in omitted so the model at least knows it exists.
func renderFiles(project map[string]string, budget int, first map[string]bool) (text string, omitted []string) {
	paths := make([]string, 0, len(project))
	for p := range project {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		if first[paths[i]] != first[paths[j]] {
			return first[paths[i]]
		}
		return paths[i] < paths[j]
	})
	var b strings.Builder
	for _, p := range paths {
		content := project[p]
		if strings.HasPrefix(p, ".github/") {
			continue // owned by the pipeline, not the model's business
		}
		block := len(content) + len(p) + 40
		if lockFile(p) || len(content) > maxPromptFile || b.Len()+block > budget {
			omitted = append(omitted, p)
			continue
		}
		fmt.Fprintf(&b, "=== FILE: %s ===\n%s", p, content)
		if !strings.HasSuffix(content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("=== END FILE ===\n\n")
	}
	sort.Strings(omitted)
	return strings.TrimRight(b.String(), "\n"), omitted
}

func projectSection(b *strings.Builder, project map[string]string, otherPaths []string, budget int, first map[string]bool) {
	files, omitted := renderFiles(project, budget, first)
	omitted = append(omitted, otherPaths...)
	sort.Strings(omitted)
	if files == "" && len(omitted) == 0 {
		b.WriteString("The repository is empty: this is a new project.\n")
		return
	}
	if files != "" {
		b.WriteString("Current files of the repository:\n\n")
		b.WriteString(files)
		b.WriteString("\n")
	}
	if len(omitted) > 0 {
		b.WriteString("\nFiles that exist but are not shown (binary, generated or too large):\n")
		const maxListed = 150
		for i, p := range omitted {
			if i == maxListed {
				fmt.Fprintf(b, "- … and %d more\n", len(omitted)-maxListed)
				break
			}
			b.WriteString("- " + p + "\n")
		}
	}
}

// promptExtra is what a request carries besides the task and the files.
type promptExtra struct {
	// history is the end of the project's work log.
	history string
	// resume tells the model that it continues a reply that was cut short.
	resume string
	// web holds Lightpanda search and page findings gathered so far.
	web string
	// useWeb includes Lightpanda browser instructions in the system prompt.
	useWeb bool
}

func systemPromptFor(useWeb bool) string {
	if useWeb {
		return systemPrompt + webPrompt
	}
	return systemPrompt
}

func (e promptExtra) write(b *strings.Builder) {
	if e.resume != "" {
		b.WriteString("\n\nContinuation: ")
		b.WriteString(e.resume)
	}
	if e.history != "" {
		b.WriteString("\n\nWork log of this project, oldest entry first. The pipeline keeps it; the newest entry is the last step taken:\n\n")
		b.WriteString(e.history)
	}
	if e.web != "" {
		b.WriteString("\n\n")
		b.WriteString(e.web)
	}
}

// taskMessages builds the first request of a run.
func taskMessages(task Task, kind workflow.Kind, project map[string]string, otherPaths []string, budget int, extra promptExtra) []colibri.Message {
	var b strings.Builder
	b.WriteString("Task:\n")
	b.WriteString(strings.TrimSpace(task.Prompt))
	b.WriteString("\n\n")
	b.WriteString(buildRules(kind, task.Targets))
	extra.write(&b)
	fmt.Fprintf(&b, "\n\nRepository: %s/%s\n", task.Owner, task.Repo)
	projectSection(&b, project, otherPaths, budget, nil)
	return []colibri.Message{
		{Role: "system", Content: systemPromptFor(extra.useWeb)},
		{Role: "user", Content: strings.TrimRight(b.String(), "\n")},
	}
}

// fixMessages builds the request sent after a failed build.
func fixMessages(task Task, kind workflow.Kind, project map[string]string, otherPaths []string, budget int, failure string, extra promptExtra) []colibri.Message {
	// Files the log mentions are the ones the model must see.
	first := map[string]bool{}
	for p := range project {
		base := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			base = p[i+1:]
		}
		if strings.Contains(failure, p) || (len(base) > 4 && strings.Contains(failure, base)) {
			first[p] = true
		}
	}
	var b strings.Builder
	b.WriteString("The GitHub Actions build of this project failed. Find the cause in the log and fix it.\n")
	b.WriteString("Reply in the same format with only the files that must change.\n\n")
	b.WriteString("Original task:\n")
	b.WriteString(clip(strings.TrimSpace(task.Prompt), 1500))
	b.WriteString("\n\n")
	b.WriteString(buildRules(kind, task.Targets))
	b.WriteString("\n\nBuild log:\n")
	b.WriteString(failure)
	extra.write(&b)
	b.WriteString("\n\n")
	projectSection(&b, project, otherPaths, budget, first)
	return []colibri.Message{
		{Role: "system", Content: systemPromptFor(extra.useWeb)},
		{Role: "user", Content: strings.TrimRight(b.String(), "\n")},
	}
}

const reformatRequest = "Your reply contained no files in the required format, so nothing could be committed. " +
	"Send the complete result again using exactly these blocks:\n\n=== FILE: relative/path ===\n<complete content>\n=== END FILE ==="

// fileBudget estimates how many characters of project files fit into the
// model's context next to the fixed parts of a prompt and the reply.
func fileBudget(contextTokens, maxTokens, fixedChars int) int {
	if contextTokens <= 0 {
		contextTokens = 16384 // colibri's smallest automatic context
	}
	reply := maxTokens
	if reply <= 0 {
		reply = contextTokens / 3
	}
	if reply > contextTokens/2 {
		reply = contextTokens / 2
	}
	// Roughly 3 characters per token for code; Cyrillic prose costs more
	// tokens per character, hence the divisor of 2 for the fixed text.
	tokens := contextTokens - reply - 600 - fixedChars/2
	budget := tokens * 3
	if budget < 0 {
		budget = 0
	}
	return budget
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
