package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// header is substrate-gke's brand bar (chrome.go headerView) with what this
// run acts on, on every screen: cluster · Region · profile · account, with
// the account masked, and a clock.
func (m Model) header() string {
	w := m.width()
	left := theme.Title.Render(" AGENT SUBSTRATE ") + theme.Subtle.Render("· EKS installer")
	cluster := m.s.ClusterName
	if cluster == "" {
		cluster = "no cluster yet"
	}
	profile := m.s.Profile
	if profile == "" {
		profile = "default credentials"
	}
	target := []string{theme.Accent.Render(cluster), theme.Subtle.Render(m.s.Region), theme.Subtle.Render(profile)}
	if m.s.Account != "" {
		target = append(target, theme.Subtle.Render(maskAccount(m.s.Account)))
	}
	var badges []string
	if m.d.DryRun {
		badges = append(badges, theme.Warning.Render("DRY-RUN"))
	}
	badges = append(badges, strings.Join(target, theme.Fainted.Render(" · ")), theme.Subtle.Render(clock(m.d.Now())))
	right := strings.Join(badges, theme.Fainted.Render("  │  "))
	if lipgloss.Width(left)+lipgloss.Width(right)+2 > w {
		left = theme.Title.Render(" AGENT SUBSTRATE ")
	}
	gap := max(w-lipgloss.Width(left)-lipgloss.Width(right)-1, 1)
	line := lipgloss.NewStyle().MaxWidth(w).Render(left + strings.Repeat(" ", gap) + right)
	return line + "\n" + theme.Fainted.Render(strings.Repeat("─", w))
}

// clock is the wall clock in the user's own time zone, 12-hour.
func clock(t time.Time) string { return t.Format("3:04 PM MST") }

// tildify writes a path for the screen without the user's name in it: from
// the working directory when it is near (../config.local.env), else under
// the home directory as ~/..., whichever way the home is spelled.
func tildify(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	if cwd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(r, "../..") {
			return r
		}
	}
	return homeTilde(p)
}

// homeTilde writes a path under the home directory as ~/..., which a shell
// expands in a command such as export KUBECONFIG=~/...
func homeTilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	homes := []string{home}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		homes = append(homes, real)
	}
	for _, h := range homes {
		if r, err := filepath.Rel(h, p); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.Join("~", r)
		}
	}
	return p
}

// What --redact masks beyond maskIDs. Each is replaced by bullets of its own
// width, so a redacted screen keeps its layout.
var (
	// maskedAccount is an account id after maskIDs.
	maskedAccount = regexp.MustCompile(`•{8}[0-9]{4}`)
	// arnPattern is an ARN; its account and resource name are masked, its
	// partition, service and resource type kept.
	arnPattern = regexp.MustCompile(`arn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:[^:\s\x1b]*:[^\s"',;)\x1b]+`)
	// s3URL is a bucket in an s3:// URL; bucketName a bucket setup-aws
	// derives (<cluster>-snap|assets-<account>-<region>).
	s3URL      = regexp.MustCompile(`s3://[a-z0-9][a-z0-9.-]*[a-z0-9]`)
	bucketName = regexp.MustCompile(`[a-z0-9][a-z0-9.-]*-(?:snap|assets)-•{12}-[a-z]{2}(?:-[a-z]+)+-[0-9]`)
	// phoneNumber is an E.164 number, + and 7 to 15 digits (WHATSAPP_PEER,
	// in a plugin's output); its digits are masked, its + kept.
	phoneNumber = regexp.MustCompile(`\+[1-9][0-9]{6,14}\b`)
)

func bullets(s string) string { return strings.Repeat("•", utf8.RuneCountInString(s)) }

// redact masks, with --redact, every account id whole, the account and
// resource name of every ARN, every bucket name and every phone number. It
// runs after maskIDs.
func (m Model) redact(s string) string {
	if !m.d.Redact {
		return s
	}
	s = maskedAccount.ReplaceAllStringFunc(s, bullets)
	s = arnPattern.ReplaceAllStringFunc(s, func(arn string) string {
		p := strings.SplitN(arn, ":", 6)
		p[4] = bullets(p[4])
		if i := strings.IndexByte(p[5], '/'); i >= 0 {
			p[5] = p[5][:i+1] + bullets(p[5][i+1:])
		} else {
			p[5] = bullets(p[5])
		}
		return strings.Join(p, ":")
	})
	s = s3URL.ReplaceAllStringFunc(s, func(u string) string { return "s3://" + bullets(u[len("s3://"):]) })
	s = phoneNumber.ReplaceAllStringFunc(s, func(n string) string { return "+" + bullets(n[1:]) })
	return bucketName.ReplaceAllStringFunc(s, bullets)
}
