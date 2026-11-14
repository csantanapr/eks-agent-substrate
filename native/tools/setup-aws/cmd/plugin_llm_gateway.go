package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// The llm-gateway plugin's AWS half, its IAM part: one role, assumed
// through EKS Pod Identity by llm-gateway/llm-gateway, allowed
// to invoke only the inference profiles the gateway serves. Each
// foundation-model grant is conditioned on its own profile, so the role
// cannot call a model directly or any other model. The installer's
// llmgateway package deploys the gateway and picks the same profiles.
const (
	llmNamespace      = "llm-gateway"
	llmServiceAccount = "llm-gateway"
	// llmDefaultProfile is BEDROCK_INFERENCE_PROFILE's default (config.env),
	// the model "claude". The installer reads the same variable.
	llmDefaultProfile = "us.anthropic.claude-haiku-5-5"
)

func init() {
	registerPlugin(pluginDef{ID: "llm-gateway",
		Short: "Create the LLM gateway's Bedrock IAM role and its Pod Identity association",
		Create: func(ctx context.Context, e *env) error {
			def := getEnv("BEDROCK_INFERENCE_PROFILE", llmDefaultProfile)
			ps, err := bedrockProfiles(ctx, e.cfg)
			if err != nil {
				return err
			}
			models, err := llmModels(ps, def)
			if err != nil {
				return err
			}
			for _, n := range llmModelNames {
				slog.Info("model", "name", n, "inference profile", models[n].ID)
			}
			policy, err := llmPolicy(models)
			if err != nil {
				return err
			}
			return e.roleForSA(ctx, PodIdentity{llmNamespace, llmServiceAccount, policy})
		},
		Teardown: func(e *tdEnv) []pluginStep { return e.podIdentitySteps(llmNamespace, llmServiceAccount) },
		Inventory: func(ctx context.Context, e *tdEnv) ([]tdRow, error) {
			return e.podIdentityRows(ctx, llmNamespace, llmServiceAccount)
		},
	})
}

// llmModelNames are the names actors ask the gateway for: "claude" is the
// default profile, each other the newest ACTIVE profile of its family with
// the default's prefix (us., eu., global.).
var llmModelNames = []string{"claude", "claude-haiku", "claude-sonnet", "claude-opus"}

// bedrockProfile is one entry of Bedrock's ListInferenceProfiles.
type bedrockProfile struct {
	ID     string `json:"inferenceProfileId"`
	ARN    string `json:"inferenceProfileArn"`
	Status string `json:"status"`
	Models []struct {
		ARN string `json:"modelArn"`
	} `json:"models"`
}

// llmVersion reads the version after "<prefix>.anthropic.claude-<family>-":
// the minor version has at most two digits, so a date such as
// claude-sonnet-4-20250514 reads as 4.0.
var llmVersion = regexp.MustCompile(`^([0-9]+)(?:-([0-9]{1,2}))?(?:-|$)`)

// newestProfile is the ACTIVE profile with id prefix p and the highest
// version; a tie goes to the greater ID, so the choice does not depend on the
// order Bedrock lists them in.
func newestProfile(ps []bedrockProfile, p string) (bedrockProfile, bool) {
	var best bedrockProfile
	bestMaj, bestMin, found := 0, 0, false
	for _, x := range ps {
		rest, ok := strings.CutPrefix(x.ID, p)
		if x.Status != "ACTIVE" || !ok {
			continue
		}
		m := llmVersion.FindStringSubmatch(rest)
		if m == nil {
			continue
		}
		maj, _ := strconv.Atoi(m[1])
		mnr := 0
		if m[2] != "" {
			mnr, _ = strconv.Atoi(m[2])
		}
		if !found || maj > bestMaj || (maj == bestMaj && (mnr > bestMin || (mnr == bestMin && x.ID > best.ID))) {
			best, bestMaj, bestMin, found = x, maj, mnr, true
		}
	}
	return best, found
}

// llmModels maps each model name to its profile: "claude" to def, the others
// to the newest of their family.
func llmModels(ps []bedrockProfile, def string) (map[string]bedrockProfile, error) {
	prefix, _, ok := strings.Cut(def, ".")
	if !ok {
		return nil, fmt.Errorf("BEDROCK_INFERENCE_PROFILE %q has no Region prefix (us., eu., global.)", def)
	}
	out := map[string]bedrockProfile{}
	for _, p := range ps {
		if p.ID == def {
			out["claude"] = p
		}
	}
	if _, ok := out["claude"]; !ok {
		return nil, fmt.Errorf("no inference profile %s in this Region", def)
	}
	for _, n := range llmModelNames[1:] {
		p, ok := newestProfile(ps, prefix+".anthropic."+n+"-")
		if !ok {
			return nil, fmt.Errorf("no active %s. Claude %s inference profile in this Region", prefix, strings.TrimPrefix(n, "claude-"))
		}
		out[n] = p
	}
	return out, nil
}

// llmPolicy is the role's inline policy: per distinct profile, invoke the
// profile, and invoke its foundation models only through that profile.
func llmPolicy(models map[string]bedrockProfile) (string, error) {
	type stmt struct {
		Effect    string                       `json:"Effect"`
		Action    []string                     `json:"Action"`
		Resource  any                          `json:"Resource"`
		Condition map[string]map[string]string `json:"Condition,omitempty"`
	}
	actions := []string{"bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"}
	byID := map[string]bedrockProfile{}
	for _, p := range models {
		byID[p.ID] = p
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var ss []stmt
	for _, id := range ids {
		p := byID[id]
		var fm []string
		for _, m := range p.Models {
			fm = append(fm, m.ARN)
		}
		if p.ARN == "" || len(fm) == 0 {
			return "", fmt.Errorf("inference profile %s lists no models", id)
		}
		ss = append(ss, stmt{Effect: "Allow", Action: actions, Resource: p.ARN},
			stmt{Effect: "Allow", Action: actions, Resource: fm,
				Condition: map[string]map[string]string{"StringEquals": {"bedrock:InferenceProfileArn": p.ARN}}})
	}
	b, err := json.Marshal(struct {
		Version   string `json:"Version"`
		Statement []stmt `json:"Statement"`
	}{"2012-10-17", ss})
	return string(b), err
}

// bedrockProfiles lists the Region's inference profiles. setup-aws has no
// Bedrock SDK module, so it signs the REST call itself with the SDK's SigV4
// signer and awsload's credentials (signedGet says what that covers).
// Replaced in tests.
//
// shortcut: the endpoint is the aws partition's (bedrock.<region>.amazonaws.com);
// add the other partitions' DNS suffixes if they are ever supported.
var bedrockProfiles = func(ctx context.Context, c Config) ([]bedrockProfile, error) {
	ac, err := loadAWSConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	var out []bedrockProfile
	next := ""
	for {
		q := url.Values{"maxResults": {"1000"}}
		if next != "" {
			q.Set("nextToken", next)
		}
		var page struct {
			Summaries []bedrockProfile `json:"inferenceProfileSummaries"`
			Next      string           `json:"nextToken"`
		}
		u := "https://bedrock." + c.Region + ".amazonaws.com/inference-profiles?" + q.Encode()
		if err := signedGet(ctx, ac, "bedrock", u, &page); err != nil {
			return nil, fmt.Errorf("bedrock:ListInferenceProfiles: %w", err)
		}
		out = append(out, page.Summaries...)
		if next = page.Next; next == "" {
			return out, nil
		}
	}
}

// emptySHA256 is the payload hash of a request with no body.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signedGet is a SigV4-signed GET of u decoded into v. The credentials stay
// in memory; an error carries the service's message, never a header.
//
// The credentials come from ac (awsload), so they carry its pinned identity,
// but the request does not go through the SDK's retryer: a response that the
// credentials expired is not retried with refreshed ones, as SDK calls are.
// The step fails at once and is run again.
func signedGet(ctx context.Context, ac aws.Config, service, u string, v any) error {
	creds, err := ac.Credentials.Retrieve(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, emptySHA256, service, ac.Region, time.Now()); err != nil {
		return err
	}
	var hc interface {
		Do(*http.Request) (*http.Response, error)
	} = http.DefaultClient
	if ac.HTTPClient != nil {
		hc = ac.HTTPClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		return fmt.Errorf("%s: %s", resp.Status, e.Message)
	}
	return json.Unmarshal(body, v)
}

// ---- teardown and inventory of a ServiceAccount's Pod Identity --------------

// podIdentitySteps delete what roleForSA made for ns/sa: its associations,
// then its role <IAMPrefix>-<sa>. An association without this deployment's
// owner tag is left alone, a fixed name (stepAddon); the role, a derived
// name, is a refusal.
func (e *tdEnv) podIdentitySteps(ns, sa string) []pluginStep {
	role := e.n.IAMPrefix + "-" + sa
	return []pluginStep{
		{Step: func(ctx context.Context) ([]tdTarget, []string, error) {
			as, err := e.saAssociations(ctx, ns, sa)
			if err != nil {
				return nil, nil, err
			}
			var ts []tdTarget
			var rs []string
			for _, a := range as {
				if !e.owned(a.Tags) {
					continue
				}
				label := assocLabel(a)
				if r := e.refuse("Pod Identity association", label, a.Tags); r != "" {
					rs = append(rs, r)
					continue
				}
				id := a.AssociationId
				ts = append(ts, tdTarget{kind: "Pod Identity association", id: label, del: func(ctx context.Context) error {
					_, err := e.c.EKS.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{ClusterName: aws.String(e.n.Cluster), AssociationId: id})
					return err
				}})
			}
			return ts, rs, nil
		}},
		{Step: func(ctx context.Context) ([]tdTarget, []string, error) {
			return e.roleTargets(ctx, []string{role}, true)
		}},
	}
}

// podIdentityRows are ns/sa's associations and role that carry this
// deployment's owner tag, as inventory and core teardown label them.
func (e *tdEnv) podIdentityRows(ctx context.Context, ns, sa string) ([]tdRow, error) {
	as, err := e.saAssociations(ctx, ns, sa)
	if err != nil {
		return nil, err
	}
	var rows []tdRow
	for _, a := range as {
		if e.owned(a.Tags) {
			rows = append(rows, tdRow{Kind: "Pod Identity association", ID: assocLabel(a), Tags: a.Tags})
		}
	}
	roles, err := e.ownedRoles(ctx, e.n.IAMPrefix+"-"+sa)
	if err != nil {
		return nil, err
	}
	return append(rows, roles...), nil
}

// ownedRoles are the named roles that exist with this deployment's owner tag.
func (e *tdEnv) ownedRoles(ctx context.Context, names ...string) ([]tdRow, error) {
	var rows []tdRow
	for _, n := range names {
		g, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(n)})
		if tdGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get role %s: %w", n, err)
		}
		if tags := kvMap(g.Role.Tags, iamKV); e.owned(tags) {
			rows = append(rows, tdRow{Kind: "IAM role", ID: n, Tags: tags})
		}
	}
	return rows, nil
}

// saAssociations are the cluster's associations for ns/sa (none when the
// cluster is gone).
func (e *tdEnv) saAssociations(ctx context.Context, ns, sa string) ([]*ekstypes.PodIdentityAssociation, error) {
	cl, err := e.cluster(ctx)
	if err != nil || cl == nil {
		return nil, err
	}
	all, err := e.associations(ctx)
	if err != nil {
		return nil, err
	}
	var out []*ekstypes.PodIdentityAssociation
	for _, a := range all {
		if aws.ToString(a.Namespace) == ns && aws.ToString(a.ServiceAccount) == sa {
			out = append(out, a)
		}
	}
	return out, nil
}
