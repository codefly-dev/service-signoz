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
	require.Equal(t, []string{"signoz/signoz:0.137.0", "signoz/signoz-otel-collector:0.144.8"}, upgradeSubjects())

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
	for _, path := range []string{
		".goreleaser.yaml", ".github/workflows/ci.yml", ".github/workflows/releaser.yml", ".github/workflows/manifest-guard.yml",
	} {
		_, err = os.Stat(path)
		require.NoError(t, err, path)
	}
}
