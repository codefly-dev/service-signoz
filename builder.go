package main

import (
	"context"
	"embed"
	"fmt"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/upgrade"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
)

type DeploymentTemplateParameters struct {
	SigNozImage    string
	CollectorImage string
}

type Builder struct {
	*services.DefaultBuilder
	*Service
}

func NewBuilder() *Builder {
	service := NewService()
	return &Builder{
		DefaultBuilder: services.NewDefaultBuilder(service.Builder),
		Service:        service,
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
	s.Base.SetDockerImage(signozImage)

	return s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Parameters: DeploymentTemplateParameters{
			SigNozImage:    signozImage.FullName(),
			CollectorImage: collectorImage.FullName(),
		},
		Prepare: func(_ context.Context, deployment *services.KustomizeDeploymentContext) error {
			for _, key := range []string{"CLICKHOUSE_DSN", "SIGNOZ_TOKENIZER_JWT_SECRET"} {
				ref := deployment.Kubernetes.GetSecretReferences()[key]
				if ref == nil || ref.GetName() == "" || ref.GetKey() == "" {
					return fmt.Errorf("SigNoz deployment requires a Kubernetes Secret reference for %s", key)
				}
			}
			return nil
		},
	})
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
	var lockfileDiff string
	for _, subject := range upgradeSubjects() {
		result, err := upgrade.Docker(ctx, subject, upgrade.Options{
			IncludeMajor: req.IncludeMajor,
			DryRun:       req.DryRun,
		})
		if err != nil {
			return s.Builder.UpgradeError(err)
		}
		changes = append(changes, result.Changes...)
		lockfileDiff += result.LockfileDiff
	}
	return s.Builder.UpgradeResponse(changes, lockfileDiff)
}

func upgradeSubjects() []string {
	return []string{
		"signoz/signoz:" + signozVersion,
		"signoz/signoz-otel-collector:" + collectorVersion,
	}
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
		base := s.Base.BaseEndpoint(standards.TCP)
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
