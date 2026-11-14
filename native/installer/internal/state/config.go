package state

// eks-talk's config.env holds the defaults every install uses, and
// config.local.env (gitignored) what a person changed. The wizard reads both,
// as lib/common.sh does, so the native installer and the scripts share one
// configuration; its Settings screen writes config.local.env, as eks-talk's
// first installer did. This is that installer's config package, cut to the
// keys the Settings screen edits.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

// ConfigEntry is one KEY=VALUE line and the comment block right above it.
type ConfigEntry struct {
	Key, Value string
	Doc        string // the comment lines above the key, without "# "
}

// ConfigFile is a parsed config file, in file order.
type ConfigFile struct{ Entries []ConfigEntry }

// Get returns the value of key and whether the file sets it (the last
// assignment wins, as in bash).
func (f ConfigFile) Get(key string) (string, bool) {
	for i := len(f.Entries) - 1; i >= 0; i-- {
		if f.Entries[i].Key == key {
			return f.Entries[i].Value, true
		}
	}
	return "", false
}

// Doc returns the comment block above key.
func (f ConfigFile) Doc(key string) string {
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

// ParseConfig reads a bash KEY=VALUE file. It understands what eks-talk's
// files use: bare words, "double quoted" and 'single quoted' values, a
// trailing " # comment", and comment lines. Anything else (a command, an
// expansion) is an error, so the installer never misreads a file.
func ParseConfig(r io.Reader) (ConfigFile, error) {
	var f ConfigFile
	var doc []string
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			doc = nil
			continue
		case strings.HasPrefix(line, "#"):
			d := strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(line, "#"), " "), " ")
			if sectionRe.MatchString(d) {
				doc = nil // "--- Cluster ---" heads a section, not a key
			} else {
				doc = append(doc, d)
			}
			continue
		}
		m := keyRe.FindStringSubmatch(strings.TrimPrefix(line, "export "))
		if m == nil {
			return f, fmt.Errorf("line %d: not KEY=VALUE: %q", n, line)
		}
		v, err := unquote(m[2])
		if err != nil {
			return f, fmt.Errorf("line %d (%s): %w", n, m[1], err)
		}
		f.Entries = append(f.Entries, ConfigEntry{Key: m[1], Value: v, Doc: strings.Join(doc, "\n")})
		doc = nil
	}
	return f, sc.Err()
}

func unquote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	rest := func(r string) error {
		if r = strings.TrimSpace(r); r != "" && !strings.HasPrefix(r, "#") {
			return fmt.Errorf("text after the closing quote: %q", r)
		}
		return nil
	}
	switch s[0] {
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			switch c := s[i]; {
			case c == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0:
				b.WriteByte(s[i+1])
				i++
			case c == '"':
				return b.String(), rest(s[i+1:])
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
		return s[1 : end+1], rest(s[end+2:])
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if strings.ContainsAny(s, " \t$`\"'\\;&|<>()") {
		return "", fmt.Errorf("unquoted value %q needs quotes", s)
	}
	return s, nil
}

func parseConfigFile(path string) (ConfigFile, error) {
	fh, err := os.Open(path)
	if os.IsNotExist(err) {
		return ConfigFile{}, nil
	}
	if err != nil {
		return ConfigFile{}, err
	}
	defer fh.Close()
	f, err := ParseConfig(fh)
	if err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Config is an eks-talk checkout's config.env and config.local.env, under
// the environment: Lookup answers as lib/common.sh's scripts see a key.
type Config struct {
	// Dir is the checkout; empty when the installer runs outside one, and
	// then Lookup reads the environment alone and nothing is written.
	Dir             string
	Defaults, Local ConfigFile
	// Env is the environment (os.LookupEnv); nil reads none.
	Env func(string) (string, bool)
}

// FindConfig returns the eks-talk checkout above any of starts: the first
// directory with a config.env next to native/tools/setup-aws. It returns a
// Config with no Dir when there is none.
func FindConfig(env func(string) (string, bool), starts ...string) (Config, error) {
	for _, start := range starts {
		for dir := start; dir != "" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, "native", "tools", "setup-aws", "go.mod")); err != nil {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, "config.env")); err != nil {
				continue
			}
			return LoadConfig(dir, env)
		}
	}
	return Config{Env: env}, nil
}

// LoadConfig reads DIR/config.env and DIR/config.local.env.
func LoadConfig(dir string, env func(string) (string, bool)) (Config, error) {
	c := Config{Dir: dir, Env: env}
	var err error
	if c.Defaults, err = parseConfigFile(filepath.Join(dir, "config.env")); err != nil {
		return c, err
	}
	if c.Local, err = parseConfigFile(filepath.Join(dir, "config.local.env")); err != nil {
		return c, err
	}
	return c, nil
}

// Lookup is key from the environment, else config.local.env, else config.env.
func (c Config) Lookup(key string) (string, bool) {
	if c.Env != nil {
		if v, ok := c.Env(key); ok {
			return v, true
		}
	}
	if v, ok := c.Local.Get(key); ok {
		return v, true
	}
	return c.Defaults.Get(key)
}

// LocalPath is DIR/config.local.env, or "" outside a checkout.
func (c Config) LocalPath() string {
	if c.Dir == "" {
		return ""
	}
	return filepath.Join(c.Dir, "config.local.env")
}

// ManagedKeys are the keys the Settings screen edits, in the order
// config.local.env lists them. The cluster, Region and profile are always
// written, so the file alone says which deployment it is for.
var ManagedKeys = []string{
	"CLUSTER_NAME", "AWS_REGION", "AWS_PROFILE", "KUBERNETES_VERSION", "AUTO_MODE",
	"SYSTEM_INSTANCE_TYPE", "SYSTEM_NODES", "GVISOR_INSTANCE_TYPE", "GVISOR_NODES",
	"MICROVM_INSTANCE_TYPE", "MICROVM_NODES", "TAGS", "EKS_PUBLIC_ACCESS_CIDRS",
	"SUBSTRATE_TAG", "SUBSTRATE_IMAGE_REPO",
}

var alwaysWritten = map[string]bool{"CLUSTER_NAME": true, "AWS_REGION": true, "AWS_PROFILE": true}

// ConfigValues are s's answers under ManagedKeys.
func ConfigValues(s *Setup) map[string]string {
	v := map[string]string{"CLUSTER_NAME": s.ClusterName, "AWS_REGION": s.Region, "AWS_PROFILE": s.Profile,
		"TAGS": s.Tags, "SUBSTRATE_TAG": s.SubstrateTag, "SUBSTRATE_IMAGE_REPO": s.ImageRepo}
	for _, kv := range s.NewClusterConfig.Env() {
		k, val, _ := strings.Cut(kv, "=")
		v[k] = val
	}
	return v
}

var bareRe = regexp.MustCompile(`^[A-Za-z0-9_.:/+@,=-]+$`)

// quote formats a value for bash: bare when it is a plain word, else in
// double quotes with ", \, $ and ` escaped.
func quote(v string) string {
	if bareRe.MatchString(v) {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(v) + `"`
}

// RenderLocal is the config.local.env text for values: each managed key that
// differs from config.env (and the cluster, Region and profile), then every
// other key the existing config.local.env set, as it was.
func (c Config) RenderLocal(values map[string]string) (string, error) {
	var b strings.Builder
	b.WriteString("# Written by the substrate-eks installer (Settings). The scripts and the\n" +
		"# installer read it after config.env; edit it, or run the installer again.\n\n")
	managed := map[string]bool{}
	for _, k := range ManagedKeys {
		managed[k] = true
		v, ok := values[k]
		if !ok {
			continue
		}
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("%s: a value cannot hold a line break", k)
		}
		if def, _ := c.Defaults.Get(k); v == def && !alwaysWritten[k] {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, quote(v))
	}
	var kept []ConfigEntry
	seen := map[string]bool{}
	for i := len(c.Local.Entries) - 1; i >= 0; i-- {
		if e := c.Local.Entries[i]; !managed[e.Key] && !seen[e.Key] {
			seen[e.Key] = true
			kept = append([]ConfigEntry{e}, kept...)
		}
	}
	if len(kept) > 0 {
		b.WriteString("\n# Kept from the previous config.local.env\n")
		for _, e := range kept {
			fmt.Fprintf(&b, "%s=%s\n", e.Key, quote(e.Value))
		}
	}
	return b.String(), nil
}

// WriteLocal writes text as config.local.env with mode 0600, through a
// temporary file so a failed write never leaves half a file. A different
// existing file is copied first to backup (0600, in a 0700 directory).
func (c Config) WriteLocal(text, backup string) error {
	path := c.LocalPath()
	if path == "" {
		return fmt.Errorf("no eks-talk checkout: config.local.env has nowhere to go")
	}
	if old, err := os.ReadFile(path); err == nil {
		if string(old) == text {
			return os.Chmod(path, 0o600)
		}
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(backup, old, 0o600); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	// WriteFile keeps the mode of a .tmp left by an earlier crash.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var (
	versionRe = regexp.MustCompile(`^1\.([0-9]{2})$`)
	typeRe    = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z0-9]+$`)
	nodesRe   = regexp.MustCompile(`^[1-9][0-9]?$`)
	tagRe     = regexp.MustCompile(`^[A-Za-z0-9_.:/+@-]+=[A-Za-z0-9_.:/+@-]*$`)
	cidrRe    = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$`)
	releaseRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`)
	imageRe   = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+$`)
)

// ValidateConfig returns a message for each of values' managed keys that
// setup-aws or the control plane would reject. The cluster name, Region and
// profile are chosen on their own screens and checked there.
func ValidateConfig(values map[string]string) map[string]string {
	bad := map[string]string{}
	check := func(k string, ok bool, msg string) {
		if !ok {
			bad[k] = msg
		}
	}
	minor := 0
	if m := versionRe.FindStringSubmatch(values["KUBERNETES_VERSION"]); m != nil {
		minor, _ = strconv.Atoi(m[1])
	}
	check("KUBERNETES_VERSION", minor >= awsx.MinMinor,
		fmt.Sprintf("an EKS version 1.%d or newer, such as 1.%d", awsx.MinMinor, awsx.MinMinor))
	check("AUTO_MODE", values["AUTO_MODE"] == "on" || values["AUTO_MODE"] == "", "on, or empty for off")
	for _, g := range []string{"SYSTEM", "GVISOR", "MICROVM"} {
		check(g+"_INSTANCE_TYPE", typeRe.MatchString(values[g+"_INSTANCE_TYPE"]), "an EC2 instance type such as m8i.2xlarge")
		check(g+"_NODES", nodesRe.MatchString(values[g+"_NODES"]), "a node count from 1 to 99")
	}
	for _, kv := range strings.Fields(values["TAGS"]) {
		check("TAGS", tagRe.MatchString(kv), "space-separated key=value pairs (letters, digits and _ . : / + @ -)")
	}
	cidrs := strings.Fields(values["EKS_PUBLIC_ACCESS_CIDRS"])
	check("EKS_PUBLIC_ACCESS_CIDRS", len(cidrs) > 0, "at least one CIDR (0.0.0.0/0 is any address)")
	for _, c := range cidrs {
		check("EKS_PUBLIC_ACCESS_CIDRS", cidrRe.MatchString(c), "space-separated IPv4 CIDRs such as 203.0.113.0/24")
	}
	check("SUBSTRATE_TAG", releaseRe.MatchString(values["SUBSTRATE_TAG"]), "a release tag such as v1.0.0")
	check("SUBSTRATE_IMAGE_REPO", imageRe.MatchString(values["SUBSTRATE_IMAGE_REPO"]), "an image repository such as ghcr.io/agent-substrate/substrate")
	return bad
}
