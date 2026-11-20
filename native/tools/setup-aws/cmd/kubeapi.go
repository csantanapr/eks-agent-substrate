package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// setup-aws reads one thing from the Kubernetes API: whether the nodes of
// a self-managed node group are Ready. EKS reports nothing about nodes it
// does not manage, so the wait asks the API itself, as kubectl would, with
// a token from the caller's AWS credentials. The caller already has
// cluster-admin from the access phase.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/cluster-auth.html

// kubeNode is what the wait reads of a node.
type kubeNode struct {
	Name        string
	Labels      map[string]string
	Ready       bool
	Allocatable map[string]string
}

// nodeLister lists the nodes that match a label selector.
type nodeLister interface {
	Nodes(ctx context.Context, labelSelector string) ([]kubeNode, error)
}

// stsPresigner presigns sts:GetCallerIdentity (sts.PresignClient).
type stsPresigner interface {
	PresignGetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// kubeTimeout bounds one request to the Kubernetes API.
var kubeTimeout = 15 * time.Second

type kubeClient struct {
	cluster, endpoint string
	http              *http.Client
	sts               stsPresigner
}

// newKubeClient is a client of cluster c's API that trusts only the
// cluster's own certificate authority.
func newKubeClient(c *ekstypes.Cluster, p stsPresigner) (*kubeClient, error) {
	ca := ""
	if c.CertificateAuthority != nil {
		ca = aws.ToString(c.CertificateAuthority.Data)
	}
	pem, err := base64.StdEncoding.DecodeString(ca)
	pool := x509.NewCertPool()
	endpoint := aws.ToString(c.Endpoint)
	if endpoint == "" || err != nil || !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("cluster %s reports no endpoint or certificate authority", aws.ToString(c.Name))
	}
	// Go's client, like kubectl, honours HTTPS_PROXY.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &kubeClient{cluster: aws.ToString(c.Name), endpoint: strings.TrimSuffix(endpoint, "/"),
		http: &http.Client{Transport: t, Timeout: kubeTimeout}, sts: p}, nil
}

// eksToken is a bearer token for cluster, as `aws eks get-token` makes it:
// a presigned sts:GetCallerIdentity URL that names the cluster in a signed
// header. EKS takes it for about 15 minutes, so each request makes a new one.
func eksToken(ctx context.Context, p stsPresigner, cluster string) (string, error) {
	req, err := p.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, func(so *sts.Options) {
			so.APIOptions = append(so.APIOptions,
				smithyhttp.SetHeaderValue("x-k8s-aws-id", cluster),
				smithyhttp.SetHeaderValue("X-Amz-Expires", "60"))
		})
	})
	if err != nil {
		return "", fmt.Errorf("presign sts:GetCallerIdentity for the Kubernetes API: %w", err)
	}
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(req.URL)), nil
}

// nodeList is the part of a v1 NodeList the wait reads.
type nodeList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Allocatable map[string]string `json:"allocatable"`
			Conditions  []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// kubeError is a request to the Kubernetes API that failed on the way:
// Status is the HTTP status of the answer, 0 when none came whole (a
// transport error or a timeout), or -1 when the server's certificate failed
// verification.
type kubeError struct {
	Status int
	err    error
}

func (e *kubeError) Error() string { return e.err.Error() }
func (e *kubeError) Unwrap() error { return e.err }

// certificateError reports a TLS failure to verify the server's certificate.
func certificateError(err error) bool {
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &ci)
}

// kubeTransient reports whether err is a failed request that may work when
// made again: no whole answer, 429 (too many requests) or a 5xx. Any other
// answer (401, 403, another 4xx) would come again.
func kubeTransient(err error) bool {
	var ke *kubeError
	return errors.As(err, &ke) && (ke.Status == 0 || ke.Status == http.StatusTooManyRequests || ke.Status >= 500)
}

// request sends one request to the Kubernetes API with a new EKS token and
// returns the answer's status and body, whatever the status. A request that
// gets no whole answer is a kubeError with Status 0, or -1 when the server's
// certificate fails verification; what names the request in the error. The
// token is never part of an error.
func (k *kubeClient) request(ctx context.Context, method, path string, query url.Values, contentType string, body []byte, what string) (int, []byte, error) {
	token, err := eksToken(ctx, k.sts, k.cluster)
	if err != nil {
		return 0, nil, err
	}
	u := k.endpoint + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	failed := func(status int, err error) error {
		return &kubeError{Status: status, err: fmt.Errorf("%s of cluster %s: %w", what, k.cluster, err)}
	}
	resp, err := k.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		// A certificate the cluster CA does not sign (a wrong CA, or a
		// TLS-inspecting proxy) fails the same way on every try.
		if certificateError(err) {
			return 0, nil, failed(-1, err)
		}
		return 0, nil, failed(0, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, failed(0, err)
	}
	return resp.StatusCode, b, nil
}

// Nodes lists the nodes that match labelSelector. Any answer but 200 is a
// kubeError that names the status; the token is never part of one.
func (k *kubeClient) Nodes(ctx context.Context, labelSelector string) ([]kubeNode, error) {
	status, body, err := k.request(ctx, http.MethodGet, "/api/v1/nodes", url.Values{"labelSelector": {labelSelector}}, "", nil, "list the nodes")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &kubeError{Status: status, err: fmt.Errorf("list the nodes of cluster %s: the Kubernetes API answered %d %s", k.cluster, status, http.StatusText(status))}
	}
	var list nodeList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("list the nodes of cluster %s: %w", k.cluster, err)
	}
	out := make([]kubeNode, 0, len(list.Items))
	for _, it := range list.Items {
		n := kubeNode{Name: it.Metadata.Name, Labels: it.Metadata.Labels, Allocatable: it.Status.Allocatable}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		out = append(out, n)
	}
	return out, nil
}

// setup-aws also applies the objects of KARPENTER=on (karpenter_kube.go)
// and deletes some of them in teardown: Karpenter's chart, its NodePools,
// EC2NodeClasses and NodeOverlays. It reads and writes only the kinds of kubeKinds, through
// server-side apply, as kubectl apply --server-side does, with its own field
// manager.
//
//	https://kubernetes.io/docs/reference/using-api/server-side-apply/

// kubeKind is one kind of object the client knows: its API version, kind,
// resource (the URL's plural) and scope.
type kubeKind struct {
	APIVersion, Kind, Resource string
	Namespaced                 bool
}

var (
	kindCRD                = kubeKind{"apiextensions.k8s.io/v1", "CustomResourceDefinition", "customresourcedefinitions", false}
	kindServiceAccount     = kubeKind{"v1", "ServiceAccount", "serviceaccounts", true}
	kindService            = kubeKind{"v1", "Service", "services", true}
	kindClusterRole        = kubeKind{"rbac.authorization.k8s.io/v1", "ClusterRole", "clusterroles", false}
	kindClusterRoleBinding = kubeKind{"rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "clusterrolebindings", false}
	kindRole               = kubeKind{"rbac.authorization.k8s.io/v1", "Role", "roles", true}
	kindRoleBinding        = kubeKind{"rbac.authorization.k8s.io/v1", "RoleBinding", "rolebindings", true}
	kindPDB                = kubeKind{"policy/v1", "PodDisruptionBudget", "poddisruptionbudgets", true}
	kindDeployment         = kubeKind{"apps/v1", "Deployment", "deployments", true}
	// kindPod is only read: why the controller Deployment is not available.
	kindPod          = kubeKind{"v1", "Pod", "pods", true}
	kindNodePool     = kubeKind{"karpenter.sh/v1", "NodePool", "nodepools", false}
	kindNodeClaim    = kubeKind{"karpenter.sh/v1", "NodeClaim", "nodeclaims", false}
	kindEC2NodeClass = kubeKind{"karpenter.k8s.aws/v1", "EC2NodeClass", "ec2nodeclasses", false}

	// kubeKinds are the kinds the client reads and writes; any other is an
	// error.
	kubeKinds = []kubeKind{kindCRD, kindServiceAccount, kindService, kindClusterRole, kindClusterRoleBinding,
		kindRole, kindRoleBinding, kindPDB, kindDeployment, kindPod, kindNodePool, kindNodeClaim, kindEC2NodeClass, kindNodeOverlay}
)

func (k kubeKind) String() string { return k.Kind }

// path is the URL path of the kind's objects in namespace ns (ignored for a
// cluster-scoped kind), or of object name when it is not empty.
func (k kubeKind) path(ns, name string) string {
	p := "/apis/" + k.APIVersion
	if !strings.Contains(k.APIVersion, "/") {
		p = "/api/" + k.APIVersion
	}
	if k.Namespaced {
		p += "/namespaces/" + ns
	}
	p += "/" + k.Resource
	if name != "" {
		p += "/" + name
	}
	return p
}

// kubeObject is one object as JSON decodes it.
type kubeObject map[string]any

// field is the value at path, or nil.
func (o kubeObject) field(path ...string) any {
	var v any = map[string]any(o)
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// str is the string at path, or "".
func (o kubeObject) str(path ...string) string {
	s, _ := o.field(path...).(string)
	return s
}

func (o kubeObject) name() string      { return o.str("metadata", "name") }
func (o kubeObject) namespace() string { return o.str("metadata", "namespace") }

// kind is the object's entry in kubeKinds.
func (o kubeObject) kind() (kubeKind, error) {
	for _, k := range kubeKinds {
		if k.APIVersion == o.str("apiVersion") && k.Kind == o.str("kind") {
			return k, nil
		}
	}
	return kubeKind{}, fmt.Errorf("internal: setup-aws does not apply %s %s", o.str("apiVersion"), o.str("kind"))
}

// label is the object's label key, or "".
func (o kubeObject) label(key string) string { return o.str("metadata", "labels", key) }

// setLabel sets label key, making the labels when there are none.
func (o kubeObject) setLabel(key, value string) {
	md, _ := o["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
		o["metadata"] = md
	}
	labels, _ := md["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
		md["labels"] = labels
	}
	labels[key] = value
}

// kubeCondition is one of status.conditions.
type kubeCondition struct{ Type, Status, Reason, Message string }

func (o kubeObject) conditions() []kubeCondition {
	list, _ := o.field("status", "conditions").([]any)
	var out []kubeCondition
	for _, c := range list {
		m, _ := c.(map[string]any)
		str := func(k string) string { s, _ := m[k].(string); return s }
		out = append(out, kubeCondition{str("type"), str("status"), str("reason"), str("message")})
	}
	return out
}

// condition is the condition of type t, and whether there is one.
func (o kubeObject) condition(t string) (kubeCondition, bool) {
	for _, c := range o.conditions() {
		if c.Type == t {
			return c, true
		}
	}
	return kubeCondition{}, false
}

// kubeObjects reads and writes objects of kubeKinds. Get returns nil for an
// object that does not exist, or whose kind the cluster does not serve (its
// CRD is not there); Delete returns nil for one that is gone; List fails
// with a kubeError of Status 404 (kubeNotFound) when the kind is not served.
// Any other answer but success is a kubeError that names the status.
type kubeObjects interface {
	Apply(ctx context.Context, obj kubeObject, dryRun bool) (kubeObject, error)
	Get(ctx context.Context, k kubeKind, namespace, name string) (kubeObject, error)
	List(ctx context.Context, k kubeKind, namespace, labelSelector string) ([]kubeObject, error)
	Delete(ctx context.Context, k kubeKind, namespace, name string) error
}

// fieldManager owns the fields setup-aws applies.
const fieldManager = "setup-aws"

// kubeNotFound reports a 404 from the Kubernetes API.
func kubeNotFound(err error) bool {
	var ke *kubeError
	return errors.As(err, &ke) && ke.Status == http.StatusNotFound
}

// answered is the kubeError of an answer that is not a success: its status,
// and the message of the Status the API sends with it.
func (k *kubeClient) answered(status int, body []byte, what string) error {
	msg := fmt.Sprintf("the Kubernetes API answered %d %s", status, http.StatusText(status))
	var st struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &st) == nil && st.Message != "" {
		m := st.Message
		if len(m) > 400 {
			m = m[:400] + "..."
		}
		msg += ": " + m
	}
	return &kubeError{Status: status, err: fmt.Errorf("%s of cluster %s: %s", what, k.cluster, msg)}
}

// Apply server-side applies obj, with force, so setup-aws takes the fields
// it sets from any other manager, and returns the object as the API stores
// it. dryRun asks the API what it would store, and stores nothing.
func (k *kubeClient) Apply(ctx context.Context, obj kubeObject, dryRun bool) (kubeObject, error) {
	kind, err := obj.kind()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	q := url.Values{"fieldManager": {fieldManager}, "force": {"true"}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	what := "apply " + kind.Kind + " " + obj.name()
	status, resp, err := k.request(ctx, http.MethodPatch, kind.path(obj.namespace(), obj.name()), q, "application/apply-patch+yaml", body, what)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, k.answered(status, resp, what)
	}
	var out kubeObject
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("%s of cluster %s: %w", what, k.cluster, err)
	}
	return out, nil
}

// Get reads object name of kind k.
func (k *kubeClient) Get(ctx context.Context, kind kubeKind, ns, name string) (kubeObject, error) {
	what := "read " + kind.Kind + " " + name
	status, resp, err := k.request(ctx, http.MethodGet, kind.path(ns, name), nil, "", nil, what)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, k.answered(status, resp, what)
	}
	var out kubeObject
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("%s of cluster %s: %w", what, k.cluster, err)
	}
	return out, nil
}

// List reads the objects of kind k that match labelSelector, in namespace
// ns for a namespaced kind (ignored for a cluster-scoped one).
func (k *kubeClient) List(ctx context.Context, kind kubeKind, ns, labelSelector string) ([]kubeObject, error) {
	what := "list the " + kind.Resource
	var q url.Values
	if labelSelector != "" {
		q = url.Values{"labelSelector": {labelSelector}}
	}
	status, resp, err := k.request(ctx, http.MethodGet, kind.path(ns, ""), q, "", nil, what)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, k.answered(status, resp, what)
	}
	var list struct {
		Items []kubeObject `json:"items"`
	}
	if err := json.Unmarshal(resp, &list); err != nil {
		return nil, fmt.Errorf("%s of cluster %s: %w", what, k.cluster, err)
	}
	return list.Items, nil
}

// Delete asks the API to delete object name of kind k. The object can stay
// a while after: its finalizers run first.
func (k *kubeClient) Delete(ctx context.Context, kind kubeKind, ns, name string) error {
	what := "delete " + kind.Kind + " " + name
	status, resp, err := k.request(ctx, http.MethodDelete, kind.path(ns, name), nil, "", nil, what)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusAccepted, http.StatusNotFound:
		return nil
	}
	return k.answered(status, resp, what)
}
