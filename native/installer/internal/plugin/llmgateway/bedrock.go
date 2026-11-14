package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/awsload"
)

// ModelNames are the names actors ask the gateway for: "claude" is the
// default profile, each other the newest ACTIVE profile of its family with
// the default's prefix (us., eu., global.). setup-aws's
// plugin_llm_gateway.go picks the same ones for the IAM policy, from the same
// list, with the same rule.
var ModelNames = []string{"claude", "claude-haiku", "claude-sonnet", "claude-opus"}

// Profile is one entry of Bedrock's ListInferenceProfiles.
type Profile struct {
	ID     string `json:"inferenceProfileId"`
	Status string `json:"status"`
}

// version reads the version after "<prefix>.anthropic.claude-<family>-": the
// minor version has at most two digits, so a date such as
// claude-sonnet-4-20250514 reads as 4.0.
var version = regexp.MustCompile(`^([0-9]+)(?:-([0-9]{1,2}))?(?:-|$)`)

// Newest is the ACTIVE profile with id prefix p and the highest version; a
// tie goes to the greater ID, so the choice does not depend on list order.
func Newest(ps []Profile, p string) (string, bool) {
	best, bestMaj, bestMin, found := "", 0, 0, false
	for _, x := range ps {
		rest, ok := strings.CutPrefix(x.ID, p)
		if x.Status != "ACTIVE" || !ok {
			continue
		}
		m := version.FindStringSubmatch(rest)
		if m == nil {
			continue
		}
		maj, _ := strconv.Atoi(m[1])
		mnr := 0
		if m[2] != "" {
			mnr, _ = strconv.Atoi(m[2])
		}
		if !found || maj > bestMaj || (maj == bestMaj && (mnr > bestMin || (mnr == bestMin && x.ID > best))) {
			best, bestMaj, bestMin, found = x.ID, maj, mnr, true
		}
	}
	return best, found
}

// Models maps each model name to its profile ID: "claude" to def (which must
// be listed), the others to the newest of their family.
func Models(ps []Profile, def string) (map[string]string, error) {
	prefix, _, ok := strings.Cut(def, ".")
	if !ok {
		return nil, fmt.Errorf("BEDROCK_INFERENCE_PROFILE %q has no Region prefix (us., eu., global.)", def)
	}
	out := map[string]string{}
	for _, p := range ps {
		if p.ID == def {
			out["claude"] = def
		}
	}
	if out["claude"] == "" {
		return nil, fmt.Errorf("no inference profile %s in this Region", def)
	}
	for _, n := range ModelNames[1:] {
		id, ok := Newest(ps, prefix+".anthropic."+n+"-")
		if !ok {
			return nil, fmt.Errorf("no active %s. Claude %s inference profile in this Region", prefix, strings.TrimPrefix(n, "claude-"))
		}
		out[n] = id
	}
	return out, nil
}

// emptySHA256 is the payload hash of a request with no body.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// bedrockProfiles lists the Region's inference profiles. The installer has
// no Bedrock SDK module, so it signs the REST call with the SDK's SigV4
// signer and the profile's credential chain; the credentials stay in memory.
//
// shortcut: the endpoint is the aws partition's (bedrock.<region>.amazonaws.com);
// add the other partitions' DNS suffixes if they are ever supported.
func bedrockProfiles(ctx context.Context, profile, region string) ([]Profile, error) {
	ac, err := awsload.Config(ctx, region, profile)
	if err != nil {
		return nil, err
	}
	var out []Profile
	next := ""
	for {
		q := url.Values{"maxResults": {"1000"}}
		if next != "" {
			q.Set("nextToken", next)
		}
		creds, err := ac.Credentials.Retrieve(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://bedrock."+region+".amazonaws.com/inference-profiles?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if err := v4.NewSigner().SignHTTP(ctx, creds, req, emptySHA256, "bedrock", region, time.Now()); err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("bedrock:ListInferenceProfiles: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		var page struct {
			Summaries []Profile `json:"inferenceProfileSummaries"`
			Next      string    `json:"nextToken"`
			Message   string    `json:"message"`
		}
		_ = json.Unmarshal(body, &page)
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("bedrock:ListInferenceProfiles: %s: %s", resp.Status, page.Message)
		}
		out = append(out, page.Summaries...)
		if next = page.Next; next == "" {
			return out, nil
		}
	}
}
