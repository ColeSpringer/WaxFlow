package testutil

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"unicode"
	"unicode/utf8"
)

// goCmd is one `go test`, `go vet` or `go run` read from command text: a
// Makefile recipe, a workflow run: step, a script or a go:generate directive.
type goCmd struct {
	where        string // file:line
	target       string // the Makefile target whose recipe holds it
	verb         string // "test", "vet" or "run"
	run          string // the -run pattern
	hasRun       bool
	race         bool
	tags         []string
	goos, goarch string // "" is the host's
	dirs         []pkgDir
}

// pkgDir is a package argument as a directory under the repository root. A
// tree is a `/...` argument: every package below dir.
type pkgDir struct {
	dir  string
	tree bool
}

func (d pkgDir) String() string {
	switch {
	case d.tree:
		return "./" + path.Join(d.dir, "...")
	case d.dir == ".":
		return "."
	}
	return "./" + d.dir
}

// holds reports whether a command over d reaches the package in dir. A tree
// stops at a nested module, as ./... does.
func (d pkgDir) holds(fsys fs.FS, dir string) bool {
	if !d.tree {
		return d.dir == dir
	}
	for p := dir; p != d.dir; p = path.Dir(p) {
		if _, err := fs.Stat(fsys, path.Join(p, "go.mod")); err == nil || p == "." {
			return false
		}
	}
	return true
}

// readerFile is where a refusal sends a maintainer.
const readerFile = "internal/testutil/runpatterns_test.go"

// refuse is the error for command text the reader does not model. It names
// both ways forward: reshape the line, or extend the reader.
func refuse(file string, line int, text, why string) error {
	return fmt.Errorf("%s:%d: %s\n\t%s\n\tEvery -run of a go test must be checked against the tests that exist. Write this one as a plain `go test -run <pattern> ./<dir>` command line, or teach the reader in %s to take this shape.",
		file, line, why, strings.TrimSpace(text), readerFile)
}

// runWord finds a -run flag in raw text, inside a quoted command too.
var runWord = regexp.MustCompile("(?:^|[\\s='\"(`])--?run(?:[\\s=]|$)")

// shellWords splits a command line the way a shell would, far enough to find
// the go commands in it: into simple commands at each unquoted ; & or |, and
// those into words. A comment and a redirection with its target are dropped.
func shellWords(s string) ([][]string, error) {
	var (
		cmds   = [][]string{nil}
		cur    strings.Builder
		inWord bool
		drop   bool // the next word is a redirection's target
		depth  int
		quote  byte
	)
	flush := func() {
		if inWord && !drop {
			cmds[len(cmds)-1] = append(cmds[len(cmds)-1], cur.String())
		}
		drop = drop && !inWord
		cur.Reset()
		inWord = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			switch {
			case c == quote:
				quote = 0
			case c == '\\' && quote == '"' && i+1 < len(s) && strings.IndexByte(`$"\`, s[i+1]) >= 0:
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == '$' && i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '{'):
			depth++
			cur.WriteString(s[i : i+2])
			i++
			inWord = true
		case depth > 0:
			if c == ')' || c == '}' {
				depth--
			}
			cur.WriteByte(c)
		case c == ' ' || c == '\t':
			flush()
		case c == '#' && !inWord:
			return cmds, nil
		case c == '>' || c == '<':
			// A leading file descriptor (2>) belongs to the redirection.
			if strings.Trim(cur.String(), "0123456789") != "" {
				flush()
			}
			cur.Reset()
			inWord = false
			for i+1 < len(s) && strings.IndexByte("<>&", s[i+1]) >= 0 {
				i++
			}
			drop = true
		case c == ';' || c == '&' || c == '|':
			flush()
			drop = false
			cmds = append(cmds, nil)
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 || depth != 0 {
		return nil, errors.New("an unbalanced quote or $( group")
	}
	flush()
	return cmds, nil
}

// cmdLine is one logical line of command text: physical lines n to last of
// its file, joined where a backslash continued them.
type cmdLine struct {
	n, last int
	text    string
}

// joinLines numbers lines from first and joins each that ends in a backslash
// to the next.
func joinLines(lines []string, first int) []cmdLine {
	var out []cmdLine
	for i := 0; i < len(lines); i++ {
		l := cmdLine{n: first + i, text: lines[i]}
		for strings.HasSuffix(l.text, `\`) && i+1 < len(lines) {
			i++
			l.text = strings.TrimSuffix(l.text, `\`) + " " + lines[i]
		}
		l.last = first + i
		out = append(out, l)
	}
	return out
}

var (
	envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	goSetting = regexp.MustCompile(`^(?:GOOS|GOARCH|GOFLAGS)(?:=|$)`)
	plainDir  = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./-]*$`) // a cd the reader can follow
)

// Flags of go test, go vet and go run by whether the next word is their
// value. A flag in neither list is refused, since its value would read as a
// package.
var (
	boolFlags = []string{"v", "race", "short", "update", "json", "failfast", "cover", "benchmem",
		"c", "x", "a", "n", "trimpath", "work", "msan", "asan"}
	valueFlags = []string{"run", "tags", "timeout", "bench", "benchtime", "count", "skip", "fuzz",
		"fuzztime", "fuzzminimizetime", "cpu", "parallel", "p", "o", "C", "shuffle", "coverprofile",
		"covermode", "coverpkg", "vet", "list", "exec", "mod", "modfile", "ldflags", "gcflags",
		"cpuprofile", "memprofile", "overlay"}
)

// readArgs fills the command from the words after `go test`, `go vet` or
// `go run`. A leg whose pattern is `^$` names no test, so nothing else about
// it has to be readable.
func (c *goCmd) readArgs(cwd string, args []string) error {
	var bad error
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if c.verb == "run" {
				break // the program and its own arguments
			}
			if strings.Contains(a, "$") {
				bad = cmp.Or(bad, fmt.Errorf("the word %s is a make or shell variable, and what it holds is not read: a flag or a package in it would go unseen, so write its value out", a))
				continue
			}
			if a != "." && !strings.HasPrefix(a, "./") {
				bad = cmp.Or(bad, fmt.Errorf("package argument %q is not a relative directory such as ./server", a))
				continue
			}
			rest, tree := strings.CutSuffix(a, "...")
			c.dirs = append(c.dirs, pkgDir{dir: path.Join(cwd, rest), tree: tree})
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name == "args" {
			break
		}
		if !hasVal {
			if slices.Contains(boolFlags, name) {
				c.race = c.race || name == "race"
				continue
			}
			if !slices.Contains(valueFlags, name) || i+1 == len(args) {
				bad = cmp.Or(bad, fmt.Errorf("flag %s is in neither boolFlags nor valueFlags: add it to boolFlags if it takes no value, to valueFlags if the next word is its value", a))
				continue
			}
			i++
			val = args[i]
		}
		switch name {
		case "run":
			c.run, c.hasRun = val, true
		case "tags":
			c.tags = append(c.tags, strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ' ' })...)
		case "C":
			cwd = path.Join(cwd, val)
		}
	}
	if bad != nil && (!c.hasRun || c.run != "^$") {
		return bad
	}
	if len(c.dirs) == 0 {
		c.dirs = []pkgDir{{dir: path.Join(cwd, ".")}}
	}
	return nil
}

// shell reads the command lines of one script: a recipe line, a run: step, a
// shell script or a directive. cd carries from line to line. A leg's platform
// and tags come from its own command line and goos alone, so anything else
// that could change them makes the shell unsure.
type shell struct {
	file    string
	read    map[int]bool // the file's lines this reader has accounted for
	target  string
	cwd     string // relative to the repository root
	goos    string // the runner's; "" is the host's
	foreign string // the step's shell when it is not sh: only go commands are read
	unsure  string // why a leg from here on cannot be placed
	sub     *subshell
}

// subshell is a subshell open over the lines being read: what the line that
// closes it gives back.
type subshell struct{ cwd, unsure string }

const (
	inSubshell  = "the leg sits in a subshell ( ... ), which the reader does not follow, so its directory cannot be placed: write `cd <dir> && go test ...` without the parentheses, or pass go test -C <dir>"
	subUnpaired = "a subshell above this leg holds parentheses the reader cannot pair, so it cannot tell where the subshell ends or where this leg runs: close the subshell with a `)` on a line of its own and keep other parentheses out of it"
)

// run reads the go commands of one line. Every -run in a simple command
// outside an echo is that command's leg, or the line is refused.
func (s *shell) run(l cmdLine) ([]goCmd, error) {
	for n := l.n; n <= l.last; n++ {
		s.read[n] = true
	}
	simples, err := shellWords(l.text)
	if err != nil {
		if runWord.MatchString(l.text) {
			return nil, refuse(s.file, l.n, l.text, "a -run on a line with "+err.Error())
		}
		return nil, nil // a multi-line string or a here-document
	}
	// A subshell open from a line above ends at a line that starts with its
	// `)`, which gives back the directory and the doubt from before it. Any
	// other parenthesis in it could be that `)`, so the reader stops pairing.
	before, open := subshell{s.cwd, s.unsure}, s.sub
	switch {
	case open == nil:
	case len(simples[0]) > 0 && simples[0][0] == ")":
		s.cwd, s.unsure, s.sub = open.cwd, open.unsure, nil
	case strings.ContainsAny(l.text, "()"):
		defer func() { s.unsure, s.sub = subUnpaired, nil }()
	}
	var out []goCmd
	// sub is why a leg of this line cannot be placed: it sits in a subshell.
	sub, closed := "", false
	for _, simple := range simples {
		has := func(match func(string) bool) bool { return slices.ContainsFunc(simple, match) }
		if len(simple) > 0 && strings.HasPrefix(simple[0], "(") {
			sub = inSubshell
		}
		closed = closed || sub != "" && has(func(w string) bool { return strings.HasSuffix(w, ")") })
		runs := 0
		for _, w := range simple {
			runs += len(runWord.FindAllString(w, -1))
		}
		sets := has(goSetting.MatchString)

		c := goCmd{target: s.target, goos: s.goos}
		unsure := ""
		for len(simple) > 0 && (simple[0] == "env" || envAssign.MatchString(simple[0])) {
			switch k, v, _ := strings.Cut(simple[0], "="); k {
			case "GOOS":
				c.goos = v
			case "GOARCH":
				c.goarch = v
			case "GOFLAGS":
				unsure = "GOFLAGS is set ahead of the command, where the reader does not look: pass the flag on the command itself"
			}
			simple = simple[1:]
		}
		isWord := func(i int, names ...string) bool {
			return i < len(simple) && slices.Contains(names, simple[i])
		}
		isGo := isWord(0, "go") && isWord(1, "test", "vet", "run")
		legs := 0
		switch {
		case sets && (len(simple) == 0 || isWord(0, "export")):
			s.unsure = "GOOS, GOARCH or GOFLAGS is set on a line of its own ahead of this leg: set it on the leg's own command line, as in `GOOS=windows go test ...`"
		case s.foreign != "" && len(simple) > 0 && !isGo:
			s.unsure = "a command the reader cannot follow comes before this leg in a " + s.foreign + " step, and the reader models sh alone: give the step `shell: bash`"
		case isWord(0, "echo", "printf") && !has(func(w string) bool { return strings.Contains(w, "$(") || strings.Contains(w, "`") }):
			runs = 0 // printed, not run
		case isWord(0, "cd") && len(simple) == 2 && plainDir.MatchString(simple[1]):
			s.cwd = path.Join(s.cwd, simple[1])
		case has(func(w string) bool { return w == "cd" || w == "pushd" || w == "popd" }):
			s.unsure = "a directory change the reader cannot follow comes before this leg, so its packages cannot be placed"
		case isGo:
			c.verb, c.where = simple[1], fmt.Sprintf("%s:%d", s.file, l.n)
			if sub != "" && runs > 0 {
				return nil, refuse(s.file, l.n, l.text, sub)
			}
			if err := c.readArgs(s.cwd, simple[2:]); err != nil {
				if runs > 0 {
					return nil, refuse(s.file, l.n, l.text, err.Error())
				}
				break
			}
			if why := cmp.Or(unsure, s.unsure); why != "" && c.hasRun && c.run != "^$" {
				return nil, refuse(s.file, l.n, l.text, why)
			}
			if c.verb == "run" {
				runs = 0 // a -run after go run is the program's own flag
			} else if c.hasRun {
				legs = 1
			}
			out = append(out, c)
		}
		if runs != legs {
			return nil, refuse(s.file, l.n, l.text, cmp.Or(sub, "a -run here is not read as a go test leg: a wrapper, a variable or a quoted command holds it. If this text is only printed, print it with echo or printf"))
		}
	}
	if sub != "" && !closed {
		// It stays open on the lines below.
		if s.sub == nil {
			s.sub = &before
		}
		s.unsure = sub
	}
	return out, nil
}

func (s *shell) runAll(lines []cmdLine) ([]goCmd, error) {
	var out []goCmd
	for _, l := range lines {
		cmds, err := s.run(l)
		if err != nil {
			return nil, err
		}
		out = append(out, cmds...)
	}
	return out, nil
}

// uncommented drops an unquoted # comment from a line.
func uncommented(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

var goTest = regexp.MustCompile(`\bgo\s+test\b`)

// unreadRuns is the net under the readers: a -run on a line of the file that
// none of them reached is a failure, never a leg left unchecked. A file with
// no reader passes read as nil, and there only a go test with a -run counts.
func unreadRuns(file, src string, read map[int]bool) error {
	for i, l := range strings.Split(src, "\n") {
		l = uncommented(l)
		switch {
		case !runWord.MatchString(l) || read[i+1]:
		case read != nil:
			return refuse(file, i+1, l, "a -run outside the command text the reader takes from this file")
		case goTest.MatchString(l):
			return refuse(file, i+1, l, "a go test with a -run in a file the reader takes no commands from: move the leg to the Makefile, a workflow run: step or a scripts/*.sh file")
		}
	}
	return nil
}

var (
	makeRule    = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*:([^=]|$)`)
	makeSkipped = regexp.MustCompile(`^\s*(?:#|$|(?:ifeq|ifneq|ifdef|ifndef|else|endif)\b|\$\((?:info|warning|error)\s)`)
	makeInclude = regexp.MustCompile(`^[-s]?include\s+(.*)$`)
)

// makeCmds reads the go commands of a makefile's recipes and names the
// makefiles it includes. Every tab line is a recipe line whatever heads its
// rule; the target is kept only where the header is one plain name.
func makeCmds(name, src string) (cmds []goCmd, includes []string, err error) {
	read := map[int]bool{}
	target := ""
	for _, l := range joinLines(strings.Split(src, "\n"), 1) {
		recipe, isRecipe := strings.CutPrefix(l.text, "\t")
		if !isRecipe {
			for n := l.n; n <= l.last; n++ {
				read[n] = true
			}
			text := uncommented(l.text)
			if makeSkipped.MatchString(l.text) {
				continue // a comment, a conditional or a message leaves the recipe open
			}
			if m := makeInclude.FindStringSubmatch(text); m != nil {
				if strings.ContainsAny(m[1], "$*?") {
					return nil, nil, refuse(name, l.n, l.text, "an include the reader cannot follow, so a leg in it would go unread: name the file")
				}
				includes = append(includes, strings.Fields(m[1])...)
				continue
			}
			target = ""
			if m := makeRule.FindStringSubmatch(text); m != nil {
				target = m[1]
			}
			// `target: ; command` holds a recipe line on the header. With
			// an = before the semicolon it is a target's variable instead.
			header, inline, found := strings.Cut(text, ";")
			isInline := found && strings.Contains(header, ":") && !strings.Contains(header, "=")
			if !isInline {
				header = text
			}
			if runWord.MatchString(header) {
				return nil, nil, refuse(name, l.n, l.text, "a -run outside a recipe: a command held in a make variable is not read, so write the go test in the recipe itself")
			}
			if !isInline {
				continue
			}
			recipe = inline
		}
		// Each recipe line is its own shell, and make's $$ is the shell's $.
		sh := shell{file: name, read: read, target: target}
		l.text = strings.ReplaceAll(strings.TrimLeft(strings.TrimSpace(recipe), "@-+"), "$$", "$")
		got, err := sh.run(l)
		if err != nil {
			return nil, nil, err
		}
		cmds = append(cmds, got...)
	}
	return cmds, includes, unreadRuns(name, src, read)
}

// scriptCmds reads the go commands of a shell script, taken to run from the
// repository root.
func scriptCmds(name, src string) ([]goCmd, error) {
	sh := shell{file: name, read: map[int]bool{}}
	cmds, err := sh.runAll(joinLines(strings.Split(src, "\n"), 1))
	if err != nil {
		return nil, err
	}
	return cmds, unreadRuns(name, src, sh.read)
}

// yamlKey is one `key: value` line of a YAML file with the keys above it.
type yamlKey struct {
	path  []string // the enclosing keys, then this one
	item  bool     // the line opens a list item
	val   string
	line  int      // 1-based
	block []string // the lines of a | or > block under it
	loose string   // its value and the lines after it that are no key lines
}

var (
	yamlKeyLine = regexp.MustCompile(`^( *)((?:- )*)([A-Za-z0-9_.-]+):(?:\s+(.*?))?\s*$`)
	yamlBlock   = regexp.MustCompile(`^[|>][+-]?[0-9]?\s*(?:#.*)?$`)
	goKey       = regexp.MustCompile(`(?:^|[\s{,"'])(GOOS|GOARCH|GOFLAGS)["']?\s*:`)
)

// yamlKeys lists the key lines of src, nesting them by indentation. It is as
// much YAML as a workflow's placing keys need: block mappings, lists of them
// and block scalars.
func yamlKeys(src string) []yamlKey {
	type open struct {
		indent int
		key    string
	}
	var (
		keys  []yamlKey
		stack []open
		lines = strings.Split(src, "\n")
	)
	for i := 0; i < len(lines); i++ {
		m := yamlKeyLine.FindStringSubmatch(lines[i])
		if m == nil {
			if len(keys) > 0 {
				keys[len(keys)-1].loose += "\n" + uncommented(lines[i])
			}
			continue
		}
		// A key inside a list item sits two columns in for each dash.
		indent := len(m[1]) + len(m[2])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, open{indent, m[3]})
		k := yamlKey{item: m[2] != "", val: m[4], line: i + 1, loose: uncommented(m[4])}
		for _, o := range stack {
			k.path = append(k.path, o.key)
		}
		if yamlBlock.MatchString(k.val) {
			for i+1 < len(lines) && (strings.TrimSpace(lines[i+1]) == "" || len(lines[i+1])-len(strings.TrimLeft(lines[i+1], " ")) > len(m[1])) {
				i++
				k.block = append(k.block, lines[i])
			}
		}
		keys = append(keys, k)
	}
	return keys
}

// yamlScope is what a workflow, a job or a step sets for the commands below.
type yamlScope struct {
	cwd, shell string
	goEnv      string // GOOS, GOARCH or GOFLAGS, when env: sets one
}

type yamlStep struct {
	yamlScope
	run    []cmdLine // the run: value, or the lines of its block
	folded bool
}

type yamlJob struct {
	yamlScope
	runsOn string // "" in a composite action
	steps  []*yamlStep
}

// workflow is the part of a workflow or a composite action that places a
// command: defaults, runs-on, working-directory, shell, env and run.
type workflow struct {
	yamlScope
	jobs  []*yamlJob
	prose map[int]bool // the lines of name: and description:, never commands
}

func parseWorkflow(src string) workflow {
	w := workflow{prose: map[int]bool{}}
	var (
		job  *yamlJob
		step *yamlStep
	)
	for _, k := range yamlKeys(src) {
		// p is the key's path inside its scope: the workflow, a job or a step.
		p, scope, inStep := k.path, &w.yamlScope, false
		switch {
		case p[0] == "jobs" && len(p) == 2, p[0] == "runs" && len(p) == 1:
			job, step = &yamlJob{}, nil
			w.jobs = append(w.jobs, job)
			continue
		case p[0] == "jobs" && len(p) > 2:
			p, scope = p[2:], &job.yamlScope
		case p[0] == "runs":
			p, scope = p[1:], &job.yamlScope
		}
		inJob := scope != &w.yamlScope
		if inJob && p[0] == "steps" && len(p) > 1 {
			if len(p) == 2 && k.item {
				step = &yamlStep{}
				job.steps = append(job.steps, step)
			}
			if step == nil {
				continue
			}
			p, scope, inStep = p[1:], &step.yamlScope, true
		}
		key := strings.Join(p, "/")
		isRun := inStep && key == "run"
		isProse := p[len(p)-1] == "name" || p[len(p)-1] == "description"
		// A step names its directory and shell itself; a job and the
		// workflow name them under defaults: run:.
		setting, underDefaults := strings.CutPrefix(key, "defaults/run/")
		stepSets, defaultSets := inStep && !underDefaults, !inStep && underDefaults
		val := strings.Trim(uncommented(k.val), `'" `)
		// A flow mapping or a quoted key the cases below do not take.
		if m := goKey.FindStringSubmatch(k.loose); m != nil && !isRun && !isProse {
			scope.goEnv = m[1]
		}
		switch {
		case isRun && k.block != nil:
			step.run, step.folded = joinLines(k.block, k.line+1), k.val[0] == '>'
		case isRun:
			step.run = []cmdLine{{n: k.line, last: k.line, text: k.val}}
		case (stepSets || defaultSets) && setting == "working-directory":
			scope.cwd = val
		case (stepSets || defaultSets) && setting == "shell":
			scope.shell, _, _ = strings.Cut(val, " ")
		case inJob && !inStep && key == "runs-on":
			job.runsOn = val
		case len(p) == 2 && p[0] == "env" && goSetting.MatchString(p[1]):
			scope.goEnv = p[1]
		case isProse:
			for n := range len(k.block) + 1 {
				w.prose[k.line+n] = true
			}
		}
	}
	return w
}

// runnerGOOS maps a fixed runs-on label to the GOOS its steps test on.
func runnerGOOS(label string) string {
	for _, r := range [][2]string{{"windows", "windows"}, {"macos", "darwin"}, {"ubuntu", "linux"}} {
		if strings.Contains(label, r[0]) && !strings.ContainsAny(label, "$[") {
			return r[1]
		}
	}
	return ""
}

// workflowCmds reads the go commands of a workflow's or a composite action's
// run: steps.
func workflowCmds(name, src string) ([]goCmd, error) {
	w := parseWorkflow(src)
	read := w.prose
	var cmds []goCmd
	for _, j := range w.jobs {
		for _, st := range j.steps {
			sh := shell{file: name, read: read, goos: runnerGOOS(j.runsOn), cwd: cmp.Or(st.cwd, j.cwd, w.cwd)}
			goEnv := cmp.Or(st.goEnv, j.goEnv, w.goEnv)
			switch {
			case sh.goos == "":
				sh.unsure = fmt.Sprintf("its runner (runs-on: %s) is not a fixed ubuntu, macos or windows runner, so the platform that builds this leg is unknown: move the leg to a job with a fixed runs-on",
					cmp.Or(j.runsOn, "none, as in a composite action"))
			case goEnv != "":
				sh.unsure = "an env: block sets " + goEnv + " above this leg, where the reader does not look: set it on the leg's own command line"
			case strings.Contains(sh.cwd, "$"):
				sh.unsure = "the step's working-directory is an expression, so the leg's packages cannot be placed"
			}
			// Without a shell: key a Windows runner runs pwsh.
			switch shellName := cmp.Or(st.shell, j.shell, w.shell); {
			case shellName == "" && sh.goos == "windows":
				sh.foreign = "pwsh"
			case shellName != "" && shellName != "bash" && shellName != "sh":
				sh.foreign = shellName
			}
			// Folding joins lines; whether to one command depends on their indentation.
			if i := slices.IndexFunc(st.run, func(l cmdLine) bool { return runWord.MatchString(l.text) }); st.folded && i >= 0 {
				return nil, refuse(name, st.run[i].n, st.run[i].text, "a -run in a folded `run: >` block, which the reader does not fold: write the block as `run: |`")
			}
			got, err := sh.runAll(st.run)
			if err != nil {
				return nil, err
			}
			cmds = append(cmds, got...)
		}
	}
	return cmds, unreadRuns(name, src, read)
}

// skipDir reports a directory no walk here enters: dot directories (worktrees
// and caches hold whole copies of the tree), node_modules and testdata.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata"
}

// generateCmds reads the go commands of every go:generate directive in
// fsys. A directive runs in its own file's directory.
func generateCmds(fsys fs.FS) ([]goCmd, error) {
	var out []goCmd
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && p != "." && skipDir(d.Name()):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(p, ".go"):
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		for n, l := range strings.Split(string(src), "\n") {
			text, ok := strings.CutPrefix(l, "//go:generate ")
			if !ok {
				continue
			}
			named := runWord.MatchString(text) || strings.Contains(text, "-tags")
			if named && strings.Contains(text, "'") {
				return refuse(p, n+1, l, "go generate does not strip single quotes, so they reach the command as written: use double quotes or none")
			}
			sh := shell{file: p, read: map[int]bool{}, cwd: path.Dir(p)}
			cmds, err := sh.run(cmdLine{n: n + 1, last: n + 1, text: strings.TrimSpace(text)})
			if err != nil {
				return err
			}
			if strings.Contains(text, "-tags") && !slices.ContainsFunc(cmds, func(c goCmd) bool { return len(c.tags) > 0 }) {
				return refuse(p, n+1, l, "a -tags in a directive that is not a go test, go vet or go run, so nothing holds the tag to the vet recipe")
			}
			out = append(out, cmds...)
		}
		return nil
	})
	return out, err
}

// maxAlternatives bounds what runAlternatives expands.
const maxAlternatives = 256

// runAlternatives splits a -run pattern into one pattern per test it names
// when it is a finite alternation of literals (anchors kept), and returns it
// whole otherwise.
func runAlternatives(pattern string) ([]string, error) {
	if strings.Contains(pattern, "/") {
		return nil, fmt.Errorf("-run %s names a subtest, and only top-level names are checked: drop the part from the slash on, or teach runAlternatives in %s", pattern, readerFile)
	}
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, err
	}
	// The parser folds A|B into a class. One the pattern spells out is a
	// range of names, of which a single match is enough.
	if alts, ok := expandLiterals(re, !strings.ContainsAny(pattern, `[\`)); ok {
		return alts, nil
	}
	return []string{pattern}, nil
}

// expandLiterals lists the strings re matches, each as a pattern, or reports
// that they are not a short finite list.
func expandLiterals(re *syntax.Regexp, classes bool) ([]string, bool) {
	switch re.Op {
	case syntax.OpEmptyMatch:
		return []string{""}, true
	case syntax.OpBeginText:
		return []string{"^"}, true
	case syntax.OpEndText:
		return []string{"$"}, true
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			return nil, false
		}
		return []string{regexp.QuoteMeta(string(re.Rune))}, true
	case syntax.OpCharClass:
		var out []string
		for i := 0; classes && i+1 < len(re.Rune); i += 2 {
			if int64(re.Rune[i+1])-int64(re.Rune[i])+int64(len(out)) >= maxAlternatives {
				return nil, false
			}
			for r := re.Rune[i]; r <= re.Rune[i+1]; r++ {
				out = append(out, regexp.QuoteMeta(string(r)))
			}
		}
		return out, classes
	case syntax.OpCapture:
		return expandLiterals(re.Sub[0], classes)
	case syntax.OpQuest:
		sub, ok := expandLiterals(re.Sub[0], classes)
		return append([]string{""}, sub...), ok
	case syntax.OpAlternate:
		var out []string
		for _, s := range re.Sub {
			sub, ok := expandLiterals(s, classes)
			if !ok || len(out)+len(sub) > maxAlternatives {
				return nil, false
			}
			out = append(out, sub...)
		}
		return out, true
	case syntax.OpConcat:
		out := []string{""}
		for _, s := range re.Sub {
			sub, ok := expandLiterals(s, classes)
			if !ok || len(out)*len(sub) > maxAlternatives {
				return nil, false
			}
			var next []string
			for _, head := range out {
				for _, tail := range sub {
					next = append(next, head+tail)
				}
			}
			out = next
		}
		return out, true
	}
	return nil, false
}

// testNames lists what -run can select in d: the tests, fuzz targets and
// examples of the test files the command's GOOS, GOARCH, -tags and -race
// build.
func testNames(fsys fs.FS, d pkgDir, c goCmd) ([]string, error) {
	ctx := build.Default
	ctx.BuildTags = slices.Clone(c.tags)
	if c.race {
		ctx.BuildTags = append(ctx.BuildTags, "race")
	}
	ctx.GOOS, ctx.GOARCH = cmp.Or(c.goos, ctx.GOOS), cmp.Or(c.goarch, ctx.GOARCH)
	// MatchFile reads the file's build constraints through these.
	ctx.JoinPath = path.Join
	ctx.OpenFile = func(name string) (io.ReadCloser, error) { return fsys.Open(name) }
	var names []string
	readDir := func(dir string) error {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return fmt.Errorf("package %s is not a directory of this tree: %v", d, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			if ok, err := ctx.MatchFile(dir, e.Name()); err != nil {
				return err
			} else if !ok {
				continue
			}
			name := path.Join(dir, e.Name())
			src, err := fs.ReadFile(fsys, name)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && isRunnable(fn) {
					names = append(names, fn.Name.Name)
				}
			}
		}
		return nil
	}
	if !d.tree {
		return names, readDir(d.dir)
	}
	// ./... stops at a nested module.
	err := fs.WalkDir(fsys, d.dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.IsDir() {
			return err
		}
		if p != d.dir {
			if _, err := fs.Stat(fsys, path.Join(p, "go.mod")); err == nil || skipDir(e.Name()) {
				return fs.SkipDir
			}
		}
		return readDir(p)
	})
	return names, err
}

// isRunnable is go test's rule for what -run selects: TestXxx(*testing.T),
// FuzzXxx(*testing.F) and ExampleXxx(), where Xxx does not start with a
// lower-case letter.
func isRunnable(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	for _, kind := range [][2]string{{"Test", "T"}, {"Fuzz", "F"}, {"Example", ""}} {
		rest, ok := strings.CutPrefix(fn.Name.Name, kind[0])
		if r, _ := utf8.DecodeRuneInString(rest); !ok || fn.Recv != nil || unicode.IsLower(r) {
			continue
		}
		if kind[1] == "" {
			return len(params) == 0
		}
		if len(params) != 1 {
			return false
		}
		star, ok := params[0].Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == kind[1]
	}
	return false
}

// unnamedTests reports every alternative of a -run pattern that matches
// nothing in one of its command's package directories. `^$` names nothing on
// purpose (the bench and fuzz legs) and is skipped.
func unnamedTests(fsys fs.FS, cmds []goCmd) ([]string, error) {
	var problems []string
	for _, c := range cmds {
		if c.verb != "test" || !c.hasRun {
			continue
		}
		alts, err := runAlternatives(c.run)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", c.where, err)
		}
		for _, d := range c.dirs {
			names, err := testNames(fsys, d, c)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", c.where, err)
			}
			for _, alt := range alts {
				if alt == "^$" {
					continue
				}
				re, err := regexp.Compile(alt)
				if err != nil {
					return nil, fmt.Errorf("%s: %v", c.where, err)
				}
				if !slices.ContainsFunc(names, re.MatchString) {
					problems = append(problems, fmt.Sprintf("%s: -run %s: no test in %s matches %s", c.where, c.run, d, alt))
				}
			}
		}
	}
	return problems, nil
}

// unvettedTags reports every tag of a tagged command that no `go vet -tags`
// in vet runs over the command's package.
func unvettedTags(fsys fs.FS, vet, tagged []goCmd) []string {
	var problems []string
	for _, c := range tagged {
		for _, tag := range c.tags {
			covered := slices.ContainsFunc(vet, func(v goCmd) bool {
				return v.verb == "vet" && slices.Contains(v.tags, tag) &&
					slices.ContainsFunc(v.dirs, func(d pkgDir) bool { return d.holds(fsys, c.dirs[0].dir) })
			})
			if !covered {
				problems = append(problems, fmt.Sprintf("%s: the vet recipe has no `go vet -tags %s %s/`", c.where, tag, c.dirs[0]))
			}
		}
	}
	return problems
}

// repoSources is the go commands of every place the repository writes one:
// the Makefile and the makefiles it includes, the workflows and composite
// actions, the scripts, and the generate directives.
type repoSources struct {
	mk, workflows, scripts, directives []goCmd
}

func readRepo(t *testing.T, fsys fs.FS) repoSources {
	t.Helper()
	var r repoSources
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		b, err := fs.ReadFile(fsys, name)
		check(err)
		return string(b)
	}
	glob := func(patterns ...string) []string {
		var out []string
		for _, p := range patterns {
			found, err := fs.Glob(fsys, p)
			check(err)
			out = append(out, found...)
		}
		return out
	}
	makefiles := []string{"Makefile"}
	for i := 0; i < len(makefiles); i++ {
		// make itself stops at a missing include unless it is optional.
		if _, err := fs.Stat(fsys, makefiles[i]); err != nil && i > 0 {
			continue
		}
		cmds, includes, err := makeCmds(makefiles[i], read(makefiles[i]))
		check(err)
		r.mk = append(r.mk, cmds...)
		for _, inc := range includes {
			if !slices.Contains(makefiles, inc) {
				makefiles = append(makefiles, inc)
			}
		}
	}
	ymls := glob(".github/workflows/*.yml", ".github/workflows/*.yaml", ".github/actions/*/action.yml", ".github/actions/*/action.yaml")
	if len(ymls) == 0 {
		t.Fatal("no workflows found")
	}
	for _, f := range ymls {
		cmds, err := workflowCmds(f, read(f))
		check(err)
		r.workflows = append(r.workflows, cmds...)
	}
	// A script that is not sh, a Dockerfile and a compose file get the net
	// alone.
	check(fs.WalkDir(fsys, "scripts", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && skipDir(d.Name()):
			return fs.SkipDir
		case d.IsDir():
			return nil
		case !strings.HasSuffix(p, ".sh"):
			return unreadRuns(p, read(p), nil)
		}
		cmds, err := scriptCmds(p, read(p))
		r.scripts = append(r.scripts, cmds...)
		return err
	}))
	for _, f := range glob(netGlobs...) {
		check(unreadRuns(f, read(f), nil))
	}
	var err error
	r.directives, err = generateCmds(fsys)
	check(err)
	return r
}

// netGlobs match the files outside scripts that no reader takes commands
// from: Dockerfiles and compose files, at the root and one directory down.
var netGlobs = []string{"Dockerfile*", "*/Dockerfile*",
	"compose*.y*ml", "*/compose*.y*ml", "docker-compose*.y*ml", "*/docker-compose*.y*ml"}

// legSources are the sources known to hold a -run leg that names a test, so
// a reader gone blind to one fails. A source whose last leg moved elsewhere
// is taken off this list.
var legSources = []string{"the makefiles", "the workflows", "the generate directives"}

// leglessSources reports each of legSources in which the readers found no
// leg, with what to do about it.
func leglessSources(r repoSources) []string {
	found := map[string][]goCmd{"the makefiles": r.mk, "the workflows": r.workflows, "the generate directives": r.directives}
	var out []string
	for _, name := range legSources {
		if !slices.ContainsFunc(found[name], func(c goCmd) bool { return c.hasRun && c.run != "^$" }) {
			out = append(out, fmt.Sprintf("found no -run leg in %s. If one is still written there, the reader in %s no longer sees it and needs fixing. If the last one moved elsewhere on purpose, remove %q from legSources in that file.",
				name, readerFile, name))
		}
	}
	return out
}

// repoTree is the repository, which every reader here takes as a file system.
var repoTree = os.DirFS("../..")

// TestRunPatternsNameTests holds every `go test -run` leg to tests that
// exist: go test passes a pattern that matches nothing, having run nothing.
func TestRunPatternsNameTests(t *testing.T) {
	r := readRepo(t, repoTree)
	for _, p := range leglessSources(r) {
		t.Error(p)
	}
	if !slices.ContainsFunc(r.scripts, func(c goCmd) bool { return c.hasRun }) {
		t.Errorf("found no -run in the scripts, where the fuzz driver passes one. If it still does, the reader in %s no longer sees it; if it moved, drop this check in TestRunPatternsNameTests.", readerFile)
	}
	problems, err := unnamedTests(repoTree, slices.Concat(r.mk, r.workflows, r.scripts, r.directives))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestVetCoversGeneratorTags holds every build tag a generate directive
// names to a `go vet -tags` line over the directive's package in the vet
// recipe. Nothing else in the tree compiles a file behind such a tag.
func TestVetCoversGeneratorTags(t *testing.T) {
	r := readRepo(t, repoTree)
	vet := slices.DeleteFunc(r.mk, func(c goCmd) bool { return c.target != "vet" })
	if !slices.ContainsFunc(r.directives, func(c goCmd) bool { return len(c.tags) > 0 }) {
		t.Error("found no generate directive with -tags")
	}
	for _, p := range unvettedTags(repoTree, vet, r.directives) {
		t.Error(p)
	}
}

// treeOf is a tree of files, path to content.
func treeOf(files map[string]string) fs.FS {
	tree := fstest.MapFS{}
	for name, content := range files {
		tree[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return tree
}

const testFileHead = "package p\n\nimport \"testing\"\n\n"

// legTree is the tree the reader cases run against.
func legTree() fs.FS {
	return treeOf(map[string]string{
		"a_test.go": testFileHead + "func TestRoot(t *testing.T) {}\n",
		"gate/a_test.go": testFileHead + "func TestGate(t *testing.T) {}\nfunc TestMain(m *testing.M) {}\nfunc Testify(t *testing.T) {}\n" +
			"func FuzzGate(f *testing.F) {}\nfunc ExampleGate() {}\n",
		"gate/b_windows_test.go": testFileHead + "func TestOnWindows(t *testing.T) {}\n",
		"gate/c_test.go":         "//go:build gen\n\n" + testFileHead + "func TestGenerate(t *testing.T) {}\n",
		"gate/r_test.go":         "//go:build race\n\n" + testFileHead + "func TestRaceOnly(t *testing.T) {}\n",
		"gate/n_test.go":         "//go:build !race\n\n" + testFileHead + "func TestNoRace(t *testing.T) {}\n",
		"shapes/a_test.go": testFileHead + "/*\nfunc TestCommentedOut(t *testing.T) {\n}\n*/\n\nconst raw = `\nfunc TestInRawString(t *testing.T) {\n}\n`\n\n" +
			"func TestMultiLine(\n\tt *testing.T,\n) {\n}\n\ntype S struct{}\n\nfunc (S) TestMethod(t *testing.T) {}\n",
		"other/a_test.go":      testFileHead + "func TestOther(t *testing.T) {}\n",
		"cli/a_test.go":        testFileHead + "func TestCli(t *testing.T) {}\n",
		"mod/sub/a_test.go":    testFileHead + "func TestDeep(t *testing.T) {}\n",
		"mod/nested/go.mod":    "module nested\n",
		"mod/nested/a_test.go": testFileHead + "func TestNested(t *testing.T) {}\n",
	})
}

// legCase is command text and what the reader makes of it: the reports of
// unnamed tests in order (a substring of each), or a word of its refusal.
type legCase struct {
	name, src string
	want      []string
	refused   string
}

func runLegCases(t *testing.T, read func(src string) ([]goCmd, error), cases []legCase) {
	t.Helper()
	root := legTree()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmds, err := read(c.src)
			var got []string
			if err == nil {
				got, err = unnamedTests(root, cmds)
			}
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("read as %+v with reports %q and error %v, want a refusal holding %q", cmds, got, err, c.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("reports = %q, want %d", got, len(c.want))
			}
			for i, want := range c.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("report %d = %q, want it to hold %q", i, got[i], want)
				}
			}
		})
	}
}

func readMake(src string) ([]goCmd, error) {
	cmds, _, err := makeCmds("Makefile", src)
	return cmds, err
}

// Words of the refusals the cases below match.
const (
	notALeg     = "not read as a go test leg"
	onlyPrinted = "print it with echo or printf"
	setApart    = "on the leg's own command line"
)

// In the refusals below the leg itself is sound (TestGate is in ./gate), so
// each passes in silence if its refusal is lost.
func TestAMakefileLegIsReadOrRefused(t *testing.T) {
	const missing = "-run TestNoSuch: no test in ./gate matches TestNoSuch"
	runLegCases(t, readMake, []legCase{
		{name: "a test that exists", src: "gate:\n\tgo test -run TestGate ./gate\n"},
		{name: "a test that does not", src: "gate:\n\tgo test -run TestNoSuchGate -count=1 -v ./gate\n",
			want: []string{"Makefile:2: -run TestNoSuchGate: no test in ./gate matches TestNoSuchGate"}},
		{name: "one alternative of three", src: "gate:\n\tgo test -run '^Test(Gate|Gone|Other)$$' ./gate\n",
			want: []string{"no test in ./gate matches ^TestGone$", "no test in ./gate matches ^TestOther$"}},
		{name: "each directory of a continued line", src: "gate:\n\tgo test -run 'TestGate$$' ./gate \\\n\t\t./other -update\n",
			want: []string{"Makefile:2: -run TestGate$: no test in ./other matches TestGate$"}},
		{name: "cd moves the package", src: "gate:\n\tcd gate && go test -run TestGate .\n\tcd other && go test -run TestGate\n",
			want: []string{"Makefile:3: -run TestGate: no test in ./other matches TestGate"}},
		{name: "-C moves the package", src: "gate:\n\tgo test -C other -run TestGate .\n", want: []string{"no test in ./other matches"}},
		{name: "the root package prints as a dot", src: "gate:\n\tgo test -run TestRoot .\n\tgo test -run=TestNoSuch .\n",
			want: []string{"Makefile:3: -run TestNoSuch: no test in . matches TestNoSuch"}},
		{name: "--run is -run", src: "gate:\n\tgo test --run TestNoSuch ./gate\n\tgo test --run=TestNoSuch ./gate\n", want: []string{missing, missing}},
		{name: "a pattern kept whole", src: "gate:\n\tgo test -run 'Test[A-Z][a-z]+$$' ./gate\n\tgo test -run 'Test[0-9]+$$' ./gate\n",
			want: []string{"no test in ./gate matches Test[0-9]+$"}},
		{name: "a written class is one name", src: "gate:\n\tgo test -run 'TestGat[a-z]$$' ./gate\n"},
		{name: "the empty pattern names nothing on purpose", src: "bench:\n\tgo test -run '^$$' -bench . -benchtime 2s -benchmem ./...\n"},
		{name: "the empty pattern beside what cannot be read", src: "fuzz:\n\tgo test -run '^$$' -fuzz \"^$$t\\$$\" $$parallel -newflag \"$$pkg\" 2>&1 | tee log\n"},
		{name: "not a test: TestMain, Testify", src: "gate:\n\tgo test -run 'TestMain|Testify' ./gate\n",
			want: []string{"matches TestMain", "matches Testify"}},
		{name: "only a declared function is a test", src: "gate:\n\tgo test -run 'Test(CommentedOut|InRawString|MultiLine|Method)$$' ./shapes\n",
			want: []string{"matches TestCommentedOut$", "matches TestInRawString$", "matches TestMethod$"}},
		{name: "a fuzz target and an example are named", src: "gate:\n\tgo test -run '^FuzzGate$$' ./gate\n\tgo test -run ExampleGate ./gate\n\tgo test -run '^FuzzGone$$' ./gate\n",
			want: []string{"Makefile:4: -run ^FuzzGone$"}},
		{name: "a tagged file needs its tag", src: "gate:\n\tgo test -run TestGenerate ./gate\n\tgo test -tags gen -run TestGenerate ./gate\n",
			want: []string{"Makefile:2: -run TestGenerate: no test in ./gate matches TestGenerate"}},
		{name: "-race sets the race tag", src: "gate:\n\tgo test -race -run TestRaceOnly ./gate\n\tgo test -run TestRaceOnly ./gate\n\tgo test -race -run TestNoRace ./gate\n",
			want: []string{"Makefile:3: -run TestRaceOnly", "Makefile:4: -run TestNoRace"}},
		{name: "a windows file needs GOOS", src: "gate:\n\tGOOS=linux go test -run TestOnWindows ./gate\n\tGOOS=windows go test -run TestOnWindows ./gate\n\tenv GOOS=windows go test -run TestOnWindows ./gate\n",
			want: []string{"Makefile:2: -run TestOnWindows: no test in ./gate matches TestOnWindows"}},
		{name: "a tree stops at a nested module", src: "gate:\n\tcd mod && go test -run TestDeep ./...\n\tcd mod && go test -run TestNested ./...\n",
			want: []string{"Makefile:3: -run TestNested: no test in ./mod/... matches TestNested"}},
		{name: "flags with values, and -args", src: "gate:\n\tgo test -coverprofile cover.out -shuffle on -fuzzminimizetime 10s -run TestNoSuch ./gate -args -x 1 ./y\n", want: []string{missing}},
		{name: "a redirection is no package", src: "gate:\n\tgo test -run TestNoSuch ./gate > log.txt\n\tgo test -run TestNoSuch ./gate >log.txt 2>&1 | tee x\n\tgo test -run TestNoSuch ./gate 2> err.txt\n",
			want: []string{missing, missing, missing}},
		{name: "commands after the leg", src: "gate:\n\tWAX=1 go test -run TestNoSuch ./gate || true\n\tgo test -run TestNoSuch ./gate; echo done\n", want: []string{missing, missing}},
		{name: "a target with a path", src: "bin/x:\n\tgo test -run TestNoSuch ./gate\n", want: []string{missing}},
		{name: "two targets", src: "a b:\n\tgo test -run TestNoSuch ./gate\n", want: []string{missing}},
		{name: "a pattern rule", src: "%-gate:\n\tgo test -run TestNoSuch ./gate\n", want: []string{missing}},
		{name: "a conditional inside a recipe", src: "gate:\nifdef CI\n\tgo test -run TestNoSuch ./gate\nendif\n", want: []string{missing}},
		{name: "a variable between header and recipe", src: "gate:\nX := 1\n\tgo test -run TestNoSuch ./gate\n", want: []string{missing}},
		{name: "a recipe on the header line", src: "gate: ; go test -run TestNoSuch ./gate\n", want: []string{missing}},
		{name: "a tab line in a define", src: "define RUNIT\n\tgo test -run TestNoSuch ./gate\nendef\n", want: []string{missing}},
		{name: "prose that names go test", src: "X := 1 # see go test -run TestNoSuch\n.PHONY: gate # -run is checked\n$(info run go test -run TestNoSuch by hand)\n" +
			"gate:\n\t@echo \"now run go test -run TestNoSuch by hand\"\n\tgo vet ./gate # go test -run TestNoSuch comes later\n" +
			"\tprintf 'go test -run %s\\n' TestNoSuch\n\t# go test -run TestNoSuch \\\n\t./gate\n"},
		{name: "a program's own -run flag", src: "gate:\n\tgo run ./cmd/x -run fast\n"},

		{name: "go held in a variable", src: "gate:\n\t$(GO) test -run TestGate ./gate\n", refused: notALeg},
		{name: "a command held in a variable", src: "RUN = go test -run TestGate ./gate\ngate:\n\t$(RUN)\n", refused: "make variable"},
		{name: "a command held in a define", src: "define RUNIT\ngo test -run TestGate ./gate\nendef\n", refused: "make variable"},
		{name: "a target's variable with a semicolon", src: "gate: FLAGS = -run TestGate; x\ngate:\n\tgo test $(FLAGS) ./gate\n", refused: "make variable"},
		{name: "flags held in a variable", src: "gate:\n\tgo test $(TESTFLAGS) -run TestGate ./gate\n", refused: "the word $(TESTFLAGS) is a make or shell variable"},
		{name: "a wrapper", src: "gate:\n\ttimeout 30m go test -run TestGate ./gate\n", refused: notALeg},
		{name: "a wrapper beside a leg", src: "gate:\n\tgo test --run TestGate ./gate && timeout 5m go test -run TestGate ./gate\n", refused: notALeg},
		{name: "a subshell", src: "gate:\n\t(cd gate && go test -run TestGate .)\n", refused: "sits in a subshell"},
		{name: "a subshell with a command after the leg", src: "gate:\n\t(cd gate && go test -run TestGate . && echo ok)\n", refused: "sits in a subshell"},
		{name: "a leg alone in a subshell", src: "gate:\n\t(go test -run TestGate ./gate)\n", refused: "sits in a subshell"},
		{name: "a package that is a bare name", src: "gate:\n\tgo test -run TestGate gate\n", refused: "not a relative directory"},
		{name: "an if", src: "gate:\n\tif ! go test -run TestGate ./gate; then exit 1; fi\n", refused: notALeg},
		{name: "a loop", src: "gate:\n\tfor d in gate cli; do go test -run TestGate ./gate; done\n", refused: notALeg},
		{name: "a quoted command", src: "gate:\n\tbash -c 'go test -run TestGate ./gate'\n", refused: notALeg},
		{name: "a command substitution under echo", src: "gate:\n\techo $$(go test -run TestGate ./gate)\n", refused: notALeg},
		{name: "text printed by another command", src: "gate:\n\t./scripts/say.sh \"hint: go test -run TestGate ./gate\"\n", refused: onlyPrinted},
		{name: "a cd into a variable", src: "gate:\n\tcd $(DIR) && go test -run TestGate ./gate\n", refused: "directory change"},
		{name: "an unknown flag", src: "gate:\n\tgo test -run TestGate -frobnicate on ./gate\n", refused: "boolFlags"},
		{name: "GOFLAGS ahead of the command", src: "gate:\n\tGOFLAGS=-count=1 go test -run TestGate ./gate\n", refused: "pass the flag on the command"},
		{name: "GOOS exported apart", src: "gate:\n\texport GOOS=linux; go test -run TestGate ./gate\n", refused: setApart},
		{name: "an unbalanced quote", src: "gate:\n\tgo test -run 'TestGate ./gate\n", refused: "unbalanced"},
		{name: "a subtest", src: "gate:\n\tgo test -run TestGate/sub ./gate\n", refused: "subtest"},
		{name: "a package that is no directory", src: "gate:\n\tgo test -run TestGate ./nodir\n", refused: "./nodir is not a directory"},
		{name: "an include it cannot follow", src: "include $(TOOLS).mk\ngate:\n\tgo test -run TestGate ./gate\n", refused: "an include the reader cannot follow"},
	})
	_, includes, err := makeCmds("Makefile", "include tools.mk # the tools\n-include local.mk\nsinclude extra.mk\n")
	if want := []string{"tools.mk", "local.mk", "extra.mk"}; err != nil || !slices.Equal(includes, want) {
		t.Errorf("includes = %q, %v, want %q", includes, err, want)
	}
}

func TestAWorkflowLegIsReadOrRefused(t *testing.T) {
	job := func(runsOn, steps string) string {
		return "jobs:\n  a:\n    runs-on: " + runsOn + "\n    steps:\n" + steps
	}
	const leg = "      - run: go test -run TestGate ./gate\n"
	read := func(src string) ([]goCmd, error) { return workflowCmds("ci.yml", src) }
	runLegCases(t, read, []legCase{
		{name: "a step directly under runs-on", src: job("ubuntu-latest", "      - run: go test -run TestNoSuch ./gate\n"),
			want: []string{"ci.yml:5: -run TestNoSuch: no test in ./gate matches TestNoSuch"}},
		{name: "each job on its own runner", src: job("windows-latest", "      - run: go test -run '^Test(OnWindows|Gone)$' ./gate\n") +
			"  b:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: go test -run TestOnWindows ./gate\n",
			want: []string{"ci.yml:5: -run ^Test(OnWindows|Gone)$: no test in ./gate matches ^TestGone$", "ci.yml:9: -run TestOnWindows"}},
		{name: "working-directory before run", src: job("ubuntu-latest", "      - name: cli tests\n        working-directory: cli\n        shell: bash\n        run: go test -run TestGate .\n"),
			want: []string{"ci.yml:8: -run TestGate: no test in ./cli matches"}},
		{name: "working-directory after run", src: job("ubuntu-latest", "      - run: go test -run TestGate .\n        working-directory: cli\n"),
			want: []string{"no test in ./cli matches"}},
		{name: "working-directory after a list", src: job("ubuntu-latest", "      - name: x\n        with:\n          paths:\n            - a\n        working-directory: cli\n        run: go test -run TestCli .\n")},
		{name: "steps flush with their key", src: "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n    - uses: actions/checkout@v7\n    - name: t\n      working-directory: cli\n      run: go test -run TestGate .\n",
			want: []string{"ci.yml:8: -run TestGate: no test in ./cli matches"}},
		{name: "job defaults", src: "jobs:\n  a:\n    runs-on: ubuntu-latest\n    defaults:\n      run:\n        working-directory: cli\n    steps:\n      - uses: actions/checkout@v7\n      - run: go test -run TestGate .\n",
			want: []string{"no test in ./cli matches"}},
		{name: "workflow defaults", src: "defaults:\n  run:\n    working-directory: cli\n" + job("ubuntu-latest", "      - run: go test -run TestGate .\n"),
			want: []string{"no test in ./cli matches"}},
		{name: "cd carries down a block", src: job("ubuntu-latest", "      - run: |-\n          cd cli\n\n          go test -run TestCli .\n          go test -run TestGate .\n      - run: go test -run TestGate ./gate\n"),
			want: []string{"ci.yml:9: -run TestGate: no test in ./cli matches"}},
		{name: "a continued line in a block", src: job("ubuntu-latest", "      - run: |\n          WAX=1 \\\n            go test -run TestNoSuch \\\n            ./gate\n"),
			want: []string{"ci.yml:6: -run TestNoSuch: no test in ./gate"}},
		{name: "a here-document's lines are read", src: job("ubuntu-latest", "      - run: |\n          bash <<'EOF'\n          go test -run TestNoSuch ./gate\n          EOF\n"),
			want: []string{"ci.yml:7: -run TestNoSuch"}},
		{name: "a plain go line in a step that is not sh", src: job("windows-latest", "      - run: go test -run TestOnWindows ./gate\n      - shell: pwsh\n        run: go test -run TestNoSuch ./gate\n"),
			want: []string{"ci.yml:7: -run TestNoSuch"}},
		{name: "a matrix job without a leg", src: "jobs:\n  a:\n    strategy:\n      matrix:\n        os: [ubuntu-latest, macos-latest]\n    runs-on: ${{ matrix.os }}\n    steps:\n      - run: make test\n"},
		{name: "prose that names go test", src: job("ubuntu-latest", "      - name: go test -run TestNoSuch with race\n        run: make test # instead of go test -run TestNoSuch ./...\n        timeout-minutes: 15 # go test -run TestNoSuch takes long\n"+
			"      # go test -run TestNoSuch\n      - run: |\n          # go test -run TestNoSuch\n          echo \"go test -run TestNoSuch\"\n")},

		{name: "a matrix runner", src: job("${{ matrix.os }}", "      - if: matrix.os == 'ubuntu-latest'\n        run: go test -run TestGate ./gate\n"), refused: "fixed runs-on"},
		{name: "a self-hosted runner", src: job("[self-hosted, linux]", leg), refused: "fixed runs-on"},
		{name: "a composite action", src: "name: tools\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: go test -run TestGate ./gate\n", refused: "fixed runs-on"},
		{name: "step env sets GOOS", src: job("ubuntu-latest", leg+"        env:\n          GOOS: linux\n"), refused: "env: block sets GOOS"},
		{name: "job env sets GOARCH", src: "jobs:\n  a:\n    env:\n      GOARCH: amd64\n    runs-on: ubuntu-latest\n    steps:\n" + leg, refused: "env: block sets GOARCH"},
		{name: "workflow env sets GOFLAGS", src: "env:\n  GOFLAGS: -mod=readonly\n" + job("ubuntu-latest", leg), refused: "env: block sets GOFLAGS"},
		{name: "a flow env on a step", src: job("ubuntu-latest", leg+"        env: {GOOS: linux}\n"), refused: "env: block sets GOOS"},
		{name: "a flow env on a job", src: "jobs:\n  a:\n    env: { \"GOARCH\": amd64 }\n    runs-on: ubuntu-latest\n    steps:\n" + leg, refused: "env: block sets GOARCH"},
		{name: "a flow env at the top", src: "env: {X: 1,\n  GOFLAGS: -mod=readonly}\n" + job("ubuntu-latest", leg), refused: "env: block sets GOFLAGS"},
		{name: "a quoted env key", src: job("ubuntu-latest", leg+"        env:\n          \"GOOS\": linux\n"), refused: "env: block sets GOOS"},
		{name: "GOOS exported in the block", src: job("ubuntu-latest", "      - run: |\n          export GOOS=linux\n          go test -run TestGate ./gate\n"), refused: setApart},
		{name: "a command ahead of the leg in pwsh", src: job("ubuntu-latest", "      - shell: pwsh\n        run: |\n          Set-Location other\n          go test -run TestGate ./gate\n"), refused: "shell: bash"},
		{name: "a command ahead of the leg in a Windows runner's default shell", src: job("windows-latest", "      - run: |\n          $env:X = 1\n          go test -run TestGate ./gate\n"), refused: "shell: bash"},
		{name: "a cd it cannot follow", src: job("ubuntu-latest", "      - run: |\n          pushd gate\n          go test -run TestGate ./gate\n"), refused: "directory change"},
		{name: "a folded block", src: job("ubuntu-latest", "      - run: >\n          go test -run TestGate\n          ./gate\n"), refused: "folded"},
		{name: "a quoted run", src: job("ubuntu-latest", "      - run: \"go test -run TestGate ./gate\"\n"), refused: notALeg},
		{name: "a quoted run key", src: job("ubuntu-latest", "      - \"run\": go test -run TestGate ./gate\n"), refused: "outside the command text"},
		{name: "a working-directory from an expression", src: job("ubuntu-latest", leg+"        working-directory: ${{ matrix.dir }}\n"), refused: "working-directory is an expression"},
		{name: "a command outside run", src: job("ubuntu-latest", "      - uses: some/action@v1\n        with:\n          command: go test -run TestGate ./gate\n"), refused: "outside the command text"},
		{name: "a script for another tool", src: job("ubuntu-latest", "      - uses: actions/github-script@v7\n        with:\n          script: |\n            exec('go test -run TestGate ./gate')\n"), refused: "outside the command text"},
	})
}

func TestAScriptLegIsReadOrRefused(t *testing.T) {
	read := func(src string) ([]goCmd, error) { return scriptCmds("scripts/x.sh", src) }
	runLegCases(t, read, []legCase{
		{name: "the fuzz driver's shape", src: "#!/usr/bin/env bash\n# go test -run TestNoSuch\nrun() {\n\tgo test -run '^$' -fuzz \"^${target}\\$\" -fuzztime \"$fuzztime\" $parallel -v \"$pkg\" 2>&1 | tee \"$log\"\n}\n\techo \"  go test -run='^\\$' -fuzz='^FuzzName\\$' ./path/to/pkg\"\n"},
		{name: "a leg", src: "set -eu\ncd gate\ngo test -run TestNoSuch .\n", want: []string{"scripts/x.sh:3: -run TestNoSuch: no test in ./gate matches"}},
		{name: "a leg under an if", src: "if [ -n \"$CI\" ]; then\n  go test -run TestNoSuch ./gate\nfi\n", want: []string{"scripts/x.sh:2: -run TestNoSuch"}},
		{name: "a cd it cannot follow", src: "cd \"$(dirname \"$0\")/..\"\ngo test -run TestGate ./gate\n", refused: "directory change"},
		{name: "a subshell open over lines", src: "(\n  cd gate\n  go test -run TestGate .\n)\n", refused: "sits in a subshell"},
		{name: "a leg after a subshell that closed", src: "(cd other && make)\ngo test -run TestNoSuch ./gate\n", want: []string{"scripts/x.sh:2: -run TestNoSuch"}},
		{name: "a leg after a subshell closed on its own line", src: "(\n  cd other\n  export GOOS=windows\n) || exit 1\ngo test -run TestGate .\n",
			want: []string{"scripts/x.sh:5: -run TestGate: no test in . matches TestGate"}},
		{name: "a leg on the line that closes a subshell", src: "( cd other\n  make\n) && go test -run TestNoSuch ./gate\n", want: []string{"scripts/x.sh:3: -run TestNoSuch: no test in ./gate matches"}},
		{name: "a doubt from before a subshell outlasts it", src: "export GOOS=linux\n(\n  cd other\n)\ngo test -run TestGate ./gate\n", refused: setApart},
		{name: "a leg after a subshell with other parentheses", src: "(\n  cd other\n  args=(a\n  b)\n  make\n)\ngo test -run TestGate ./gate\n", refused: "cannot pair"},
		{name: "a leg after a subshell inside a subshell", src: "(\n  (\n    cd other\n  )\n  cd gate\n)\ngo test -run TestGate ./gate\n", refused: "cannot pair"},
		{name: "a package held in a variable", src: "go test -run TestGate \"$pkg\"\n", refused: "the word $pkg is a make or shell variable"},
		{name: "GOOS exported apart", src: "export GOOS=linux\ngo test -run TestGate ./gate\n", refused: setApart},
		{name: "flags held in a variable", src: "RUNFLAGS=\"-run TestGate\"\ngo test $RUNFLAGS ./gate\n", refused: notALeg},
		{name: "flags held in an array", src: "args=(-run TestGate)\ngo test \"${args[@]}\" ./gate\n", refused: notALeg},
		{name: "a function that wraps go test", src: "run_go() { go test \"$@\"; }\nrun_go -run TestGate ./gate\n", refused: notALeg},
		{name: "a case arm", src: "case $1 in\n  x) go test -run TestGate ./gate ;;\nesac\n", refused: notALeg},
		{name: "text printed by a helper", src: "say \"try: go test -run TestGate ./gate\"\n", refused: onlyPrinted},
		{name: "text printed by a here-document", src: "cat <<EOF\nusage: go test -run TestGate ./gate\nEOF\n", refused: onlyPrinted},
	})

	// A file with no reader holds no go test with a -run at all.
	runLegCases(t, func(src string) ([]goCmd, error) { return nil, unreadRuns("Dockerfile", src, nil) }, []legCase{
		{name: "a file without a leg", src: "FROM golang\nRUN go build -run x ./...\n# RUN go test -run TestGate ./gate\nRUN go test ./...\n"},
		{name: "a leg in a Dockerfile", src: "FROM golang\nRUN go test -run TestGate ./gate\n", refused: "takes no commands from"},
		{name: "a leg in a PowerShell script", src: "go test --run=TestGate ./gate | Out-Null\n", refused: "takes no commands from"},
		{name: "a leg in a compose file", src: "services:\n  check:\n    command: go test -run TestGate ./gate\n", refused: "takes no commands from"},
	})
}

// A compose file is found under every name compose reads, at the root and
// one directory down, beside the Dockerfiles.
func TestComposeFilesAreInTheNet(t *testing.T) {
	want := []string{"Dockerfile", "compose.override.yml", "compose.yaml", "deploy/Dockerfile.ci", "deploy/docker-compose.yaml", "docker-compose.dev.yml"}
	files := map[string]string{"composer.json": "", "deploy/notes.yml": "", "a/b/compose.yaml": ""}
	for _, name := range want {
		files[name] = ""
	}
	root := treeOf(files)
	var got []string
	for _, pattern := range netGlobs {
		found, err := fs.Glob(root, pattern)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, found...)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the net holds %q, want %q", got, want)
	}
}

// A source that holds no leg any more fails with where the expectation is
// kept and that it may be dropped for that source.
func TestASourceWithNoLegSaysWhatToDo(t *testing.T) {
	leg := []goCmd{{hasRun: true, run: "TestGate"}}
	if got := leglessSources(repoSources{mk: leg, workflows: leg, directives: leg}); len(got) != 0 {
		t.Errorf("every source holds a leg, and the guard reports %q", got)
	}
	got := leglessSources(repoSources{mk: leg, workflows: []goCmd{{hasRun: true, run: "^$"}}, directives: leg})
	if len(got) != 1 {
		t.Fatalf("the workflows hold no leg, and the guard reports %q", got)
	}
	for _, want := range []string{"no -run leg in the workflows", readerFile, `remove "the workflows" from legSources`} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the report %q does not hold %q", got[0], want)
		}
	}
}

func TestRunPatternExpandsNestedAlternatives(t *testing.T) {
	for _, c := range []struct {
		pattern string
		want    []string
	}{
		{`^Test(A|B(C|D))$`, []string{"^TestA$", "^TestBC$", "^TestBD$"}},
		{`TestA|TestAB`, []string{"TestA", "TestAB"}},
		{`TestSoak|TestLoad|TestTTFA`, []string{"TestSoak", "TestLoad", "TestTTFA"}},
		{`TestGolden(CacheKeys|KnobAnswers)$`, []string{"TestGoldenCacheKeys$", "TestGoldenKnobAnswers$"}},
		{`^TestPath(ForAPID)?$`, []string{"^TestPath$", "^TestPathForAPID$"}},
		{`TestGolden`, []string{"TestGolden"}},
		{`^$`, []string{"^$"}},
		{`Test.*Quality`, []string{`Test.*Quality`}},
		{`TestGolden[A-Z]`, []string{`TestGolden[A-Z]`}},
		{`Test\d`, []string{`Test\d`}},
		{`(?i)testgate`, []string{`(?i)testgate`}},
	} {
		got, err := runAlternatives(c.pattern)
		if err != nil {
			t.Errorf("%s: %v", c.pattern, err)
			continue
		}
		slices.Sort(got)
		slices.Sort(c.want)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s expands to %q, want %q", c.pattern, got, c.want)
		}
	}
}

func TestAGenerateDirectiveIsHeldToItsTestsAndItsVetLine(t *testing.T) {
	const quoted = "const quoted = \"\\n//go:generate go test -run TestInAString\\n\"\n"
	root := treeOf(map[string]string{
		"codec/gen.go":         "package p\n\n//go:generate go test -tags gen -run ^TestGenerate$ -count=1\n\n" + quoted,
		"codec/gen_test.go":    "//go:build gen\n\n" + testFileHead + "func TestGenerate(t *testing.T) {}\n",
		"codec/stale.go":       "package p\n\n//go:generate go test -run \"^TestGenerateGone$\" -count=1\n",
		"tables/gen.go":        "package p\n\n//go:generate go run -tags tablesgen gen_main.go -run fast\n",
		"cli/go.mod":           "module cli\n",
		"cli/label/gen.go":     "package p\n\n//go:generate go run -tags cligen gen_main.go\n",
		".copy/codec/stale.go": "package p\n\n//go:generate go test -run ^TestInADotDirectory$\n",
		"testdata/stale.go":    "package p\n\n//go:generate go test -run ^TestInTestdata$\n",
	})
	directives, err := generateCmds(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unnamedTests(root, directives)
	want := []string{"codec/stale.go:3: -run ^TestGenerateGone$: no test in ./codec matches ^TestGenerateGone$"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("reports = %q, %v, want %q", got, err, want)
	}

	// A tag is vetted by a line over its own package or over a tree above
	// it in the same module.
	const cli = "\tcd cli && go vet -tags cligen ./...\n"
	for recipe, want := range map[string][]string{
		"\tgo vet -tags gen ./codec/\n\tgo vet -tags tablesgen ./tables\n" + cli:  nil,
		"\tgo vet -tags gen,tablesgen ./...\n" + cli:                              nil,
		"\tgo vet -tags gen,tablesgen,cligen ./...\n":                             {"cli/label/gen.go:3: the vet recipe has no `go vet -tags cligen ./cli/label/`"},
		"\tgo vet -tags gen ./tables\n\tgo vet -tags tablesgen ./codec/\n" + cli:  {"codec/gen.go:3: the vet recipe has no `go vet -tags gen ./codec/`", "tables/gen.go:3: the vet recipe has no `go vet -tags tablesgen ./tables/`"},
		"\tgo test -tags gen ./codec\n\tgo vet -tags tablesgen ./tab/...\n" + cli: {"go vet -tags gen ./codec/", "go vet -tags tablesgen ./tables/"},
	} {
		vet, err := readMake("vet:\n" + recipe)
		if err != nil {
			t.Fatal(err)
		}
		got := unvettedTags(root, vet, directives)
		if len(got) != len(want) {
			t.Errorf("%q: reports = %q, want %q", recipe, got, want)
			continue
		}
		for i := range want {
			if !strings.Contains(got[i], want[i]) {
				t.Errorf("%q: report %d = %q, want it to hold %q", recipe, i, got[i], want[i])
			}
		}
	}

	for name, c := range map[string]struct{ directive, refused string }{
		"single quotes reach the command": {"//go:generate go test -run '^TestGenerate$'", "single quotes"},
		"a tag no go command takes":       {"//go:generate stringer -tags gen -type=Op", "nothing holds the tag"},
	} {
		root := treeOf(map[string]string{
			"codec/gen.go":      "package p\n\n" + c.directive + "\n",
			"codec/gen_test.go": testFileHead + "func TestGenerate(t *testing.T) {}\n",
		})
		if cmds, err := generateCmds(root); err == nil || !strings.Contains(err.Error(), c.refused) {
			t.Errorf("%s: read as %+v, %v, want a refusal holding %q", name, cmds, err, c.refused)
		}
	}
}
