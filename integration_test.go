package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/require"
	collecttracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestLocalStackAcceptsTraceAndSurvivesRestart(t *testing.T) {
	if os.Getenv("CODEFLY_SIGNOZ_INTEGRATION") != "1" {
		t.Skip("set CODEFLY_SIGNOZ_INTEGRATION=1 to exercise the Docker stack")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	image := os.Getenv("CODEFLY_SIGNOZ_CLICKHOUSE_IMAGE")
	if image == "" {
		image = clickHouseImage
	}
	nativePort := freePort(t)
	httpPort := freePort(t)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	containerName := "codefly-signoz-clickhouse-" + suffix
	volumeName := "codefly-signoz-clickhouse-data-" + suffix
	password := "integration-password"
	docker(t, ctx, "volume", "create", volumeName)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", containerName).Run()
		_ = exec.CommandContext(cleanupCtx, "docker", "volume", "rm", volumeName).Run()
	})
	docker(t, ctx,
		"run", "-d", "--name", containerName,
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", nativePort),
		"-p", fmt.Sprintf("127.0.0.1:%d:8123", httpPort),
		"-e", "CLICKHOUSE_USER=signoz",
		"-e", "CLICKHOUSE_PASSWORD="+password,
		"-e", "CLICKHOUSE_DB=signoz",
		"-e", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1",
		"-v", volumeName+":/var/lib/clickhouse",
		image,
	)
	waitForClickHouse(t, ctx, httpPort, password)

	root := t.TempDir()
	identity, endpoints := createIntegrationService(t, ctx, root)
	mappings := integrationNetworkMappings(t, ctx, identity, endpoints)
	dependency := clickHouseConfiguration(nativePort, password)

	first := startIntegrationRuntime(t, ctx, identity, mappings, dependency)
	sendControlledTrace(t, ctx, first.otlpHTTPPort)
	waitForTrace(t, ctx, httpPort, password)
	requireHTTPStatus(t, ctx, fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", first.queryPort))
	requireHTTPStatus(t, ctx, fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", first.uiPort))
	require.NoError(t, destroyRuntime(ctx, first))

	docker(t, ctx, "restart", containerName)
	waitForClickHouse(t, ctx, httpPort, password)
	require.Greater(t, traceCount(t, ctx, httpPort, password), 0)

	second := startIntegrationRuntime(t, ctx, identity, mappings, dependency)
	t.Cleanup(func() { _ = destroyRuntime(context.Background(), second) })
	require.Greater(t, traceCount(t, ctx, httpPort, password), 0)
}

func createIntegrationService(t *testing.T, ctx context.Context, root string) (*basev0.ServiceIdentity, []*basev0.Endpoint) {
	t.Helper()
	service := resources.Service{Name: "signoz", Version: "0.0.0"}
	servicePath := filepath.Join(root, "observability", "signoz")
	require.NoError(t, service.SaveAtDir(ctx, servicePath))
	identity := &basev0.ServiceIdentity{
		Name: "signoz", Module: "observability", Workspace: "integration", WorkspacePath: root,
		RelativeToWorkspace: filepath.Join("observability", "signoz"),
	}
	builder := NewBuilder()
	_, err := builder.Load(ctx, &builderv0.LoadRequest{
		Identity: identity, DisableCatch: true, CreationMode: &builderv0.CreationMode{Communicate: false},
	})
	require.NoError(t, err)
	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)
	builder.Service.Service.Agent = agent.Of(resources.ServiceAgent)
	builder.Service.Service.Endpoints, err = resources.FromProtoEndpoints(builder.Endpoints...)
	require.NoError(t, err)
	require.NoError(t, builder.Service.Service.Save(ctx))
	return identity, builder.Endpoints
}

func integrationNetworkMappings(t *testing.T, ctx context.Context, identity *basev0.ServiceIdentity, endpoints []*basev0.Endpoint) []*basev0.NetworkMapping {
	t.Helper()
	manager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	manager.WithTemporaryPorts()
	environment := resources.LocalEnvironment()
	mappings, err := manager.GenerateNetworkMappings(
		ctx,
		environment,
		&resources.Workspace{Name: identity.Workspace},
		resources.ServiceIdentityFromProto(identity),
		endpoints,
		resources.NewRuntimeContextFree(),
	)
	require.NoError(t, err)
	return mappings
}

func clickHouseConfiguration(port int, password string) *basev0.Configuration {
	return &basev0.Configuration{
		Origin: "observability/clickhouse", RuntimeContext: resources.NewRuntimeContextContainer(),
		Infos: []*basev0.ConfigurationInformation{{
			Name: "clickhouse",
			ConfigurationValues: []*basev0.ConfigurationValue{{
				Key: "connection", Value: fmt.Sprintf("clickhouse://signoz:%s@host.docker.internal:%d/signoz", password, port), Secret: true,
			}},
		}},
	}
}

func startIntegrationRuntime(t *testing.T, ctx context.Context, identity *basev0.ServiceIdentity, mappings []*basev0.NetworkMapping, dependency *basev0.Configuration) *Runtime {
	t.Helper()
	runtime := NewRuntime()
	environment := resources.LocalEnvironment()
	loadResponse, err := runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity: identity, Environment: shared.Must(environment.Proto()), DisableCatch: true,
	})
	require.NoError(t, err)
	require.NoError(t, services.ValidateRuntimeLoadResponse(loadResponse))
	require.Len(t, runtime.Endpoints, 4)
	initResponse, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext: resources.NewRuntimeContextFree(), ProposedNetworkMappings: mappings,
		Configuration: &basev0.Configuration{
			Origin: "observability/signoz", RuntimeContext: resources.NewRuntimeContextFree(),
			Infos: []*basev0.ConfigurationInformation{{
				Name: "signoz",
				ConfigurationValues: []*basev0.ConfigurationValue{{
					Key: "SIGNOZ_TOKENIZER_JWT_SECRET", Value: "integration-jwt-secret-with-sufficient-entropy", Secret: true,
				}},
			}},
		},
		DependenciesConfigurations: []*basev0.Configuration{dependency},
	})
	require.NoError(t, err)
	require.NoError(t, services.ValidateRuntimeInitResponse(initResponse))
	startResponse, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.NoError(t, services.ValidateRuntimeStartResponse(startResponse))
	return runtime
}

func destroyRuntime(ctx context.Context, runtime *Runtime) error {
	response, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	if err != nil {
		return err
	}
	return services.ValidateRuntimeDestroyResponse(response)
}

func sendControlledTrace(t *testing.T, ctx context.Context, port uint16) {
	t.Helper()
	now := uint64(time.Now().UnixNano())
	payload, err := proto.Marshal(&collecttracev1.ExportTraceServiceRequest{
		ResourceSpans: []*tracev1.ResourceSpans{{
			Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{{
				Key: "service.name", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "codefly-acceptance"}},
			}}},
			ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{{
				TraceId: []byte("0123456789abcdef"), SpanId: []byte("12345678"), Name: "controlled-trace",
				Kind: tracev1.Span_SPAN_KIND_SERVER, StartTimeUnixNano: now, EndTimeUnixNano: now + uint64(time.Millisecond),
			}}}},
		}},
	})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/traces", port), bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/x-protobuf")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
}

func waitForTrace(t *testing.T, ctx context.Context, port int, password string) {
	t.Helper()
	for range 60 {
		if traceCount(t, ctx, port, password) > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	t.Fatal("controlled trace was not written to ClickHouse")
}

func traceCount(t *testing.T, ctx context.Context, port int, password string) int {
	t.Helper()
	query := url.Values{"query": {"SELECT count() FROM signoz_traces.signoz_index_v3 WHERE name = 'controlled-trace'"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/?%s", port, query.Encode()), nil)
	require.NoError(t, err)
	request.SetBasicAuth("signoz", password)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0
	}
	count, _ := strconv.Atoi(strings.TrimSpace(string(body)))
	return count
}

func waitForClickHouse(t *testing.T, ctx context.Context, port int, password string) {
	t.Helper()
	for range 60 {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/ping", port), nil)
		require.NoError(t, err)
		request.SetBasicAuth("signoz", password)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	t.Fatal("ClickHouse did not become ready")
}

func requireHTTPStatus(t *testing.T, ctx context.Context, address string) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Less(t, response.StatusCode, http.StatusInternalServerError)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func docker(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	require.NoError(t, err, "%s", strings.TrimSpace(string(output)))
	return strings.TrimSpace(string(output))
}
