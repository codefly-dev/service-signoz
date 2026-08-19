package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestParseClickHouseDependency(t *testing.T) {
	connection, err := parseClickHouseDependency(context.Background(), clickHouseTestConfigurations("secret"))
	require.NoError(t, err)
	require.Equal(t, clickHouseDependency{
		Native:    clickHouseConnection{Host: "127.0.0.1", Port: "39000", User: "signoz", Password: "secret"},
		Container: clickHouseConnection{Host: "host.docker.internal", Port: "39000", User: "signoz", Password: "secret"},
	}, connection)
}

func TestParseClickHouseDependencyRejectsMissingCredentials(t *testing.T) {
	_, err := parseClickHouseConnection("clickhouse://host.docker.internal:39000/signoz")
	require.EqualError(t, err, "ClickHouse dependency connection is incomplete")
}

func TestParseClickHouseConnectionAcceptsRawAndEscapedCredentials(t *testing.T) {
	for _, test := range []struct {
		name       string
		connection string
		password   string
	}{
		{name: "raw reserved characters", connection: "clickhouse://signoz:pa/ss?#%@word@host.docker.internal:39000/signoz", password: "pa/ss?#%@word"},
		{name: "escaped user info", connection: "clickhouse://signoz:pa%2Fss%3F%23%25%40word@host.docker.internal:39000/signoz", password: "pa/ss?#%@word"},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection, err := parseClickHouseConnection(test.connection)
			require.NoError(t, err)
			require.Equal(t, test.password, connection.Password)
			require.Equal(t, "host.docker.internal", connection.Host)
			require.Equal(t, "39000", connection.Port)
		})
	}
}

func TestTCPRelayForwardsConnections(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Close() })
	go func() {
		connection, acceptErr := upstream.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = io.Copy(connection, connection)
	}()

	_, loopback, err := net.ParseCIDR("127.0.0.0/8")
	require.NoError(t, err)
	relay, err := startTCPRelay("127.0.0.1:0", upstream.Addr().String(), []*net.IPNet{loopback})
	require.NoError(t, err)
	t.Cleanup(func() { _ = relay.Close() })
	connection, err := net.Dial("tcp", relay.listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	require.NoError(t, connection.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = connection.Write([]byte("trace"))
	require.NoError(t, err)
	response := make([]byte, len("trace"))
	_, err = io.ReadFull(connection, response)
	require.NoError(t, err)
	require.Equal(t, "trace", string(response))
}

func clickHouseTestConfigurations(password string) []*basev0.Configuration {
	configuration := func(runtimeContext *basev0.RuntimeContext, host string) *basev0.Configuration {
		return &basev0.Configuration{
			RuntimeContext: runtimeContext,
			Infos: []*basev0.ConfigurationInformation{{
				Name: "clickhouse",
				ConfigurationValues: []*basev0.ConfigurationValue{{
					Key: "connection", Value: "clickhouse://signoz:" + password + "@" + host + ":39000/signoz", Secret: true,
				}},
			}},
		}
	}
	return []*basev0.Configuration{
		configuration(resources.NewRuntimeContextNative(), "127.0.0.1"),
		configuration(resources.NewRuntimeContextContainer(), "host.docker.internal"),
	}
}

func TestCompanionClickHouseConfiguration(t *testing.T) {
	collectorConfig, err := runtimeFS.ReadFile("runtime/otel-collector-config.yaml")
	require.NoError(t, err)
	require.Contains(t, string(collectorConfig), "clickhousetraces:")
	require.Contains(t, string(collectorConfig), "signozspanmetrics/delta")
	require.Contains(t, string(collectorConfig), "${env:CLICKHOUSE_DSN}")
	require.NotContains(t, string(collectorConfig), "tcp://")

	config, err := os.ReadFile("clickhouse/config.d/signoz-cluster.xml")
	require.NoError(t, err)
	require.Contains(t, string(config), "<keeper_server>")
	require.Contains(t, string(config), "<cluster>")
	require.Contains(t, string(config), "from_env=\"CLICKHOUSE_PASSWORD\"")
}

func TestRuntimeShutdownRemovesCollectorConfiguration(t *testing.T) {
	runtime := NewRuntime()
	configDir := filepath.Join(t.TempDir(), "collector-config")
	runtime.configDir = configDir
	require.NoError(t, os.Mkdir(configDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("test"), 0o600))

	require.NoError(t, runtime.shutdown(context.Background(), false))
	_, err := os.Stat(configDir)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Empty(t, runtime.configDir)
	require.NoError(t, runtime.shutdown(context.Background(), false))
}
