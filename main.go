package main

import (
	"context"
	"embed"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	signozVersion          = "0.137.0"
	collectorVersion       = "0.144.8"
	clickHouseVersion      = "25.12.5"
	clickHouseAgentVersion = "0.0.9"
)

var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
)

var signozImage = &resources.DockerImage{
	Name:   "signoz/signoz",
	Tag:    "v" + signozVersion,
	Digest: "sha256:d0c715a7f1441eca73df098b119693f8dd09154f99b835d59ed150f831645e51",
}

var collectorImage = &resources.DockerImage{
	Name:   "signoz/signoz-otel-collector",
	Tag:    "v" + collectorVersion,
	Digest: "sha256:6d1a59bc553e041014597eff0970608948c5c7447aaa984c4d109f2bc9f4062c",
}

const clickHouseImage = "ghcr.io/codefly-dev/service-signoz/clickhouse:25.12.5-1@sha256:a1a57d1d2ec9de0c7bba2e3b7aba52ec16773a0ba0b3b2230379e272f869484d"

type Settings struct{}

type Service struct {
	*services.Base
	*Settings

	otlpGRPCEndpoint *basev0.Endpoint
	otlpHTTPEndpoint *basev0.Endpoint
	queryEndpoint    *basev0.Endpoint
	uiEndpoint       *basev0.Endpoint
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	defer s.Wool.Catch()

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return services.Advertisement{
		Backends: runnersbase.BackendSupport{Docker: true},
		Config: []*agentv0.ConfigurationValueDetail{{
			Name: "connection", Description: "SigNoz service endpoints",
			Fields: []*agentv0.ConfigurationValueInformation{
				{Name: "grpc", Description: "private OTLP/gRPC ingestion endpoint"},
				{Name: "http", Description: "private OTLP/HTTP ingestion endpoint"},
				{Name: "query", Description: "module-visible query endpoint"},
				{Name: "web", Description: "module-visible UI endpoint"},
			},
		}, {
			Name: "signoz", Description: "SigNoz runtime secrets",
			Fields: []*agentv0.ConfigurationValueInformation{
				{Name: "SIGNOZ_TOKENIZER_JWT_SECRET", Description: "JWT signing secret"},
			},
		}},
		ReadMe: readme,
	}.Build(), nil
}

func main() {
	service := NewService()
	agents.Serve(agents.PluginRegistration{
		Agent:   service,
		Builder: NewBuilder(),
		Runtime: NewRuntime(),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
