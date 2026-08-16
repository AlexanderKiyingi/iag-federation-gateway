# platform-go

Shared Go runtime for IAG services. Consumed by every Go service in this programme
(authentication, notifications, accounts, finance, fleet, procurement,
project-management, supply-chain).

Replaces:

- per-service Kafka consumer/producer scaffolding
- per-service JWKS verifier
- per-service bearer-auth middleware
- per-service slog/OTel/request-id wiring
- the static `GATEWAY_INTERNAL_SECRET` trust pattern

## Packages

| Package        | What it provides                                                                   |
| -------------- | ---------------------------------------------------------------------------------- |
| `authclient`   | JWKS verifier with `aud` enforcement, multi-`kid` rotation, periodic refresh       |
| `serviceauth`  | OAuth2 `client_credentials` token cache for outbound service-to-service calls      |
| `middleware`   | Gin bearer-auth, principal context, request-id, audience guards                    |
| `events`       | Kafka consumer with dedupe + DLQ, producer with idempotent writes                  |
| `otel`         | OTLP tracer setup, Gin middleware integration                                      |

## Usage outline

```go
// main.go
tp := otel.MustInit(ctx, otel.Config{ServiceName: "notifications"})
defer tp.Shutdown(ctx)

verifier := authclient.NewVerifier(authclient.Options{
    JWKSURL:  cfg.JWKSURL,
    Issuer:   cfg.JWTIssuer,
    Audience: "iag.notifications",  // services REJECT tokens lacking this aud
})
_ = verifier.Refresh(ctx)
verifier.StartRefreshLoop(ctx, 15*time.Minute)

svcAuth := serviceauth.NewClient(serviceauth.Options{
    TokenURL:     cfg.AuthTokenURL,
    ClientID:     cfg.ServiceClientID,
    ClientSecret: cfg.ServiceClientSecret,
    Audience:     "iag.finance",  // when calling finance
})

consumer, _ := events.NewConsumer(events.ConsumerConfig{
    Brokers:     cfg.KafkaBrokers,
    Topic:       "iag.notifications",
    GroupID:     "iag.notifications.dispatch",
    DLQTopic:    "iag.notifications.dlq",
    Dedupe:      events.PostgresDedupe(pool, "notifications.processed_events"),
    Handler:     dispatchSvc,
})
```

## Trust model (post-cutover)

There is exactly **one** way to authenticate a request to any IAG service:
an RS256 Bearer token issued by the authentication service, with:

- `iss = https://auth.iag.local` (env-driven)
- `aud = iag.<service>` — every service rejects tokens lacking its audience
- `principal_type ∈ {user, service}` — services use `client_credentials`, users use `password` or `refresh_token`
- `sub = <uuid>` for users, `sub = client:<client_id>` for services
- signed with `kid` present in the live JWKS (which may carry current + next key)

`GATEWAY_INTERNAL_SECRET` and `X-IAG-*` trust headers are removed. The gateway
forwards the original `Authorization` header verbatim; backends verify it.
