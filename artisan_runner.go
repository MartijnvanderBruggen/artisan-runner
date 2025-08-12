package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
)

type AppConfig struct {
	LastSelections []int  `json:"last_selections"`
	SavedAt        string `json:"saved_at"`
}

type Task struct {
	Label string
	Cmd   []string // e.g. []{"php", "artisan", "optimize:clear"}
}

var tasks = []Task{
	{Label: "php artisan optimize:clear", Cmd: []string{"php", "artisan", "optimize:clear"}},
	{Label: "php artisan config:clear", Cmd: []string{"php", "artisan", "config:clear"}},
	{Label: "php artisan route:clear", Cmd: []string{"php", "artisan", "route:clear"}},
	{Label: "php artisan cache:clear", Cmd: []string{"php", "artisan", "cache:clear"}},
}

func main() {
	// Flags
	projectPath := flag.String("path", ".", "Path to the Laravel project (where artisan lives)")
	useLast := flag.Bool("use-last", false, "Run the last selections without prompting")
	numbers := flag.String("numbers", "", "Comma-separated indices to run (1-based). Use 0 for all. Example: --numbers 1,3")
	noColor := flag.Bool("no-color", false, "Disable colored output")
	noSave := flag.Bool("no-save", false, "Do not remember this selection")
	flag.Parse()

	c := newColorizer(!*noColor)

	// Resolve project path
	absPath, err := filepath.Abs(*projectPath)
	if err != nil {
		fail(c, fmt.Errorf("unable to resolve path: %w", err))
	}
	info(c, fmt.Sprintf("Project path: %s", absPath))

	// Validate artisan exists (best-effort)
	if !fileExists(filepath.Join(absPath, "artisan")) {
		warn(c, "artisan not found in the given path. If it lives elsewhere, commands may still work if PHP resolves it.")
	}

	// Determine selections
	var selectedIdxs []int

	cfgPath, _ := getConfigPath()

	switch {
	case *numbers != "":
		// numeric mode; support 0=all
		idxs, err := parseNumbers(*numbers, len(tasks))
		if err != nil {
			fail(c, err)
		}
		selectedIdxs = idxs

	case *useLast:
		// load last
		last, err := loadLastSelections(cfgPath)
		if err != nil {
			fail(c, fmt.Errorf("could not load last selections: %w", err))
		}
		if len(last) == 0 {
			fail(c, errors.New("no last selections saved"))
		}
		selectedIdxs = last

	default:
		// Interactive TUI with checkboxes (+ All option)
		opts := []string{"[Run ALL]"}
		for _, t := range tasks {
			opts = append(opts, t.Label)
		}

		var picks []string
		prompt := &survey.MultiSelect{
			Message: "Select Artisan commands (space to toggle, enter to run):",
			Options: opts,
		}

		// try to preselect last
		if last, err := loadLastSelections(cfgPath); err == nil && len(last) > 0 {
			def := []string{}
			if len(last) == len(tasks) {
				def = append(def, "[Run ALL]")
			} else {
				for _, i := range last {
					if i >= 1 && i <= len(tasks) {
						def = append(def, tasks[i-1].Label)
					}
				}
			}
			prompt.Default = def
		}

		if err := survey.AskOne(prompt, &picks, survey.WithValidator(survey.Required)); err != nil {
			fail(c, err)
		}

		// translate picks
		runAll := false
		for _, p := range picks {
			if p == "[Run ALL]" {
				runAll = true
				break
			}
		}
		if runAll {
			selectedIdxs = make([]int, 0, len(tasks))
			for i := range tasks {
				selectedIdxs = append(selectedIdxs, i+1)
			}
		} else {
			for _, p := range picks {
				for i, t := range tasks {
					if t.Label == p {
						selectedIdxs = append(selectedIdxs, i+1) // 1-based
						break
					}
				}
			}
		}
	}

	if len(selectedIdxs) == 0 {
		fail(c, errors.New("no commands selected"))
	}

	// Save selection unless disabled
	if !*noSave {
		_ = saveLastSelections(cfgPath, selectedIdxs)
	}

	// Execute
	ok(c, "Executing selected commands...\n")

	for _, idx := range selectedIdxs {
		// safe-guard
		if idx < 1 || idx > len(tasks) {
			warn(c, fmt.Sprintf("Skipping invalid index: %d", idx))
			continue
		}
		task := tasks[idx-1]
		step(c, fmt.Sprintf("Running: %s", strings.Join(task.Cmd, " ")))

		cmd := exec.Command(task.Cmd[0], task.Cmd[1:]...)
		cmd.Dir = absPath

		// stream output
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			errMsg(c, fmt.Sprintf("Error running '%s': %v", task.Label, err))
		} else {
			ok(c, "Done\n")
		}
	}

	ok(c, "All selected commands executed.")
}

/* -------------------- helpers -------------------- */

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func getConfigPath() (string, error) {
	// Use OS config dir if possible, fallback to home
	if dir, err := os.UserConfigDir(); err == nil {
		path := filepath.Join(dir, "artisan-runner.json")
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".artisan-runner.json"), nil
}

func saveLastSelections(path string, idxs []int) error {
	cfg := AppConfig{
		LastSelections: idxs,
		SavedAt:        time.Now().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return os.WriteFile(path, b, 0o600)
}

func loadLastSelections(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AppConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return cfg.LastSelections, nil
}

func parseNumbers(s string, max int) ([]int, error) {
	parts := strings.Split(s, ",")
	var out []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// 0 means all
		if p == "0" {
			out = out[:0]
			for i := 1; i <= max; i++ {
				out = append(out, i)
			}
			return out, nil
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid number: %q", p)
		}
		if n < 1 || n > max {
			return nil, fmt.Errorf("choice out of range: %d", n)
		}
		out = append(out, n)
	}
	return dedupe(out), nil
}

func dedupe(in []int) []int {
	seen := map[int]struct{}{}
	var out []int
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

/* -------------------- coloring -------------------- */

type colorizer struct {
	enabled bool
}

func newColorizer(enabled bool) *colorizer { return &colorizer{enabled: enabled} }

func (c *colorizer) wrap(code string, s string) string {
	if !c.enabled {
		return s
	}
	return code + s + "\x1b[0m"
}
func (c *colorizer) green(s string) string  { return c.wrap("\x1b[32m", s) }
func (c *colorizer) yellow(s string) string { return c.wrap("\x1b[33m", s) }
func (c *colorizer) red(s string) string    { return c.wrap("\x1b[31m", s) }
func (c *colorizer) cyan(s string) string   { return c.wrap("\x1b[36m", s) }
func (c *colorizer) bold(s string) string   { return c.wrap("\x1b[1m", s) }

func info(c *colorizer, msg string)  { fmt.Println(c.cyan("ℹ "), msg) }
func warn(c *colorizer, msg string)  { fmt.Println(c.yellow("⚠ "), msg) }
func ok(c *colorizer, msg string)    { fmt.Println(c.green("✅ "), msg) }
func step(c *colorizer, msg string)  { fmt.Println(c.bold("▶ "), msg) }
func errMsg(c *colorizer, msg string){ fmt.Println(c.red("❌ "), msg) }
func fail(c *colorizer, err error) {
	errMsg(c, err.Error())
	os.Exit(1)
}

/* -------------------- Windows ANSI enable (optional) -------------------- */

// On Windows, you might need this to enable ANSI colors in legacy terminals.
// It's safe to noop on other platforms.
func init() {
	if runtime.GOOS == "windows" {
		// Best-effort: rely on modern terminals or VS Code integrated terminal.
		// For full support, consider golang.org/x/sys/windows to enable VT processing.
	}
}

/* -------------------- Numeric fallback prompt (unused, kept for extension) -------------------- */

func numericPrompt(max int) []int {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("Select by number (comma-separated, 0 for all): ")
	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(text)
	idxs, err := parseNumbers(text, max)
	if err != nil {
		fmt.Println("Error:", err)
		return nil
	}
	return idxs
}

