package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Prompter asks plain-text questions. With Auto set, it never reads: each
// question takes its default, and the answer is shown, so --yes and --plan
// print the same questions a person would see.
type Prompter struct {
	In   *bufio.Reader
	Out  io.Writer
	Auto bool
}

// ErrNoAnswer means the input ended before a question was answered.
var ErrNoAnswer = errors.New("no answer (input ended). Answer the questions, or use --yes to take the defaults")

// Ask shows a question with its default and returns the answer, or the
// default for an empty line. check, if given, refuses bad answers: a person
// is asked again, while Auto mode stops with the reason.
func (p *Prompter) Ask(question, def string, check func(string) error) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(p.Out, "%s [%s] ", question, def)
		} else {
			fmt.Fprintf(p.Out, "%s ", question)
		}
		var ans string
		if p.Auto {
			fmt.Fprintln(p.Out, def)
			ans = def
		} else {
			line, err := p.In.ReadString('\n')
			if err != nil && (err != io.EOF || line == "") {
				fmt.Fprintln(p.Out)
				return "", ErrNoAnswer
			}
			ans = strings.TrimSpace(line)
			if ans == "" {
				ans = def
			}
		}
		if check == nil {
			return ans, nil
		}
		err := check(ans)
		if err == nil {
			return ans, nil
		}
		if p.Auto {
			return "", err
		}
		fmt.Fprintln(p.Out, "  "+err.Error())
	}
}

// YesNo asks a yes-or-no question.
func (p *Prompter) YesNo(question string, def bool) (bool, error) {
	d := "n"
	if def {
		d = "y"
	}
	var out bool
	_, err := p.Ask(question+" (y/n)", d, func(s string) error {
		switch strings.ToLower(s) {
		case "y", "yes":
			out = true
		case "n", "no":
			out = false
		default:
			return errors.New("Answer y or n.")
		}
		return nil
	})
	return out, err
}
