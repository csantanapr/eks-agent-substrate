// Package config reads eks-talk's config.env (the defaults) and an existing
// config.local.env, and writes the config.local.env the installer chose.
//
// The files are bash: KEY=VALUE lines, values bare or in single or double
// quotes, comments on their own lines. lib/common.sh sources config.env and
// then config.local.env, so a key in config.local.env wins.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Entry is one KEY=VALUE line and the comment block right above it.
type Entry struct {
	Key   string
	Value string
	Doc   string // the comment lines above the key, without "# "
}

// File is a parsed config file, in file order.
type File struct {
	Entries []Entry
}

// Get returns the value of key and whether the file sets it.
func (f File) Get(key string) (string, bool) {
	for i := len(f.Entries) - 1; i >= 0; i-- {
		if f.Entries[i].Key == key {
			return f.Entries[i].Value, true
		}
	}
	return "", false
}

// Doc returns the comment block above key.
func (f File) Doc(key string) string {
	for _, e := range f.Entries {
		if e.Key == key {
			return e.Doc
		}
	}
	return ""
}

var (
	keyRe     = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	sectionRe = regexp.MustCompile(`^-{3} .* -{3}$`)
)

// Parse reads a bash KEY=VALUE file. It understands what eks-talk's files use:
// bare words, "double quoted" and 'single quoted' values, a trailing
// " # comment" after a bare or quoted value, and comment lines. Anything else
// (a command, a function) is an error, so the installer never misreads a file.
func Parse(r io.Reader) (File, error) {
	var f File
	var doc []string
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			line = strings.TrimLeft(raw, " \t")
		}
		switch {
		case line == "":
			doc = nil
			continue
		case strings.HasPrefix(line, "#"):
			d := strings.TrimPrefix(strings.TrimPrefix(line, "#"), " ")
			if sectionRe.MatchString(d) {
				doc = nil // "--- Cluster ---" heads a section, not a key
				continue
			}
			doc = append(doc, strings.TrimRight(d, " "))
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		m := keyRe.FindStringSubmatch(line)
		if m == nil {
			return f, fmt.Errorf("line %d: not KEY=VALUE: %q", n, line)
		}
		v, err := unquote(m[2])
		if err != nil {
			return f, fmt.Errorf("line %d (%s): %w", n, m[1], err)
		}
		f.Entries = append(f.Entries, Entry{Key: m[1], Value: v, Doc: strings.Join(doc, "\n")})
		doc = nil
	}
	return f, sc.Err()
}

func unquote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	switch s[0] {
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			c := s[i]
			switch {
			case c == '\\' && i+1 < len(s) && strings.ContainsRune(`"\$`+"`", rune(s[i+1])):
				b.WriteByte(s[i+1])
				i++
			case c == '"':
				if rest := strings.TrimSpace(s[i+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
					return "", fmt.Errorf("text after the closing quote: %q", rest)
				}
				return b.String(), nil
			case c == '$' || c == '`':
				return "", fmt.Errorf("shell expansion in %q is not supported", s)
			default:
				b.WriteByte(c)
			}
		}
		return "", fmt.Errorf("no closing double quote in %q", s)
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("no closing single quote in %q", s)
		}
		if rest := strings.TrimSpace(s[end+2:]); rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("text after the closing quote: %q", rest)
		}
		return s[1 : end+1], nil
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if strings.ContainsAny(s, " \t$`\"'\\;&|<>()") {
		return "", fmt.Errorf("unquoted value %q needs quotes", s)
	}
	return s, nil
}

// ParseFile parses path; a missing file is an empty File.
func ParseFile(path string) (File, error) {
	fh, err := os.Open(path)
	if os.IsNotExist(err) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	defer fh.Close()
	f, err := Parse(fh)
	if err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Managed lists the keys the installer asks about, in the order it writes them.
var Managed = []string{
	"CLUSTER_NAME", "AWS_REGION", "AWS_PROFILE", "KUBERNETES_VERSION", "AUTO_MODE",
	"SYSTEM_INSTANCE_TYPE", "SYSTEM_NODES",
	"GVISOR_INSTANCE_TYPE", "GVISOR_NODES",
	"MICROVM_INSTANCE_TYPE", "MICROVM_NODES",
	"TAGS", "EKS_PUBLIC_ACCESS_CIDRS", "SUBSTRATE_TAG",
	// Set by the OpenClaw choice on the Settings screen.
	"OPENCLAW_DIR", "OPENCLAW_REPO", "OPENCLAW_REF",
	// Set by the mode 3 choices.
	"MODE3", "MODE3_ISO",
}

// Always lists the managed keys written even when they equal config.env, so
// config.local.env alone says which cluster, region and profile it is for.
var Always = map[string]bool{"CLUSTER_NAME": true, "AWS_REGION": true, "AWS_PROFILE": true}

// Repo is an eks-talk checkout: its config.env defaults and any existing
// config.local.env.
type Repo struct {
	Dir      string
	Defaults File
	Local    File
}

// LoadRepo reads DIR/config.env (required) and DIR/config.local.env.
func LoadRepo(dir string) (Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Repo{}, err
	}
	r := Repo{Dir: abs}
	for _, f := range []string{"config.env", "create-cluster.sh", "deploy.sh", "lib/common.sh"} {
		if _, err := os.Stat(filepath.Join(abs, f)); err != nil {
			return r, fmt.Errorf("%s is not an eks-talk checkout: no %s", abs, f)
		}
	}
	if r.Defaults, err = ParseFile(filepath.Join(abs, "config.env")); err != nil {
		return r, err
	}
	if r.Local, err = ParseFile(filepath.Join(abs, "config.local.env")); err != nil {
		return r, err
	}
	return r, nil
}

// Effective returns the value the scripts would use for key: config.local.env
// over config.env.
func (r Repo) Effective(key string) string {
	if v, ok := r.Local.Get(key); ok {
		return v
	}
	v, _ := r.Defaults.Get(key)
	return v
}

// LocalPath is DIR/config.local.env.
func (r Repo) LocalPath() string { return filepath.Join(r.Dir, "config.local.env") }

// Quote formats a value for bash: bare when it is a plain word, else in
// double quotes with ", \, $ and ` escaped.
func Quote(v string) string {
	if v != "" && regexp.MustCompile(`^[A-Za-z0-9_.:/+@,=-]+$`).MatchString(v) {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return `"` + r.Replace(v) + `"`
}

// Render returns the config.local.env text for values: the managed keys that
// differ from config.env (and the Always keys), then every other key the
// existing config.local.env set, kept as it was. header is written as
// comment lines at the top.
func Render(defaults, existing File, values map[string]string, header []string) (string, error) {
	var b strings.Builder
	for _, h := range header {
		b.WriteString(strings.TrimRight("# "+h, " ") + "\n")
	}
	if len(header) > 0 {
		b.WriteString("\n")
	}
	managed := map[string]bool{}
	for _, k := range Managed {
		managed[k] = true
		v, ok := values[k]
		if !ok {
			continue
		}
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("%s: a value cannot hold a line break", k)
		}
		def, _ := defaults.Get(k)
		if v == def && !Always[k] {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, Quote(v))
	}
	// Keys outside the installer's questions (WHATSAPP_PEER, DB_*, ...) that
	// the existing file set stay as they were.
	var kept []Entry
	seen := map[string]bool{}
	for i := len(existing.Entries) - 1; i >= 0; i-- {
		e := existing.Entries[i]
		if managed[e.Key] || seen[e.Key] {
			continue
		}
		seen[e.Key] = true
		kept = append(kept, e)
	}
	if len(kept) > 0 {
		sort.SliceStable(kept, func(i, j int) bool { return indexOf(existing, kept[i].Key) < indexOf(existing, kept[j].Key) })
		b.WriteString("\n# Kept from the previous config.local.env\n")
		for _, e := range kept {
			fmt.Fprintf(&b, "%s=%s\n", e.Key, Quote(e.Value))
		}
	}
	return b.String(), nil
}

func indexOf(f File, key string) int {
	for i, e := range f.Entries {
		if e.Key == key {
			return i
		}
	}
	return -1
}

// Write writes text to path with mode 0600, through a temporary file so a
// failed write never leaves half a file. A different existing file is kept
// as backup first (in the gitignored .work/, so the checkout stays clean).
func Write(path, text, backup string) error {
	if _, err := os.Stat(path); err == nil {
		old, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(old) == text {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(backup, old, 0o600); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validation of the values the installer asks for. lib/common.sh checks the
// same rules again when a script starts.
var (
	clusterRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,26}[a-z0-9]$`)
	regionRe  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	tagRe     = regexp.MustCompile(`^[A-Za-z0-9_.:/+@-]+=[A-Za-z0-9_.:/+@-]*$`)
	typeRe    = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z0-9]+$`)
	versionRe = regexp.MustCompile(`^1\.[0-9]{2}$`)
	profileRe = regexp.MustCompile(`^[A-Za-z0-9_.+@-]*$`)
	cidrRe    = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$`)
	stageRe   = regexp.MustCompile(`^(latest|v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?)$`)
)

// Validate returns a message for each value that the scripts would reject.
func Validate(values map[string]string) map[string]string {
	bad := map[string]string{}
	check := func(k string, ok bool, msg string) {
		if !ok {
			bad[k] = msg
		}
	}
	check("CLUSTER_NAME", clusterRe.MatchString(values["CLUSTER_NAME"]), "2-28 characters of a-z, 0-9 and '-', starting with a letter")
	check("AWS_REGION", regionRe.MatchString(values["AWS_REGION"]), "a Region name such as us-west-2")
	check("AWS_PROFILE", profileRe.MatchString(values["AWS_PROFILE"]), "a profile name from ~/.aws/config, or empty")
	check("KUBERNETES_VERSION", versionRe.MatchString(values["KUBERNETES_VERSION"]), "a version such as 1.37")
	check("AUTO_MODE", values["AUTO_MODE"] == "on" || values["AUTO_MODE"] == "", "on, or empty for off")
	for _, g := range []string{"SYSTEM", "GVISOR", "MICROVM"} {
		check(g+"_INSTANCE_TYPE", typeRe.MatchString(values[g+"_INSTANCE_TYPE"]), "an EC2 instance type such as m8i.2xlarge")
		n := values[g+"_NODES"]
		ok := regexp.MustCompile(`^[0-9]{1,2}$`).MatchString(n) && n != "0" && n != "00"
		check(g+"_NODES", ok, "a node count from 1 to 99")
	}
	for _, kv := range strings.Fields(values["TAGS"]) {
		if !tagRe.MatchString(kv) {
			bad["TAGS"] = "space-separated key=value pairs (letters, digits and _ . : / + @ -)"
		}
	}
	for _, c := range strings.Fields(values["EKS_PUBLIC_ACCESS_CIDRS"]) {
		if !cidrRe.MatchString(c) {
			bad["EKS_PUBLIC_ACCESS_CIDRS"] = "space-separated IPv4 CIDRs such as 203.0.113.0/24"
		}
	}
	if strings.TrimSpace(values["EKS_PUBLIC_ACCESS_CIDRS"]) == "" {
		bad["EKS_PUBLIC_ACCESS_CIDRS"] = "at least one CIDR (0.0.0.0/0 is any address)"
	}
	check("SUBSTRATE_TAG", stageRe.MatchString(values["SUBSTRATE_TAG"]), "a release tag such as v1.0.0, or latest")
	return bad
}
