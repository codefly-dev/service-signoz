package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestCreateDeclaresClickHouseAndEndpointVisibility(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	name := fmt.Sprintf("signoz-%d", time.Now().UnixNano())
	service := resources.Service{Name: name, Version: "0.0.0"}
	servicePath := filepath.Join(root, "observability", name)
	require.NoError(t, service.SaveAtDir(ctx, servicePath))
	identity := &basev0.ServiceIdentity{
		Name: name, Module: "observability", Workspace: "test", WorkspacePath: root,
		RelativeToWorkspace: filepath.Join("observability", name),
	}
	builder := NewBuilder()
	_, err := builder.Load(ctx, &builderv0.LoadRequest{
		Identity: identity, DisableCatch: true, CreationMode: &builderv0.CreationMode{Communicate: false},
	})
	require.NoError(t, err)
	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	require.Len(t, builder.Service.Service.ServiceDependencies, 1)
	require.Equal(t, "clickhouse", builder.Service.Service.ServiceDependencies[0].Name)
	require.Equal(t, "tcp", builder.Service.Service.ServiceDependencies[0].Endpoints[0].Name)
	require.Len(t, builder.Endpoints, 4)
	visibility := map[string]resources.Visibility{}
	for _, endpoint := range builder.Endpoints {
		visibility[endpoint.Name] = resources.Visibility(endpoint.Visibility)
	}
	require.Equal(t, resources.VisibilityPrivate, visibility["grpc"])
	require.Equal(t, resources.VisibilityPrivate, visibility["http"])
	require.Equal(t, resources.VisibilityModule, visibility["query"])
	require.Equal(t, resources.VisibilityModule, visibility["web"])

	data, err := os.ReadFile("templates/factory/service.codefly.yaml.tmpl")
	require.NoError(t, err)
	require.Contains(t, string(data), "name: signoz")
	require.Contains(t, string(data), "version: 0.0.1")
}

func TestReleasePinsAndUpgradeCoverage(t *testing.T) {
	require.Equal(t, "0.0.1", agent.Version)
	require.Equal(t, "v0.137.0", signozImage.Tag)
	require.Equal(t, "v0.144.8", collectorImage.Tag)
	require.Equal(t, "25.12.5", clickHouseVersion)
	require.Equal(t, "0.0.9", clickHouseAgentVersion)
	require.Contains(t, signozImage.FullName(), "@sha256:")
	require.Contains(t, collectorImage.FullName(), "@sha256:")
	require.Contains(t, clickHouseImage, "25.12.5-1@sha256:")
	require.Equal(t, []upgradeSubject{
		{repository: "signoz/signoz", currentTag: "v0.137.0"},
		{repository: "signoz/signoz-otel-collector", currentTag: "v0.144.8"},
	}, upgradeSubjects())

	manifest, err := os.ReadFile("agent.codefly.yaml")
	require.NoError(t, err)
	require.Contains(t, string(manifest), "version: "+agent.Version)
	factory, err := os.ReadFile("templates/factory/service.codefly.yaml.tmpl")
	require.NoError(t, err)
	require.Contains(t, string(factory), "version: "+agent.Version)
	gettingStarted, err := os.ReadFile("templates/factory/GETTING_STARTED.md.tmpl")
	require.NoError(t, err)
	require.Contains(t, string(gettingStarted), "version: "+clickHouseAgentVersion)
	require.Contains(t, string(gettingStarted), clickHouseImage)
	integrationWorkflow, err := os.ReadFile(".github/workflows/integration.yml")
	require.NoError(t, err)
	require.Contains(t, string(integrationWorkflow), clickHouseImage)
	for _, path := range []string{
		".goreleaser.yaml", ".github/workflows/ci.yml", ".github/workflows/releaser.yml", ".github/workflows/manifest-guard.yml",
	} {
		_, err = os.Stat(path)
		require.NoError(t, err, path)
	}
}

func TestUpgradeReportsVPrefixedSigNozTags(t *testing.T) {
	builder := NewBuilder()
	builder.tagSource = staticImageTagSource{
		"signoz/signoz":                {"latest", "v1.0.0", "v0.137.1", "v0.137.1-linux-amd64"},
		"signoz/signoz-otel-collector": {"v0.144.9", "v0.144.8"},
	}

	response, err := builder.Upgrade(context.Background(), &builderv0.UpgradeRequest{})
	require.NoError(t, err)
	require.Equal(t, builderv0.UpgradeStatus_SUCCESS, response.GetState().GetState())
	require.Equal(t, []*builderv0.UpgradeChange{
		{Package: "signoz/signoz", From: "v0.137.0", To: "v0.137.1"},
		{Package: "signoz/signoz-otel-collector", From: "v0.144.8", To: "v0.144.9"},
	}, response.GetChanges())

	response, err = builder.Upgrade(context.Background(), &builderv0.UpgradeRequest{IncludeMajor: true})
	require.NoError(t, err)
	require.Equal(t, "v1.0.0", response.GetChanges()[0].GetTo())
}

type staticImageTagSource map[string][]string

func (source staticImageTagSource) Tags(_ context.Context, repository string) ([]string, error) {
	return source[repository], nil
}
