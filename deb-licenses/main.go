package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/groove-x/go-licenses/assets"
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

type License struct {
	Package      string
	Score        float64
	Template     *Template
	Path         string
	Err          string
	ExtraWords   []string
	MissingWords []string
}

func listLicenses() ([]License, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, err
	}

	// Cache matched licenses by path. Useful for package with a lot of
	// subpackages like bleve.
	licenses := []License{}
	files, err := ioutil.ReadDir("/usr/share/doc/")
	if err != nil {
		return nil, err
	}
	for _, pkg := range files {
		path := filepath.Join("/usr/share/doc/", pkg.Name(), "copyright")
		license := License{
			Package: pkg.Name(),
			Path:    path,
		}
		data, err := ioutil.ReadFile(path)
		if err == nil {
			m := matchTemplates(data, templates)
			license.Score = m.Score
			license.Template = m.Template
			license.ExtraWords = m.ExtraWords
			license.MissingWords = m.MissingWords
		}
		licenses = append(licenses, license)
	}
	return licenses, nil
}

func printLicenses() error {
	flag.Usage = func() {
		fmt.Println(`Usage: deb-licenses`)
		os.Exit(1)
	}
	words := flag.Bool("w", false, "display words not matching license template")
	flag.Parse()

	confidence := 0.9
	licenses, err := listLicenses()
	if err != nil {
		return err
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
