package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
)

type DeploymentTemplateParameters struct {
	SigNozImage       string
	CollectorImage    string
	MigrationRevision string
}

type Builder struct {
	*services.DefaultBuilder
	*Service
	tagSource imageTagSource
}

type imageTagSource interface {
	Tags(context.Context, string) ([]string, error)
}

type dockerHubTagSource struct {
	client  *http.Client
	baseURL string
}

type upgradeSubject struct {
	repository string
	currentTag string
}

func NewBuilder() *Builder {
	service := NewService()
	return &Builder{
		DefaultBuilder: services.NewDefaultBuilder(service.Builder),
		Service:        service,
		tagSource:      dockerHubTagSource{client: http.DefaultClient, baseURL: "https://hub.docker.com"},
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()
	return s.Builder.LoadService(ctx, req, services.BuilderLoad{
		Settings:         s.Settings,
		Requirements:     requirements,
		FactoryTemplates: factoryFS,
		ResolveEndpoints: s.resolveEndpoints,
	})
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()
	s.Builder.LogInitRequest(req)
	s.DependencyEndpoints = req.DependenciesEndpoints
	return s.Builder.InitResponse()
}

func (s *Builder) Create(ctx context.Context, _ *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	s.Service.Service.ServiceDependencies = append(s.Service.Service.ServiceDependencies, &resources.ServiceDependency{
		Name:   "clickhouse",
		Module: s.Identity.Module,
		Endpoints: []*resources.EndpointReference{{
			Name: "tcp",
		}},
	})
	if err := s.createEndpoints(ctx); err != nil {
		return s.Builder.CreateError(err)
	}
	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	s.SetDockerImage(signozImage)

	parameters := &DeploymentTemplateParameters{
		SigNozImage:    signozImage.FullName(),
		CollectorImage: collectorImage.FullName(),
	}
	return s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Parameters:           parameters,
		Prepare: func(_ context.Context, deployment *services.KustomizeDeploymentContext) error {
			for _, key := range []string{"CLICKHOUSE_DSN", "SIGNOZ_TOKENIZER_JWT_SECRET"} {
				ref := deployment.Kubernetes.GetSecretReferences()[key]
				if ref == nil || ref.GetName() == "" || ref.GetKey() == "" {
					return fmt.Errorf("SigNoz deployment requires a Kubernetes Secret reference for %s", key)
				}
			}
			parameters.MigrationRevision = migrationRevision(parameters.CollectorImage, deployment.Kubernetes.GetSecretReferences()["CLICKHOUSE_DSN"])
			return nil
		},
	})
}

func migrationRevision(collectorImage string, reference *builderv0.KubernetesSecretKeyReference) string {
	value := fmt.Sprintf("%s\n%s\n%s\n%s\n%t", agent.Version, collectorImage, reference.GetName(), reference.GetKey(), reference.GetOptional())
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))[:12]
}

func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	return s.Builder.AuditContainer(s.Wool.Inject(ctx), req, signozImage.FullName())
}

func (s *Builder) SBOM(ctx context.Context, _ *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	return s.Builder.SBOMContainer(s.Wool.Inject(ctx), signozImage.FullName())
}

func (s *Builder) Upgrade(ctx context.Context, req *builderv0.UpgradeRequest) (*builderv0.UpgradeResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	var changes []*builderv0.UpgradeChange
	for _, subject := range upgradeSubjects() {
		tags, err := s.tagSource.Tags(ctx, subject.repository)
		if err != nil {
			return s.Builder.UpgradeError(err)
		}
		target := selectUpgradeTag(subject.currentTag, tags, req.IncludeMajor)
		if target != "" {
			changes = append(changes, &builderv0.UpgradeChange{Package: subject.repository, From: subject.currentTag, To: target})
		}
	}
	return s.Builder.UpgradeResponse(changes, "")
}

func upgradeSubjects() []upgradeSubject {
	return []upgradeSubject{
		{repository: signozImage.Name, currentTag: signozImage.Tag},
		{repository: collectorImage.Name, currentTag: collectorImage.Tag},
	}
}

func (source dockerHubTagSource) Tags(ctx context.Context, repository string) ([]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.baseURL+"/v2/repositories/"+repository+"/tags/?page_size=100&ordering=last_updated", nil)
	if err != nil {
		return nil, err
	}
	response, err := source.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list Docker Hub tags for %s: %w", repository, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list Docker Hub tags for %s: %s", repository, response.Status)
	}
	var body struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	}
	if err = json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode Docker Hub tags for %s: %w", repository, err)
	}
	tags := make([]string, 0, len(body.Results))
	for _, result := range body.Results {
		tags = append(tags, result.Name)
	}
	return tags, nil
}

func selectUpgradeTag(current string, tags []string, includeMajor bool) string {
	currentVersion, ok := parseImageVersion(current)
	if !ok {
		return ""
	}
	var selected string
	var selectedVersion [3]int
	for _, tag := range tags {
		version, valid := parseImageVersion(tag)
		if !valid || (!includeMajor && version[0] != currentVersion[0]) || compareImageVersions(version, currentVersion) <= 0 {
			continue
		}
		if selected == "" || compareImageVersions(version, selectedVersion) > 0 {
			selected = tag
			selectedVersion = version
		}
	}
	return selected
}

func parseImageVersion(tag string) ([3]int, bool) {
	version := strings.TrimPrefix(tag, "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var parsed [3]int
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return [3]int{}, false
		}
		parsed[index] = value
	}
	return parsed, true
}

func compareImageVersions(left, right [3]int) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

func (s *Service) resolveEndpoints(ctx context.Context, endpoints []*basev0.Endpoint) error {
	var err error
	s.otlpGRPCEndpoint, err = resources.FindTCPEndpointWithName(ctx, "grpc", endpoints)
	if err != nil {
		return err
	}
	s.otlpHTTPEndpoint, err = resources.FindTCPEndpointWithName(ctx, "http", endpoints)
	if err != nil {
		return err
	}
	s.queryEndpoint, err = resources.FindTCPEndpointWithName(ctx, "query", endpoints)
	if err != nil {
		return err
	}
	s.uiEndpoint, err = resources.FindTCPEndpointWithName(ctx, "web", endpoints)
	return err
}

func (s *Builder) createEndpoints(ctx context.Context) error {
	tcp, err := resources.LoadTCPAPI(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load TCP API")
	}
	create := func(name string, visibility resources.Visibility) (*basev0.Endpoint, error) {
		base := s.BaseEndpoint(standards.TCP)
		base.Name = name
		base.Visibility = visibility
		return resources.NewAPI(ctx, base, resources.ToTCPAPI(tcp))
	}
	if s.otlpGRPCEndpoint, err = create("grpc", resources.VisibilityPrivate); err != nil {
		return err
	}
	if s.otlpHTTPEndpoint, err = create("http", resources.VisibilityPrivate); err != nil {
		return err
	}
	if s.queryEndpoint, err = create("query", resources.VisibilityModule); err != nil {
		return err
	}
	if s.uiEndpoint, err = create("web", resources.VisibilityModule); err != nil {
		return err
	}
	s.Endpoints = []*basev0.Endpoint{s.otlpGRPCEndpoint, s.otlpHTTPEndpoint, s.queryEndpoint, s.uiEndpoint}
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
