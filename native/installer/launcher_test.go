package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// install.sh is a launcher only (issue #5, AC8.2): it fetches the repository,
// builds the installer and execs it. Every AWS and Kubernetes call belongs to
// the installer, so the launcher must never run aws, kubectl or eksctl, nor a
// shell or eval that could run them from a string.

// forbiddenCommand matches a forbidden command in command position: at the
// start of a line, or after a control operator, a subshell or substitution
// opener, a keyword that runs its next word, or variable assignments;
// optionally quoted or given as a path. It runs over shellCode's output, so
// comments and single-quoted text never match. Double-quoted text is kept,
// since "$(aws ...)" and "$dir/kubectl" run; prose there that names a
// command right after a ';' or '|' is flagged too, which only costs a
// rewording.
var forbiddenCommand = regexp.MustCompile(`(?m)(?:^|[;&|(!` + "`" + `]|\$\(|\b(?:then|do|else|elif|exec|command|builtin|sudo|env|time|nohup|xargs)[ \t])` +
	`[ \t]*(?:[A-Za-z_][A-Za-z0-9_]*=[^ \t;&|]*[ \t]+)*["']?(?:[^ \t;&|"']*/)?(aws|kubectl|eksctl|eval|bash|sh|zsh)(?:$|[ \t;&|)"'` + "`" + `])`)

// shellCode returns script with comments removed and single-quoted text
// blanked. Line breaks are kept, so offsets map to the script's lines.
//
// shortcut: this is not a full shell parser. It refuses heredocs (their
// bodies would go unchecked), which install.sh has no need for; replace it
// with a real tokenizer if the launcher ever needs one.
func shellCode(script string) (string, error) {
	if strings.Contains(script, "<<") {
		return "", fmt.Errorf("heredoc in the launcher: its body cannot be checked")
	}
	var b strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
				b.WriteByte(c)
			} else if c == '\n' {
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case c == '\\' && i+1 < len(script):
			b.WriteByte(c)
			b.WriteByte(script[i+1])
			i++
		case inDouble:
			inDouble = c != '"'
			b.WriteByte(c)
		case c == '#' && (i == 0 || strings.IndexByte(" \t\n;", script[i-1]) >= 0):
			for i+1 < len(script) && script[i+1] != '\n' {
				i++
			}
		case c == '\'':
			inSingle = true
			b.WriteByte(c)
		case c == '"':
			inDouble = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// launcherCalls lists every forbidden command the script would run, by line.
func launcherCalls(script string) ([]string, error) {
	code, err := shellCode(script)
	if err != nil {
		return nil, err
	}
	var out []string
	lines := strings.Split(script, "\n")
	for _, loc := range forbiddenCommand.FindAllStringSubmatchIndex(code, -1) {
		n := strings.Count(code[:loc[2]], "\n")
		out = append(out, fmt.Sprintf("line %d: %s", n+1, strings.TrimSpace(lines[n])))
	}
	return out, nil
}

func TestLauncherRunsNoAWSOrKubectlCommand(t *testing.T) {
	b, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	calls, err := launcherCalls(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) > 0 {
		t.Errorf("install.sh must only launch the installer, but runs:\n%s", strings.Join(calls, "\n"))
	}
}

// The check is proved both ways: text that only mentions the commands
// passes, and each way of running one is caught, on the line it is on.
func TestLauncherCheckAcceptsMentionsAndRejectsInvocations(t *testing.T) {
	accept := strings.Join([]string{
		`#!/usr/bin/env bash`,
		`# the installer runs kubectl and aws; never this script`,
		`echo "the doctor checks aws, kubectl and eksctl, not this script"`,
		`printf '%s\n' 'kubectl apply; aws s3 ls | eksctl create'`,
		`aws_region="${AWS_REGION:-us-west-2}"`,
		`kubectl_version=1.37 # ; kubectl`,
		`if ! command -v git >/dev/null 2>&1; then exit 1; fi`,
		`echo "a # is not a comment here"`,
		`exec "${native_dir}/bin/substrate-eks" "$@"`,
		`echo "${awscli:-none} kubectl-free ${#x}"`,
	}, "\n")
	if calls, err := launcherCalls(accept); err != nil || len(calls) > 0 {
		t.Errorf("flagged a script that runs none of them: %q %v", calls, err)
	}
	for _, line := range []string{
		`kubectl apply -f x.yaml`,
		`    aws sts get-caller-identity`,
		`make build && eksctl create cluster`,
		`ok || kubectl get ns`,
		`id="$(aws sts get-caller-identity)"`,
		"id=`aws sts get-caller-identity`",
		`exec kubectl get pods`,
		`AWS_PROFILE=p aws s3 ls`,
		`if true; then kubectl get ns; fi`,
		`/usr/local/bin/aws --version`,
		`"$HOME/bin/kubectl" version`,
		`cat x | kubectl apply -f -`,
		`( eksctl get clusters )`,
		`! aws s3 ls`,
		`env AWS_REGION=x aws ec2 describe-vpcs`,
		`command kubectl version`,
		`echo "\"quoted\"" ; aws s3 ls`,
		`bash -c 'kubectl apply -f x'`,
		`eval "$cmd"`,
		`x=1 sh -c "$cmd"`,
	} {
		calls, err := launcherCalls("#!/usr/bin/env bash\nset -e\n" + line + "\n")
		if err != nil || len(calls) != 1 || !strings.HasPrefix(calls[0], "line 3: ") {
			t.Errorf("%q: calls = %q, err = %v; want one call on line 3", line, calls, err)
		}
	}
	if _, err := launcherCalls("cat <<EOF\nkubectl apply\nEOF\n"); err == nil {
		t.Error("a heredoc was accepted unchecked")
	}
}
