package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/groove-x/go-licenses/assets"
	"github.com/groove-x/go-licenses/modinfo"
)

type Template struct {
	Title    string
	Nickname string
	Words    map[string]int
}

func parseTemplate(content string) (*Template, error) {
	t := Template{}
	text := []byte{}
	state := 0
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if state == 0 {
			if line == "---" {
				state = 1
			}
		} else if state == 1 {
			if line == "---" {
				state = 2
			} else {
				if strings.HasPrefix(line, "title:") {
					t.Title = strings.TrimSpace(line[len("title:"):])
				} else if strings.HasPrefix(line, "nickname:") {
					t.Nickname = strings.TrimSpace(line[len("nickname:"):])
				}
			}
		} else if state == 2 {
			text = append(text, scanner.Bytes()...)
			text = append(text, []byte("\n")...)
		}
	}
	t.Words = makeWordSet(text)
	return &t, scanner.Err()
}

func loadTemplates() ([]*Template, error) {
	templates := []*Template{}
	for _, a := range assets.Assets {
		templ, err := parseTemplate(a.Content)
		if err != nil {
			return nil, err
		}
		templates = append(templates, templ)
	}
	return templates, nil
}

var (
	reWords     = regexp.MustCompile(`[\w']+`)
	reCopyright = regexp.MustCompile(
		`(?i)\s*Copyright (?:©|\(c\)|\xC2\xA9)?\s*(?:\d{4}|\[year\]).*`)
)

func cleanLicenseData(data []byte) []byte {
	data = bytes.ToLower(data)
	data = reCopyright.ReplaceAll(data, nil)
	return data
}

func makeWordSet(data []byte) map[string]int {
	words := map[string]int{}
	data = cleanLicenseData(data)
	matches := reWords.FindAll(data, -1)
	for i, m := range matches {
		s := string(m)
		if _, ok := words[s]; !ok {
			// Non-matching words are likely in the license header, to mention
			// copyrights and authors. Try to preserve the initial sequences,
			// to display them later.
			words[s] = i
		}
	}
	return words
}

type Word struct {
	Text string
	Pos  int
}

type sortedWords []Word

func (s sortedWords) Len() int {
	return len(s)
}

func (s sortedWords) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s sortedWords) Less(i, j int) bool {
	return s[i].Pos < s[j].Pos
}

type MatchResult struct {
	Template     *Template
	Score        float64
	ExtraWords   []string
	MissingWords []string
}

func sortAndReturnWords(words []Word) []string {
	sort.Sort(sortedWords(words))
	tokens := []string{}
	for _, w := range words {
		tokens = append(tokens, w.Text)
	}
	return tokens
}

// matchTemplates returns the best license template matching supplied data,
// its score between 0 and 1 and the list of words appearing in license but not
// in the matched template.
func matchTemplates(license []byte, templates []*Template) MatchResult {
	bestScore := float64(-1)
	var bestTemplate *Template
	bestExtra := []Word{}
	bestMissing := []Word{}
	words := makeWordSet(license)
	for _, t := range templates {
		extra := []Word{}
		missing := []Word{}
		common := 0
		for w, pos := range words {
			_, ok := t.Words[w]
			if ok {
				common++
			} else {
				extra = append(extra, Word{
					Text: w,
					Pos:  pos,
				})
			}
		}
		for w, pos := range t.Words {
			if _, ok := words[w]; !ok {
				missing = append(missing, Word{
					Text: w,
					Pos:  pos,
				})
			}
		}
		score := 2 * float64(common) / (float64(len(words)) + float64(len(t.Words)))
		if score > bestScore {
			bestScore = score
			bestTemplate = t
			bestMissing = missing
			bestExtra = extra
		}
	}
	return MatchResult{
		Template:     bestTemplate,
		Score:        bestScore,
		ExtraWords:   sortAndReturnWords(bestExtra),
		MissingWords: sortAndReturnWords(bestMissing),
	}
}

func listDependencies(gopath string, pkgs []string) ([]*modinfo.ModulePublic, error) {
	args := []string{"list", "-m", "-json", "all"}
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Env = os.Environ()
	var b bytes.Buffer
	var berr bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &berr
	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("'go %s' failed with:\n%s",
			strings.Join(args, " "), berr.String())
	}

	dec := json.NewDecoder(&b)
	var mods []*modinfo.ModulePublic
	for {
		var mod modinfo.ModulePublic
		if err := dec.Decode(&mod); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("json decode: %s", err)
		}
		mods = append(mods, &mod)
	}
	return mods, nil
}

func isLinkedModule(modulePath string) (bool, error) {
	args := []string{"mod", "why", "-m", "-vendor", modulePath}
	cmd := exec.Command("go", args...)
	cmd.Env = os.Environ()
	var b bytes.Buffer
	var berr bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &berr
	err := cmd.Run()
	if err != nil {
		return false, fmt.Errorf("'go %s' failed with:\n%s",
			strings.Join(args, " "), berr.String())
	}
	return !strings.Contains(b.String(), "(main module does not need"), nil
}

type PkgError struct {
	Err string
}

type PkgInfo struct {
	Name       string
	Dir        string
	Root       string
	ImportPath string
	Error      *PkgError
}

var (
	reLicense = regexp.MustCompile(`(?i)^(?:` +
		`((?:un)?licen[sc]e)|` +
		`((?:un)?licen[sc]e\.(?:md|markdown|txt))|` +
		`(copy(?:ing|right)(?:\.[^.]+)?)|` +
		`(licen[sc]e\.[^.]+)` +
		`)$`)
)

// scoreLicenseName returns a factor between 0 and 1 weighting how likely
// supplied filename is a license file.
func scoreLicenseName(name string) float64 {
	m := reLicense.FindStringSubmatch(name)
	switch {
	case m == nil:
		break
	case m[1] != "":
		return 1.0
	case m[2] != "":
		return 0.9
	case m[3] != "":
		return 0.8
	case m[4] != "":
		return 0.7
	}
	return 0.
}

// findLicense looks for license files in module path. It returns the path and
// score of the best entry, an empty string if none was found.
func findLicense(mod *modinfo.ModulePublic) (string, error) {
	path := mod.Dir
	fis, err := ioutil.ReadDir(path)
	if err != nil {
		return "", err
	}
	bestScore := float64(0)
	bestName := ""
	for _, fi := range fis {
		if !fi.Mode().IsRegular() {
			continue
		}
		score := scoreLicenseName(fi.Name())
		if score > bestScore {
			bestScore = score
			bestName = fi.Name()
		}
	}
	if bestName != "" {
		return filepath.Join(path, bestName), nil
	}
	return "", nil
}

type License struct {
	Package      string
	Score        float64
	Template     *Template
	Path         string
	Err          string
	ExtraWords   []string
	MissingWords []string
}

func listLicenses(gopath string, pkgs []string) ([]License, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	mods, err := listDependencies(gopath, pkgs)
	if err != nil {
		return nil, fmt.Errorf("could not list %s dependencies: %s",
			strings.Join(pkgs, " "), err)
	}
	var linkedMods []*modinfo.ModulePublic

	// filter modules: list only linked packages
	for _, mod := range mods {
		linked, err := isLinkedModule(mod.Path)
		if err != nil {
			return nil, err
		}
		if linked {
			linkedMods = append(linkedMods, mod)
		}
	}

	// Cache matched licenses by path. Useful for package with a lot of
	// subpackages like bleve.
	matched := map[string]MatchResult{}

	licenses := []License{}
	for _, mod := range linkedMods {
		path, err := findLicense(mod)
		if err != nil {
			return nil, err
		}
		license := License{
			Package: mod.Path,
			Path:    path,
		}
		if path != "" {
			fpath := path
			m, ok := matched[fpath]
			if !ok {
				data, err := ioutil.ReadFile(fpath)
				if err != nil {
					log.Println(fpath)
					return nil, err
				}
				m = matchTemplates(data, templates)
				matched[fpath] = m
			}
			license.Score = m.Score
			license.Template = m.Template
			license.ExtraWords = m.ExtraWords
			license.MissingWords = m.MissingWords
		}
		licenses = append(licenses, license)
	}
	return licenses, nil
}

// longestCommonPrefix returns the longest common prefix over import path
// components of supplied licenses.
func longestCommonPrefix(licenses []License) string {
	type Node struct {
		Name     string
		Children map[string]*Node
	}
	// Build a prefix tree. Not super efficient, but easy to do.
	root := &Node{
		Children: map[string]*Node{},
	}
	for _, l := range licenses {
		n := root
		for _, part := range strings.Split(l.Package, "/") {
			c := n.Children[part]
			if c == nil {
				c = &Node{
					Name:     part,
					Children: map[string]*Node{},
				}
				n.Children[part] = c
			}
			n = c
		}
	}
	n := root
	prefix := []string{}
	for {
		if len(n.Children) != 1 {
			break
		}
		for _, c := range n.Children {
			prefix = append(prefix, c.Name)
			n = c
			break
		}
	}
	return strings.Join(prefix, "/")
}

// groupLicenses returns the input licenses after grouping them by license path
// and find their longest import path common prefix. Entries with empty paths
// are left unchanged.
func groupLicenses(licenses []License) ([]License, error) {
	paths := map[string][]License{}
	for _, l := range licenses {
		if l.Path == "" {
			continue
		}
		paths[l.Path] = append(paths[l.Path], l)
	}
	for k, v := range paths {
		if len(v) <= 1 {
			continue
		}
		prefix := longestCommonPrefix(v)
		if prefix == "" {
			return nil, fmt.Errorf(
				"packages share the same license but not common prefix: %v", v)
		}
		l := v[0]
		l.Package = prefix
		paths[k] = []License{l}
	}
	kept := []License{}
	for _, l := range licenses {
		if l.Path == "" {
			kept = append(kept, l)
			continue
		}
		if v, ok := paths[l.Path]; ok {
			kept = append(kept, v[0])
			delete(paths, l.Path)
		}
	}
	return kept, nil
}

func printLicenses() error {
	flag.Usage = func() {
		fmt.Println(`Usage: licenses IMPORTPATH...

licenses lists all dependencies of specified packages or commands, excluding
standard library packages, and prints their licenses. Licenses are detected by
looking for files named like LICENSE, COPYING, COPYRIGHT and other variants in
the package directory, and its parent directories until one is found. Files
content is matched against a set of well-known licenses and the best match is
displayed along with its score.

With -a, all individual packages are displayed instead of grouping them by
license files.
With -w, words in package license file not found in the template license are
displayed. It helps assessing the changes importance.`)
		os.Exit(1)
	}
	all := flag.Bool("a", false, "display all individual packages")
	words := flag.Bool("w", false, "display words not matching license template")
	flag.Parse()
	if flag.NArg() < 1 {
		return fmt.Errorf("expect at least one package argument")
	}
	pkgs := flag.Args()

	confidence := 0.9
	licenses, err := listLicenses("", pkgs)
	if err != nil {
		return err
	}
	if !*all {
		licenses, err = groupLicenses(licenses)
		if err != nil {
			return err
		}
	}
	w := tabwriter.NewWriter(os.Stdout, 1, 4, 2, ' ', 0)
	for _, l := range licenses {
		license := "?"
		if l.Template != nil {
			if l.Score > .99 {
				license = fmt.Sprintf("%s", l.Template.Title)
			} else if l.Score >= confidence {
				license = fmt.Sprintf("%s (%2d%%)", l.Template.Title, int(100*l.Score))
				if *words && len(l.ExtraWords) > 0 {
					license += "\n\t+words: " + strings.Join(l.ExtraWords, ", ")
				}
				if *words && len(l.MissingWords) > 0 {
					license += "\n\t-words: " + strings.Join(l.MissingWords, ", ")
				}
			} else {
				license = fmt.Sprintf("? (%s, %2d%%)", l.Template.Title, int(100*l.Score))
			}
		} else if l.Err != "" {
			license = strings.Replace(l.Err, "\n", " ", -1)
		}
		_, err = w.Write([]byte(l.Package + "\t" + license + "\n"))
		if err != nil {
			return err
		}
	}
	return w.Flush()
}

func main() {
	err := printLicenses()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
}
