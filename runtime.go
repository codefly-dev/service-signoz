package main

import (
	"context"
	"embed"
	"errors"
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

type clickHouseDependency struct {
	Native    clickHouseConnection
	Container clickHouseConnection
}

type tcpRelay struct {
	listener net.Listener
	target   string
	allowed  []*net.IPNet
}

type Runtime struct {
	services.RuntimeServer
	*Service

	queryRunner     *dockerrun.DockerEnvironment
	collectorRunner *dockerrun.DockerEnvironment
	configDir       string
	dependencyRelay *tcpRelay
	endpointRelays  []*tcpRelay

	queryPort    uint16
	uiPort       uint16
	otlpGRPCPort uint16
	otlpHTTPPort uint16

	queryBackendPort    uint16
	otlpGRPCBackendPort uint16
	otlpHTTPBackendPort uint16
	healthPort          uint16
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
	excludedPorts := map[uint16]struct{}{s.otlpGRPCPort: {}, s.otlpHTTPPort: {}, s.queryPort: {}, s.uiPort: {}}
	for _, backend := range []struct {
		port *uint16
		name string
	}{
		{port: &s.queryBackendPort, name: "query"},
		{port: &s.otlpGRPCBackendPort, name: "OTLP/gRPC"},
		{port: &s.otlpHTTPBackendPort, name: "OTLP/HTTP"},
		{port: &s.healthPort, name: "collector health"},
	} {
		if *backend.port, err = freeLocalPortExcluding(excludedPorts); err != nil {
			return s.Runtime.InitError(fmt.Errorf("allocate %s backend port: %w", backend.name, err))
		}
		excludedPorts[*backend.port] = struct{}{}
	}
	jwtSecret, err := resources.GetConfigurationValue(ctx, req.GetConfiguration(), "signoz", "SIGNOZ_TOKENIZER_JWT_SECRET")
	if err != nil || jwtSecret == "" {
		return s.Runtime.InitError(fmt.Errorf("SigNoz requires SIGNOZ_TOKENIZER_JWT_SECRET in its service configuration"))
	}

	dependency, err := parseClickHouseDependency(ctx, req.DependenciesConfigurations)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	clickhouse := dependency.Container
	if dependency.Container.Host == "host.docker.internal" {
		allowed, sourceErr := dockerSourceNetworks(ctx)
		if sourceErr != nil {
			return s.Runtime.InitError(sourceErr)
		}
		relay, relayErr := startTCPRelay(
			"0.0.0.0:0",
			net.JoinHostPort(dependency.Native.Host, dependency.Native.Port),
			allowed,
		)
		if relayErr != nil {
			return s.Runtime.InitError(fmt.Errorf("expose ClickHouse to service containers: %w", relayErr))
		}
		s.dependencyRelay = relay
		clickhouse.Port = strconv.Itoa(relay.listener.Addr().(*net.TCPAddr).Port)
	}
	if err = runMigrations(ctx, clickhouse); err != nil {
		return s.initFailure(err)
	}

	if err = s.initQuery(ctx, clickhouse, jwtSecret); err != nil {
		return s.initFailure(err)
	}
	if err = s.initCollector(ctx, clickhouse); err != nil {
		return s.initFailure(err)
	}
	if err = s.addRuntimeConfigurations(ctx); err != nil {
		return s.initFailure(err)
	}
	return s.Runtime.InitResponse()
}

func (s *Runtime) initFailure(cause error) (*runtimev0.InitResponse, error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.shutdown(cleanupCtx, false); err != nil {
		cause = errors.Join(cause, fmt.Errorf("clean up after failed SigNoz initialization: %w", err))
	}
	return s.Runtime.InitError(cause)
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
	defer func() { _ = listener.Close() }()
	return uint16(listener.Addr().(*net.TCPAddr).Port), nil
}

func freeLocalPortExcluding(excluded map[uint16]struct{}) (uint16, error) {
	for {
		port, err := freeLocalPort()
		if err != nil {
			return 0, err
		}
		if _, found := excluded[port]; !found {
			return port, nil
		}
	}
}

func parseClickHouseDependency(ctx context.Context, configurations []*basev0.Configuration) (clickHouseDependency, error) {
	native, err := clickHouseConnectionForRuntime(ctx, configurations, resources.NewRuntimeContextNative())
	if err != nil {
		return clickHouseDependency{}, err
	}
	container, err := clickHouseConnectionForRuntime(ctx, configurations, resources.NewRuntimeContextContainer())
	if err != nil {
		return clickHouseDependency{}, err
	}
	return clickHouseDependency{Native: native, Container: container}, nil
}

func clickHouseConnectionForRuntime(ctx context.Context, configurations []*basev0.Configuration, runtimeContext *basev0.RuntimeContext) (clickHouseConnection, error) {
	for _, configuration := range resources.FilterConfigurations(configurations, runtimeContext) {
		connection, err := resources.GetConfigurationValue(ctx, configuration, "clickhouse", "connection")
		if err != nil || connection == "" {
			continue
		}
		return parseClickHouseConnection(connection)
	}
	return clickHouseConnection{}, fmt.Errorf("SigNoz requires the clickhouse/connection dependency configuration for the %s runtime", runtimeContext.Kind)
}

func parseClickHouseConnection(connection string) (clickHouseConnection, error) {
	scheme, authority, found := strings.Cut(connection, "://")
	if !found || (scheme != "clickhouse" && scheme != "tcp") {
		return clickHouseConnection{}, fmt.Errorf("unsupported ClickHouse connection scheme %q", scheme)
	}
	credentials, hostPath, found := strings.Cut(authority, "@")
	if !found {
		return clickHouseConnection{}, fmt.Errorf("ClickHouse dependency connection is incomplete")
	}
	if lastAt := strings.LastIndex(authority, "@"); lastAt >= 0 {
		credentials, hostPath = authority[:lastAt], authority[lastAt+1:]
	}
	username, password, found := strings.Cut(credentials, ":")
	if !found || username == "" || password == "" {
		return clickHouseConnection{}, fmt.Errorf("ClickHouse dependency connection is incomplete")
	}
	hostPort, _, _ := strings.Cut(hostPath, "/")
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil || host == "" || port == "" {
		return clickHouseConnection{}, fmt.Errorf("ClickHouse dependency connection is incomplete")
	}
	return clickHouseConnection{Host: host, Port: port, User: decodeUserInfo(username), Password: decodeUserInfo(password)}, nil
}

func decodeUserInfo(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
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
	s.queryRunner = runner
	runner.WithPortMapping(ctx, s.queryBackendPort, 8080)
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
	return nil
}

func (s *Runtime) initCollector(ctx context.Context, clickhouse clickHouseConnection) error {
	configDir, err := os.MkdirTemp("", "codefly-signoz-config-")
	if err != nil {
		return err
	}
	s.configDir = configDir
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
	s.collectorRunner = runner
	runner.WithPortMapping(ctx, s.otlpGRPCBackendPort, 4317)
	runner.WithPortMapping(ctx, s.otlpHTTPBackendPort, 4318)
	runner.WithPortMapping(ctx, s.healthPort, 13133)
	runner.WithMount(configDir, "/conf")
	runner.WithEnvironmentVariables(ctx, resources.Env("CLICKHOUSE_DSN", baseDSN))
	runner.WithCommand(
		"--config=/conf/otel-collector-config.yaml",
	)
	if err = runner.Init(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Runtime) addRuntimeConfigurations(ctx context.Context) error {
	endpoints := []*basev0.Endpoint{s.otlpGRPCEndpoint, s.otlpHTTPEndpoint, s.queryEndpoint, s.uiEndpoint}
	for _, runtimeContext := range []*basev0.RuntimeContext{resources.NewRuntimeContextNative(), resources.NewRuntimeContextContainer()} {
		configuration := &basev0.Configuration{Origin: s.Unique(), RuntimeContext: runtimeContext}
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
	if err := waitForHTTP(ctx, fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", s.queryBackendPort)); err != nil {
		return s.Runtime.StartError(err)
	}
	if err := waitForHTTP(ctx, fmt.Sprintf("http://127.0.0.1:%d/", s.healthPort)); err != nil {
		return s.Runtime.StartError(err)
	}
	if err := s.startEndpointRelays(ctx); err != nil {
		return s.Runtime.StartError(err)
	}
	return s.Runtime.StartResponse()
}

func (s *Runtime) startEndpointRelays(ctx context.Context) error {
	if len(s.endpointRelays) > 0 {
		return nil
	}
	allowed, err := dockerSourceNetworks(ctx)
	if err != nil {
		return err
	}
	for _, endpoint := range []struct {
		port   uint16
		target uint16
	}{
		{port: s.otlpGRPCPort, target: s.otlpGRPCBackendPort},
		{port: s.otlpHTTPPort, target: s.otlpHTTPBackendPort},
		{port: s.queryPort, target: s.queryBackendPort},
		{port: s.uiPort, target: s.queryBackendPort},
	} {
		relay, relayErr := startTCPRelay(
			net.JoinHostPort("0.0.0.0", strconv.Itoa(int(endpoint.port))),
			net.JoinHostPort("127.0.0.1", strconv.Itoa(int(endpoint.target))),
			allowed,
		)
		if relayErr != nil {
			_ = closeTCPRelays(s.endpointRelays)
			s.endpointRelays = nil
			return fmt.Errorf("expose SigNoz endpoint %d to module containers: %w", endpoint.port, relayErr)
		}
		s.endpointRelays = append(s.endpointRelays, relay)
	}
	return nil
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
	if err := s.shutdown(ctx, true); err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) shutdown(ctx context.Context, discoverRunners bool) error {
	var errs []error
	if err := closeTCPRelays(s.endpointRelays); err != nil {
		errs = append(errs, fmt.Errorf("shut down endpoint relays: %w", err))
	}
	s.endpointRelays = nil
	for _, runner := range []struct {
		name    string
		image   *resources.DockerImage
		current *dockerrun.DockerEnvironment
	}{
		{name: "collector", image: collectorImage, current: s.collectorRunner},
		{name: "query", image: signozImage, current: s.queryRunner},
	} {
		current := runner.current
		if current == nil && discoverRunners {
			var err error
			current, err = dockerrun.NewDockerHeadlessEnvironment(ctx, runner.image, s.UniqueWithWorkspace()+"-"+runner.name)
			if err != nil {
				errs = append(errs, fmt.Errorf("find %s container: %w", runner.name, err))
				continue
			}
		}
		if current != nil {
			if err := current.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("shut down %s container: %w", runner.name, err))
			}
		}
	}
	s.collectorRunner = nil
	s.queryRunner = nil
	if s.configDir != "" {
		if err := os.RemoveAll(s.configDir); err != nil {
			errs = append(errs, fmt.Errorf("remove collector configuration: %w", err))
		}
		s.configDir = ""
	}
	if s.dependencyRelay != nil {
		if err := s.dependencyRelay.Close(); err != nil {
			errs = append(errs, fmt.Errorf("shut down ClickHouse relay: %w", err))
		}
		s.dependencyRelay = nil
	}
	return errors.Join(errs...)
}

func dockerSourceNetworks(ctx context.Context) ([]*net.IPNet, error) {
	// Relays listen on all host interfaces because Docker Desktop cannot route
	// containers to host loopback; accepting only loopback and bridge sources
	// keeps the service unavailable to other hosts.
	command := exec.CommandContext(ctx, "docker", "network", "inspect", "bridge", "--format", "{{range .IPAM.Config}}{{println .Subnet}}{{end}}")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("inspect Docker bridge network: %w: %s", err, strings.TrimSpace(string(output)))
	}
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	networks := []*net.IPNet{loopback}
	for _, subnet := range strings.Fields(string(output)) {
		_, network, parseErr := net.ParseCIDR(subnet)
		if parseErr != nil {
			return nil, fmt.Errorf("docker bridge returned invalid subnet %q", subnet)
		}
		networks = append(networks, network)
	}
	if len(networks) == 1 {
		return nil, fmt.Errorf("docker bridge returned no source subnets")
	}
	return networks, nil
}

func startTCPRelay(listenAddress, target string, allowed []*net.IPNet) (*tcpRelay, error) {
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return nil, err
	}
	relay := &tcpRelay{listener: listener, target: target, allowed: allowed}
	go relay.serve()
	return relay, nil
}

func (relay *tcpRelay) serve() {
	for {
		connection, err := relay.listener.Accept()
		if err != nil {
			return
		}
		if !relay.allows(connection.RemoteAddr()) {
			_ = connection.Close()
			continue
		}
		go relay.forward(connection)
	}
}

func (relay *tcpRelay) allows(address net.Addr) bool {
	tcpAddress, ok := address.(*net.TCPAddr)
	if !ok {
		return false
	}
	for _, network := range relay.allowed {
		if network.Contains(tcpAddress.IP) {
			return true
		}
	}
	return false
}

func (relay *tcpRelay) forward(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	upstream, err := net.Dial("tcp", relay.target)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()

	done := make(chan struct{}, 2)
	copyConnection := func(destination io.Writer, source io.Reader) {
		_, _ = io.Copy(destination, source)
		done <- struct{}{}
	}
	go copyConnection(upstream, connection)
	go copyConnection(connection, upstream)
	<-done
}

func (relay *tcpRelay) Close() error {
	if err := relay.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func closeTCPRelays(relays []*tcpRelay) error {
	var errs []error
	for _, relay := range relays {
		if relay != nil {
			errs = append(errs, relay.Close())
		}
	}
	return errors.Join(errs...)
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Test(context.Context, *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

//go:embed runtime
var runtimeFS embed.FS
