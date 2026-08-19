# service-signoz

Codefly service agent for a self-hosted SigNoz telemetry plane. It renders and runs:

- SigNoz query and UI `0.137.0`
- SigNoz OTLP collector and telemetry-store migrator `0.144.8`
- a declared `codefly.dev/clickhouse:0.0.9` dependency using ClickHouse `25.12.5`

The ClickHouse companion image adds the single-node Keeper and cluster definition required by SigNoz's upstream migrations. It remains a `service-clickhouse` image override, so ClickHouse persistence and credentials stay at the service dependency boundary.

## Endpoint policy

`grpc` (OTLP/gRPC) and `http` (OTLP/HTTP) are private. `query` and `web` (UI) are module-visible. The generated Kubernetes Services are `ClusterIP`; no ingress, load balancer, or node port is emitted.

## Secrets

Kubernetes renders require external references for `CLICKHOUSE_DSN` and `SIGNOZ_TOKENIZER_JWT_SECRET`. The DSN key contains the dedicated credentials in `tcp://user:password@clickhouse:9000` form; the ClickHouse service references the corresponding username and password keys independently. No profile emits a Kubernetes Secret or a literal credential.

## Development

```sh
go test ./...
go vet ./...
```

The Docker integration test runs when `CODEFLY_SIGNOZ_INTEGRATION=1`. It starts the pinned ClickHouse image, performs the upstream migrations, exports a controlled OTLP trace, queries it, restarts the SigNoz workloads, and confirms the trace remains.
