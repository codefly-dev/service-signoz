package main

import (
	"context"
	"embed"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
)

type clickHouseConnection struct {
	Host     string
	Port     string
	User     string
	Password string
}

type Runtime struct {
	services.RuntimeServer
	*Service

	queryRunner     *dockerrun.DockerEnvironment
	collectorRunner *dockerrun.DockerEnvironment
	uiProxy         *http.Server
	configDir       string

	queryPort    uint16
	uiPort       uint16
	otlpGRPCPort uint16
	otlpHTTPPort uint16
	healthPort   uint16
}

func NewRuntime() *Runtime {
	return &Runtime{Service: NewService()}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	return s.Runtime.LoadService(ctx, req, services.RuntimeLoad{
		Settings:         s.Settings,
		Requirements:     requirements,
		ResolveEndpoints: s.resolveEndpoints,
	})
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	s.Runtime.LogInitRequest(req)
	s.Runtime.WithContext(req.GetRuntimeContext())
	s.NetworkMappings = req.ProposedNetworkMappings

	var err error
	if s.otlpGRPCPort, err = s.nativePort(ctx, req, s.otlpGRPCEndpoint); err != nil {
		return s.Runtime.InitError(err)
	}
	if s.otlpHTTPPort, err = s.nativePort(ctx, req, s.otlpHTTPEndpoint); err != nil {
		return s.Runtime.InitError(err)
	}
	if s.queryPort, err = s.nativePort(ctx, req, s.queryEndpoint); err != nil {
		return s.Runtime.InitError(err)
	}
	if s.uiPort, err = s.nativePort(ctx, req, s.uiEndpoint); err != nil {
		return s.Runtime.InitError(err)
	}
	if s.healthPort, err = freeLocalPort(); err != nil {
		return s.Runtime.InitError(err)
	}
	jwtSecret, err := resources.GetConfigurationValue(ctx, req.GetConfiguration(), "signoz", "SIGNOZ_TOKENIZER_JWT_SECRET")
	if err != nil || jwtSecret == "" {
		return s.Runtime.InitError(fmt.Errorf("SigNoz requires SIGNOZ_TOKENIZER_JWT_SECRET in its service configuration"))
	}

	clickhouse, err := parseClickHouseDependency(ctx, req.DependenciesConfigurations)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	if err = runMigrations(ctx, clickhouse); err != nil {
		return s.Runtime.InitError(err)
	}

	if err = s.initQuery(ctx, clickhouse, jwtSecret); err != nil {
		return s.Runtime.InitError(err)
	}
	if err = s.initCollector(ctx, clickhouse); err != nil {
		_ = s.queryRunner.Shutdown(ctx)
		return s.Runtime.InitError(err)
	}
	if err = s.addRuntimeConfigurations(ctx); err != nil {
		return s.Runtime.InitError(err)
	}
	return s.Runtime.InitResponse()
}

func (s *Runtime) nativePort(ctx context.Context, req *runtimev0.InitRequest, endpoint *basev0.Endpoint) (uint16, error) {
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.ProposedNetworkMappings, endpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		return 0, err
	}
	return uint16(instance.Port), nil
}

func freeLocalPort() (uint16, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return uint16(listener.Addr().(*net.TCPAddr).Port), nil
}

func parseClickHouseDependency(ctx context.Context, configurations []*basev0.Configuration) (clickHouseConnection, error) {
	for _, configuration := range resources.FilterConfigurations(configurations, resources.NewRuntimeContextContainer()) {
		connection, err := resources.GetConfigurationValue(ctx, configuration, "clickhouse", "connection")
		if err != nil || connection == "" {
			continue
		}
		parsed, err := url.Parse(connection)
		if err != nil {
			return clickHouseConnection{}, fmt.Errorf("parse ClickHouse dependency connection: %w", err)
		}
		if parsed.Scheme != "clickhouse" && parsed.Scheme != "tcp" {
			return clickHouseConnection{}, fmt.Errorf("unsupported ClickHouse connection scheme %q", parsed.Scheme)
		}
		if parsed.User == nil {
			return clickHouseConnection{}, fmt.Errorf("ClickHouse dependency connection is incomplete")
		}
		password, _ := parsed.User.Password()
		if parsed.User.Username() == "" || password == "" || parsed.Hostname() == "" || parsed.Port() == "" {
			return clickHouseConnection{}, fmt.Errorf("ClickHouse dependency connection is incomplete")
		}
		return clickHouseConnection{
			Host: parsed.Hostname(), Port: parsed.Port(), User: parsed.User.Username(), Password: password,
		}, nil
	}
	return clickHouseConnection{}, fmt.Errorf("SigNoz requires the clickhouse/connection dependency configuration for the container runtime")
}

func runMigrations(ctx context.Context, clickhouse clickHouseConnection) error {
	dsn := "tcp://" + url.UserPassword(clickhouse.User, clickhouse.Password).String() + "@" + net.JoinHostPort(clickhouse.Host, clickhouse.Port)
	environment := append(os.Environ(),
		"CLICKHOUSE_USER="+clickhouse.User,
		"CLICKHOUSE_PASSWORD="+clickhouse.Password,
		"SIGNOZ_OTEL_COLLECTOR_CLICKHOUSE_DSN="+dsn,
		"SIGNOZ_OTEL_COLLECTOR_CLICKHOUSE_CLUSTER=cluster",
		"SIGNOZ_OTEL_COLLECTOR_CLICKHOUSE_REPLICATION=false",
		"SIGNOZ_OTEL_COLLECTOR_TIMEOUT=10m",
	)
	commands := [][]string{{"ready"}, {"bootstrap"}, {"sync", "up"}, {"async", "up"}}
	for _, migration := range commands {
		args := []string{
			"run", "--rm", "--add-host", "host.docker.internal:host-gateway",
			"-e", "CLICKHOUSE_USER", "-e", "CLICKHOUSE_PASSWORD",
			"-e", "SIGNOZ_OTEL_COLLECTOR_CLICKHOUSE_DSN",
			"-e", "SIGNOZ_OTEL_COLLECTOR_CLICKHOUSE_CLUSTER",
			"-e", "SIGNOZ_OTEL_COLLECTOR_CLICKHOUSE_REPLICATION",
			"-e", "SIGNOZ_OTEL_COLLECTOR_TIMEOUT",
			collectorImage.FullName(), "migrate",
		}
		command := exec.CommandContext(ctx, "docker", append(args, migration...)...)
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("SigNoz migration %q failed: %w: %s", strings.Join(migration, " "), err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (s *Runtime) initQuery(ctx context.Context, clickhouse clickHouseConnection, jwtSecret string) error {
	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, signozImage, s.UniqueWithWorkspace()+"-query")
	if err != nil {
		return err
	}
	runner.WithPortMapping(ctx, s.queryPort, 8080)
	if _, err = runner.WithPersistentCacheMount(ctx, "signoz-data", "/var/lib/signoz"); err != nil {
		return err
	}
	dsn := "tcp://" + net.JoinHostPort(clickhouse.Host, clickhouse.Port) + "/?username=" + url.QueryEscape(clickhouse.User) + "&password=" + url.QueryEscape(clickhouse.Password)
	runner.WithEnvironmentVariables(ctx,
		resources.Env("CLICKHOUSE_USER", clickhouse.User),
		resources.Env("CLICKHOUSE_PASSWORD", clickhouse.Password),
		resources.Env("SIGNOZ_TELEMETRYSTORE_CLICKHOUSE_DSN", dsn),
		resources.Env("SIGNOZ_TELEMETRYSTORE_CLICKHOUSE_CLUSTER", "cluster"),
		resources.Env("SIGNOZ_TOKENIZER_JWT_SECRET", jwtSecret),
	)
	if err = runner.Init(ctx); err != nil {
		return err
	}
	s.queryRunner = runner
	return nil
}

func (s *Runtime) initCollector(ctx context.Context, clickhouse clickHouseConnection) error {
	configDir, err := os.MkdirTemp("", "codefly-signoz-config-")
	if err != nil {
		return err
	}
	collectorConfig, err := runtimeFS.ReadFile("runtime/otel-collector-config.yaml")
	if err != nil {
		return err
	}
	if err = os.Chmod(configDir, 0o755); err != nil {
		return err
	}
	baseDSN := "tcp://" + url.UserPassword(clickhouse.User, clickhouse.Password).String() + "@" + net.JoinHostPort(clickhouse.Host, clickhouse.Port)
	if err = os.WriteFile(filepath.Join(configDir, "otel-collector-config.yaml"), collectorConfig, 0o644); err != nil {
		return err
	}
	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, collectorImage, s.UniqueWithWorkspace()+"-collector")
	if err != nil {
		return err
	}
	runner.WithPortMapping(ctx, s.otlpGRPCPort, 4317)
	runner.WithPortMapping(ctx, s.otlpHTTPPort, 4318)
	runner.WithPortMapping(ctx, s.healthPort, 13133)
	runner.WithMount(configDir, "/conf")
	runner.WithEnvironmentVariables(ctx, resources.Env("CLICKHOUSE_DSN", baseDSN))
	runner.WithCommand(
		"--config=/conf/otel-collector-config.yaml",
	)
	if err = runner.Init(ctx); err != nil {
		return err
	}
	s.configDir = configDir
	s.collectorRunner = runner
	return nil
}

func (s *Runtime) addRuntimeConfigurations(ctx context.Context) error {
	endpoints := []*basev0.Endpoint{s.otlpGRPCEndpoint, s.otlpHTTPEndpoint, s.queryEndpoint, s.uiEndpoint}
	for _, runtimeContext := range []*basev0.RuntimeContext{resources.NewRuntimeContextNative(), resources.NewRuntimeContextContainer()} {
		configuration := &basev0.Configuration{Origin: s.Base.Unique(), RuntimeContext: runtimeContext}
		for _, endpoint := range endpoints {
			mapping, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, endpoint)
			if err != nil {
				return err
			}
			for _, instance := range mapping.Instances {
				if resources.RuntimeContextFromInstance(instance).Kind != runtimeContext.Kind {
					continue
				}
				value := instance.Address
				if endpoint.Name != "grpc" {
					value = "http://" + value
				}
				configuration.Infos = append(configuration.Infos, &basev0.ConfigurationInformation{
					Name:                endpoint.Name,
					ConfigurationValues: []*basev0.ConfigurationValue{{Key: "connection", Value: value}},
				})
			}
		}
		if len(configuration.Infos) > 0 {
			s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, configuration)
		}
	}
	return nil
}

func (s *Runtime) Start(ctx context.Context, _ *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if err := waitForHTTP(ctx, fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", s.queryPort)); err != nil {
		return s.Runtime.StartError(err)
	}
	if err := waitForHTTP(ctx, fmt.Sprintf("http://127.0.0.1:%d/", s.healthPort)); err != nil {
		return s.Runtime.StartError(err)
	}
	proxyTarget, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", s.queryPort))
	s.uiProxy = &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", s.uiPort),
		Handler:           httputil.NewSingleHostReverseProxy(proxyTarget),
		ReadHeaderTimeout: 5 * time.Second,
	}
	listener, err := net.Listen("tcp", s.uiProxy.Addr)
	if err != nil {
		return s.Runtime.StartError(err)
	}
	go func() { _ = s.uiProxy.Serve(listener) }()
	return s.Runtime.StartResponse()
}

func waitForHTTP(ctx context.Context, address string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for range 60 {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode < http.StatusInternalServerError {
				return nil
			}
			lastErr = fmt.Errorf("status %s", response.Status)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("service at %s did not become ready: %w", address, lastErr)
}

func (s *Runtime) Stop(context.Context, *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, _ *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if s.uiProxy != nil {
		_ = s.uiProxy.Shutdown(ctx)
	}
	for _, runner := range []*dockerrun.DockerEnvironment{s.collectorRunner, s.queryRunner} {
		if runner != nil {
			if err := runner.Shutdown(ctx); err != nil {
				return s.Runtime.DestroyError(err)
			}
		}
	}
	if s.configDir != "" {
		_ = os.RemoveAll(s.configDir)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Test(context.Context, *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

//go:embed runtime
var runtimeFS embed.FS
